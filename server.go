package courier

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"sync"

	"github.com/nyaruka/courier/config"
	"github.com/nyaruka/courier/utils"
	"github.com/pressly/chi"
	"github.com/pressly/chi/middleware"
	"github.com/pressly/lg"
	"github.com/sirupsen/logrus"
)

// Server is the main interface ChannelHandlers use to interact with the database and redis. It provides an
// abstraction that makes mocking easier for isolated unit tests
type Server interface {
	Config() *config.Courier
	AddChannelRoute(handler ChannelHandler, method string, action string, handlerFunc ChannelActionHandlerFunc) error

	GetChannel(ChannelType, ChannelUUID) (Channel, error)
	WriteMsg(*Msg) error
	WriteMsgStatus(*MsgStatusUpdate) error

	WaitGroup() *sync.WaitGroup
	StopChan() chan bool
	Stopped() bool

	Router() chi.Router

	Start() error
	Stop() error
}

// NewServer creates a new Server for the passed in configuration. The server will have to be started
// afterwards, which is when configuration options are checked.
func NewServer(config *config.Courier, backend Backend) Server {
	// create our top level router
	logger := logrus.New()
	lg.RedirectStdlogOutput(logger)
	lg.DefaultLogger = logger

	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(lg.RequestLogger(logger))
	router.Use(middleware.Recoverer)

	chanRouter := chi.NewRouter()
	router.Mount("/c/", chanRouter)

	return &server{
		config:  config,
		backend: backend,

		router:     router,
		chanRouter: chanRouter,

		stopChan:  make(chan bool),
		waitGroup: &sync.WaitGroup{},
		stopped:   false,
	}
}

// Start starts the Server listening for incoming requests and sending messages. It will return an error
// if it encounters any unrecoverable (or ignorable) error, though its bias is to move forward despite
// connection errors
func (s *server) Start() error {
	// start our backend
	err := s.backend.Start()
	if err != nil {
		return err
	}

	// start our spool flushers
	startSpoolFlushers(s)

	// wire up our index page
	s.router.Get("/", s.handleIndex)

	// initialize our handlers
	s.initializeChannelHandlers()

	// configure timeouts on our server
	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.config.Port),
		Handler:      s.router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	// and start serving HTTP
	go func() {
		s.waitGroup.Add(1)
		defer s.waitGroup.Done()
		err := s.httpServer.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			logrus.WithFields(logrus.Fields{
				"comp":  "server",
				"state": "stopping",
				"err":   err,
			}).Error()
		}
	}()

	logrus.WithFields(logrus.Fields{
		"comp":    "server",
		"port":    s.config.Port,
		"state":   "started",
		"version": s.config.Version,
	}).Info("server listening on ", s.config.Port)
	return nil
}

// Stop stops the server, returning only after all threads have stopped
func (s *server) Stop() error {
	logrus.WithFields(logrus.Fields{
		"comp":  "server",
		"state": "stopping",
	}).Info("stopping server")

	err := s.backend.Stop()
	if err != nil {
		return err
	}

	s.stopped = true
	close(s.stopChan)

	// shut down our HTTP server
	if err := s.httpServer.Shutdown(nil); err != nil {
		logrus.WithFields(logrus.Fields{
			"comp": "server",
			"err":  err,
		}).Error("shutting down server")
	}

	s.waitGroup.Wait()

	logrus.WithFields(logrus.Fields{
		"comp":  "server",
		"state": "stopped",
	}).Info("server stopped")

	return nil
}

func (s *server) GetChannel(cType ChannelType, cUUID ChannelUUID) (Channel, error) {
	return s.backend.GetChannel(cType, cUUID)
}

func (s *server) WriteMsg(msg *Msg) error {
	return s.backend.WriteMsg(msg)
}

func (s *server) WriteMsgStatus(status *MsgStatusUpdate) error {
	return s.backend.WriteMsgStatus(status)
}

func (s *server) WaitGroup() *sync.WaitGroup { return s.waitGroup }
func (s *server) StopChan() chan bool        { return s.stopChan }
func (s *server) Config() *config.Courier    { return s.config }
func (s *server) Stopped() bool              { return s.stopped }

func (s *server) Backend() Backend   { return s.backend }
func (s *server) Router() chi.Router { return s.router }

type server struct {
	backend Backend

	httpServer *http.Server
	router     *chi.Mux
	chanRouter *chi.Mux

	config *config.Courier

	waitGroup *sync.WaitGroup
	stopChan  chan bool
	stopped   bool

	routes []string
}

func (s *server) initializeChannelHandlers() {
	includes := s.config.IncludeChannels
	excludes := s.config.ExcludeChannels

	// initialize handlers which are included/not-excluded in the config
	for _, handler := range registeredHandlers {
		channelType := string(handler.ChannelType())
		if (includes == nil || utils.StringArrayContains(includes, channelType)) && (excludes == nil || !utils.StringArrayContains(excludes, channelType)) {
			err := handler.Initialize(s)
			if err != nil {
				log.Fatal(err)
			}
			activeHandlers[handler.ChannelType()] = handler

			logrus.WithField("comp", "server").WithField("handler", handler.ChannelName()).WithField("handler_type", channelType).Info("handler initialized")
		}
	}

	// sort our route help
	sort.Strings(s.routes)
}

func (s *server) channelFunctionWrapper(handler ChannelHandler, handlerFunc ChannelActionHandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uuid, err := NewChannelUUID(chi.URLParam(r, "uuid"))

		if err != nil {
			WriteError(w, r, err)
			return
		}

		channel, err := s.backend.GetChannel(handler.ChannelType(), uuid)
		if err != nil {
			WriteError(w, r, err)
			return
		}

		err = handlerFunc(channel, w, r)
		if err != nil {
			WriteError(w, r, err)
		}
	}
}

func (s *server) AddChannelRoute(handler ChannelHandler, method string, action string, handlerFunc ChannelActionHandlerFunc) error {
	method = strings.ToLower(method)
	channelType := strings.ToLower(string(handler.ChannelType()))

	path := fmt.Sprintf("/%s/:uuid/%s/", channelType, action)
	if method == "get" {
		s.chanRouter.Get(path, s.channelFunctionWrapper(handler, handlerFunc))
	} else if method == "post" {
		s.chanRouter.Post(path, s.channelFunctionWrapper(handler, handlerFunc))
	} else {
		return fmt.Errorf("unsupported method: %s", method)
	}
	s.routes = append(s.routes, fmt.Sprintf("%-20s - %s %s", "/c"+path, handler.ChannelName(), action))
	return nil
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {

	var buf bytes.Buffer
	buf.WriteString("<title>courier</title><body><pre>\n")
	buf.WriteString(splash)
	buf.WriteString(s.config.Version)

	buf.WriteString(s.backend.Health())

	buf.WriteString("\n\n")
	buf.WriteString(strings.Join(s.routes, "\n"))
	buf.WriteString("</pre></body>")
	w.Write(buf.Bytes())
}

var splash = `
 ____________                   _____             
   ___  ____/_________  ___________(_)____________
    _  /  __  __ \  / / /_  ___/_  /_  _ \_  ___/
    / /__  / /_/ / /_/ /_  /   _  / /  __/  /    
    \____/ \____/\__,_/ /_/    /_/  \___//_/ v`