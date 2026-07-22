# syntax=docker/dockerfile:1.7
#
# mygnoscan — single static Go binary (pure-Go SQLite, embedded frontend).
#
# Local build:
#   docker build \
#     --build-arg COMMIT=$(git rev-parse --short HEAD) \
#     --build-arg BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
#     -t mygnoscan:dev .

# ---- Build
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG COMMIT=dev
ARG BUILD_TIME=unknown
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build \
  -trimpath \
  -ldflags "-s -w -X main.gitHash=$COMMIT -X main.buildTime=$BUILD_TIME" \
  -o /out/mygnoscan .

# ---- Runtime
FROM alpine:3.24
RUN apk add --no-cache ca-certificates wget && \
  addgroup -S -g 10001 app && \
  adduser -S -u 10001 -G app app && \
  mkdir -p /var/lib/mygnoscan && chown -R app:app /var/lib/mygnoscan
COPY --from=build /out/mygnoscan /usr/local/bin/mygnoscan
ARG COMMIT=dev
LABEL org.opencontainers.image.title="mygnoscan" \
  org.opencontainers.image.description="Gno.land explorer — syncs tx-indexer data into SQLite and serves an embedded web UI." \
  org.opencontainers.image.source="https://github.com/gnoverse/mygnoscan" \
  org.opencontainers.image.revision="$COMMIT"
EXPOSE 8888
USER app
# HEALTHCHECK assumes the default -listen :8888; a deployment that overrides
# -listen must override the healthcheck as well.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8888/api/version || exit 1
ENTRYPOINT ["/usr/local/bin/mygnoscan"]
# Deployments append/override flags here (e.g. -config /etc/mygnoscan/networks.json).
CMD ["-db", "/var/lib/mygnoscan/mygnoscan.db"]
