# syntax=docker/dockerfile:1.7
#
# Multi-stage build for recon. One builder produces both binaries; two
# runtime targets (`hub-runtime`, `agent-runtime`) wrap them with a slim
# alpine base. docker-compose picks the target per service via `target:`.

ARG GO_VERSION=1.25

# ── builder ──────────────────────────────────────────────────────────────────
FROM golang:${GO_VERSION}-alpine AS build
# git is required when GOPROXY=direct fetches modules straight from VCS hosts.
RUN apk add --no-cache git
WORKDIR /src

# GOPROXY=direct sidesteps proxy.golang.org's TLS handshake bug that surfaces
# from some build environments — fetches go directly from the upstream VCS hosts.
ENV GOPROXY=direct GOSUMDB=off

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG VERSION=docker
ARG COMMIT=unknown
ARG LDFLAGS="-s -w \
  -X 'github.com/vasyakrg/recon/internal/common/version.Version=${VERSION}' \
  -X 'github.com/vasyakrg/recon/internal/common/version.Commit=${COMMIT}'"

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "${LDFLAGS}" -o /out/recon-hub   ./cmd/hub && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "${LDFLAGS}" -o /out/recon-agent ./cmd/agent

# ── shared runtime base ──────────────────────────────────────────────────────
FROM alpine:3.20 AS runtime-base
# ca-certificates: outbound HTTPS to LLM provider / module proxies.
# tzdata:         sensible local times in logs when TZ= is set.
# wget:           healthcheck.
RUN apk add --no-cache ca-certificates tzdata wget

# ── recon-hub runtime ────────────────────────────────────────────────────────
FROM runtime-base AS hub-runtime
RUN addgroup -S -g 1000 recon \
 && adduser -S -u 1000 -G recon -h /var/lib/recon recon \
 && mkdir -p /var/lib/recon /etc/recon \
 && chown -R recon:recon /var/lib/recon

COPY --from=build /out/recon-hub /usr/local/bin/recon-hub

VOLUME ["/var/lib/recon"]
EXPOSE 8080 9443

USER recon:recon
WORKDIR /var/lib/recon

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/recon-hub"]
CMD ["--config", "/etc/recon/hub.yaml", "--mode", "serve"]

# ── recon-agent runtime ──────────────────────────────────────────────────────
FROM runtime-base AS agent-runtime
RUN addgroup -S -g 1000 recon \
 && adduser -S -u 1000 -G recon -h /var/lib/recon-agent recon \
 && mkdir -p /var/lib/recon-agent /etc/recon \
 && chown -R recon:recon /var/lib/recon-agent

COPY --from=build /out/recon-agent /usr/local/bin/recon-agent

VOLUME ["/var/lib/recon-agent"]

USER recon:recon
WORKDIR /var/lib/recon-agent

ENTRYPOINT ["/usr/local/bin/recon-agent"]
CMD ["--config", "/etc/recon/agent.yaml"]
