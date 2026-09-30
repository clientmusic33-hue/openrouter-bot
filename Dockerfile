# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM golang:1.25 AS build

WORKDIR /src

# Download dependencies first so they stay cached when only source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/openrouter-bot .

# ---------------------------------------------------------------------------
# Final stage
# ---------------------------------------------------------------------------
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /openrouter-bot

# Copy config, translations and the binary.
COPY --from=build /out/openrouter-bot ./
COPY config.yaml ./
COPY lang/ ./lang/

# Run as an unprivileged user.
RUN addgroup -S bot && adduser -S -G bot bot \
    && mkdir -p logs data \
    && chown -R bot:bot /openrouter-bot

USER bot

EXPOSE 10000
VOLUME ["/openrouter-bot/data", "/openrouter-bot/logs"]

# /etc/secrets/.env is optional: when it is absent the configuration is read
# from real environment variables instead. The previous entrypoint used
# `cp ... && exec ...`, which exited the container whenever the file was
# missing - including with the docker-compose file in this repository.
ENTRYPOINT ["/bin/sh", "-c", "[ -f /etc/secrets/.env ] && cp /etc/secrets/.env /openrouter-bot/.env; exec /openrouter-bot/openrouter-bot"]
