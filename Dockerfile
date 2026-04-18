# syntax=docker/dockerfile:1.7
#
# Multi-stage build for recon-hub.
# Layer 1: golang-alpine builds a static binary (CGO_ENABLED=0 — modernc.org/sqlite is pure Go).
# Layer 2: alpine runtime — tiny, lets the operator `docker exec` for debugging.

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build
# git is required when GOPROXY=direct fetches modules straight from VCS hosts.
RUN apk add --no-cache git
WORKDIR /src

# Cache modules in their own layer. GOPROXY=direct sidesteps proxy.golang.org's
# TLS handshake bug that surfaces from some build environments — fetches go
# directly from the upstream VCS hosts instead.
ENV GOPROXY=direct GOSUMDB=off
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=docker
ARG COMMIT=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w \
        -X 'github.com/vasyakrg/recon/internal/common/version.Version=${VERSION}' \
        -X 'github.com/vasyakrg/recon/internal/common/version.Commit=${COMMIT}'" \
      -o /out/recon-hub ./cmd/hub

# ── runtime ──────────────────────────────────────────────────────────────────
FROM alpine:3.20

# ca-certificates needed for outbound HTTPS to the LLM provider (OpenRouter,
# OpenAI, etc). tzdata so logs print sensible local times when the operator
# sets TZ=. wget for the HEALTHCHECK.
RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -S -g 1000 recon \
 && adduser -S -u 1000 -G recon -h /var/lib/recon recon \
 && mkdir -p /var/lib/recon /etc/recon \
 && chown -R recon:recon /var/lib/recon

COPY --from=build /out/recon-hub /usr/local/bin/recon-hub

# State volume — db, artifacts, generated CA. Mount with a named volume in
# docker-compose so it survives container recreation.
VOLUME ["/var/lib/recon"]

# 8080 = HTTP/UI. 9443 = gRPC for agents (mTLS).
EXPOSE 8080 9443

USER recon:recon
WORKDIR /var/lib/recon

# Probe the http handler — agents poll /healthz and so do we.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/recon-hub"]
CMD ["--config", "/etc/recon/hub.yaml", "--mode", "serve"]
