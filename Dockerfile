# The bridge: pulls Google Chat events out of Pub/Sub and drives a Pi session over the gateway's HTTP API.
#
# Static Go binary, no runtime dependencies, one port for health checks. Alpine rather than scratch on
# purpose: `kubectl exec` into the pod to read the SA key path, curl /readyz, or check DNS is a session
# you will want at 2am when the bot has gone quiet.
FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /pi-bot-bridge ./cmd/pi-bot

FROM alpine:latest

# TLS to chat.googleapis.com and pubsub.googleapis.com, and ca-certs is the only thing the binary needs
# beyond itself. Alpine rather than scratch so `kubectl exec` into a quiet bot is a debugging session you
# will want at 2am.
RUN apk add --no-cache ca-certificates

COPY --from=build /pi-bot-bridge /usr/local/bin/pi-bot-bridge

ENV PI_CHAT_HTTP_ADDR=:8091
EXPOSE 8091
# Runs as root so a mounted service-account credential with mode 0600 is readable by the process.
ENTRYPOINT ["/usr/local/bin/pi-bot-bridge"]
