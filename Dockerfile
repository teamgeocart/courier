FROM golang:1.13

WORKDIR /app

COPY . .

RUN mkdir -p /var/spool/courier
RUN go install -v ./cmd/...

EXPOSE 8080
ENTRYPOINT ["courier"]