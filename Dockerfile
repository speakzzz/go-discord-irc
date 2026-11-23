# Build stage
ARG GOLANG_VERSION=1.26
FROM golang:${GOLANG_VERSION}-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /go-discord-irc .

# Run stage: just the binary, CA certificates and the config
FROM alpine:3

RUN apk add --no-cache ca-certificates \
 && adduser -D -H bridge
WORKDIR /bot
COPY --from=build /go-discord-irc /bot/go-discord-irc
# Baked-in config (override at runtime with -v /path/config.yml:/bot/config.yml:ro)
COPY config.yml /bot/config.yml

USER bridge
CMD ["./go-discord-irc", "--config", "config.yml"]
