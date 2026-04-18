SHELL := /bin/bash
.DEFAULT_GOAL := help

GO        ?= go
GOBIN     ?= $(shell go env GOPATH)/bin
PROTOC    ?= protoc

BIN_DIR   := bin
HUB_BIN   := $(BIN_DIR)/recon-hub
AGENT_BIN := $(BIN_DIR)/recon-agent

PROTO_DIR := internal/proto
PROTO_SRC := $(PROTO_DIR)/recon.proto

LDFLAGS := -s -w -X 'github.com/vasyakrg/recon/internal/common/version.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo dev)'

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-18s %s\n", $$1, $$2}'

.PHONY: tools
tools: ## Install Go-managed dev tools (protoc plugins)
	$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

.PHONY: proto
proto: ## Generate gRPC + protobuf Go code
	@which protoc >/dev/null || (echo "protoc not installed (brew install protobuf)" && exit 1)
	PATH="$(GOBIN):$$PATH" $(PROTOC) \
		--go_out=. --go_opt=module=github.com/vasyakrg/recon \
		--go-grpc_out=. --go-grpc_opt=module=github.com/vasyakrg/recon \
		$(PROTO_SRC)

.PHONY: build
build: build-hub build-agent ## Build hub and agent binaries

.PHONY: build-hub
build-hub:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(HUB_BIN) ./cmd/hub

.PHONY: build-agent
build-agent:
	CGO_ENABLED=0 $(GO) build -ldflags "$(LDFLAGS)" -o $(AGENT_BIN) ./cmd/agent

.PHONY: test
test: ## Run unit tests
	$(GO) test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Run integration tests (testcontainers)
	$(GO) test -race -count=1 -tags=integration -timeout=10m ./...

.PHONY: lint
lint: ## Run golangci-lint
	@which golangci-lint >/dev/null || (echo "golangci-lint not installed (brew install golangci-lint)" && exit 1)
	golangci-lint run ./...

.PHONY: run-hub
run-hub: build-hub ## Run hub locally
	./$(HUB_BIN) --config ./deploy/dev/hub.yaml

.PHONY: run-agent
run-agent: build-agent ## Run agent locally
	./$(AGENT_BIN) --config ./deploy/dev/agent.yaml

.PHONY: dist
dist: dist-hub dist-agent ## Build static dist tarballs (linux/amd64 + arm64)

DIST_DIR := dist
DIST_VER := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: dist-hub
# Tarball is named WITHOUT the version (recon-hub-linux-<arch>.tar.gz).
# The release tag pins the version on the GitHub side, which lets the
# install script use stable URLs:
#   https://github.com/.../releases/latest/download/recon-hub-linux-amd64.tar.gz
#   https://github.com/.../releases/download/v0.1.0/recon-hub-linux-amd64.tar.gz
# DIST_VER is still stamped into the binary's --version output via LDFLAGS.
dist-hub:
	@mkdir -p $(DIST_DIR)
	@for arch in amd64 arm64; do \
		echo "==> recon-hub linux/$$arch"; \
		mkdir -p $(DIST_DIR)/recon-hub-linux-$$arch/{bin,deploy/systemd,deploy/nginx,deploy/docs}; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch \
		  $(GO) build -ldflags "$(LDFLAGS)" \
		  -o $(DIST_DIR)/recon-hub-linux-$$arch/bin/recon-hub ./cmd/hub; \
		cp deploy/systemd/recon-hub.service $(DIST_DIR)/recon-hub-linux-$$arch/deploy/systemd/; \
		cp deploy/nginx/recon.conf          $(DIST_DIR)/recon-hub-linux-$$arch/deploy/nginx/; \
		cp deploy/docs/install.md           $(DIST_DIR)/recon-hub-linux-$$arch/deploy/docs/; \
		tar czf $(DIST_DIR)/recon-hub-linux-$$arch.tar.gz \
		  -C $(DIST_DIR) recon-hub-linux-$$arch; \
		rm -rf $(DIST_DIR)/recon-hub-linux-$$arch; \
	done

.PHONY: dist-agent
dist-agent:
	@mkdir -p $(DIST_DIR)
	@for arch in amd64 arm64; do \
		echo "==> recon-agent linux/$$arch"; \
		mkdir -p $(DIST_DIR)/recon-agent-linux-$$arch/{bin,deploy/systemd,deploy/sudoers}; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch \
		  $(GO) build -ldflags "$(LDFLAGS)" \
		  -o $(DIST_DIR)/recon-agent-linux-$$arch/bin/recon-agent ./cmd/agent; \
		cp deploy/systemd/recon-agent.service $(DIST_DIR)/recon-agent-linux-$$arch/deploy/systemd/; \
		cp deploy/sudoers/recon               $(DIST_DIR)/recon-agent-linux-$$arch/deploy/sudoers/; \
		tar czf $(DIST_DIR)/recon-agent-linux-$$arch.tar.gz \
		  -C $(DIST_DIR) recon-agent-linux-$$arch; \
		rm -rf $(DIST_DIR)/recon-agent-linux-$$arch; \
	done

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)

# ── docker compose ───────────────────────────────────────────────────────────
COMPOSE ?= docker compose

.PHONY: docker-build
docker-build: ## Build the recon-hub docker image
	$(COMPOSE) build hub

.PHONY: compose-up
compose-up: ## Start the hub stack in the background
	@test -f .env || (echo "missing .env — copy from .env.example and fill in" && exit 1)
	$(COMPOSE) up -d hub

.PHONY: compose-down
compose-down: ## Stop the stack (state volume is preserved)
	$(COMPOSE) down

.PHONY: compose-logs
compose-logs: ## Tail hub logs
	$(COMPOSE) logs -f hub

.PHONY: compose-gen-hash
compose-gen-hash: ## Generate a bcrypt hash via the hub container; PASSWORD=… required
	@test -n "$(PASSWORD)" || (echo "set PASSWORD=…" && exit 1)
	$(COMPOSE) run --rm --no-deps \
	  -e RECON_ADMIN_PASSWORD='$(PASSWORD)' \
	  --entrypoint /usr/local/bin/recon-hub hub --mode gen-password-hash

.PHONY: compose-gen-token
compose-gen-token: ## Issue a bootstrap token; AGENT_ID=… required, TTL=24h optional
	@test -n "$(AGENT_ID)" || (echo "set AGENT_ID=…" && exit 1)
	$(COMPOSE) exec hub /usr/local/bin/recon-hub \
	  --config /etc/recon/hub.yaml --mode gen-token \
	  --agent-id "$(AGENT_ID)" --token-ttl "$${TTL:-24h}"

.PHONY: compose-rotate-ca
compose-rotate-ca: ## Regenerate the bootstrap CA so SAN changes in hub.yaml take effect (invalidates ALL enrolled agents)
	@echo "this wipes /var/lib/recon/ca/ and EVERY agent's enrolled identity"
	@read -p "type 'yes' to continue: " ok && test "$$ok" = "yes"
	@$(COMPOSE) --profile with-agent stop agent >/dev/null 2>&1 || true
	@$(COMPOSE) stop hub >/dev/null 2>&1 || true
	@docker run --rm -v recon_recon-state:/state alpine sh -c \
	  'rm -rf /state/ca && echo "CA wiped — old client certs are now stale"'
	@$(COMPOSE) up -d --force-recreate hub
	@echo "CA regenerated on hub start. Re-bootstrap each agent with the install one-liner."

.PHONY: compose-bootstrap-agent
compose-bootstrap-agent: ## (Re-)bootstrap the local-compose-agent: stop it, wipe state, revoke prior identity on the hub, issue a fresh token, start it
	@$(COMPOSE) ps hub | grep -q "Up" || (echo "hub is not running — make compose-up first" && exit 1)
	@$(COMPOSE) --profile with-agent stop agent >/dev/null 2>&1 || true
	@$(COMPOSE) exec -T hub /usr/local/bin/recon-hub \
	  --config /etc/recon/hub.yaml --mode revoke \
	  --agent-id local-compose-agent --revoke-reason "compose-bootstrap-agent rerun" >/dev/null 2>&1 || true
	@token=$$($(COMPOSE) exec -T hub /usr/local/bin/recon-hub \
	  --config /etc/recon/hub.yaml --mode gen-token \
	  --agent-id local-compose-agent --token-ttl 1h 2>/dev/null | tail -1); \
	test -n "$$token" || (echo "failed to obtain bootstrap token" && exit 1); \
	echo "$$token" | $(COMPOSE) --profile with-agent run --rm --no-deps -T --entrypoint /bin/sh agent \
	  -c 'rm -f /var/lib/recon-agent/agent.pem /var/lib/recon-agent/agent.key /var/lib/recon-agent/hub-ca.pem /var/lib/recon-agent/bootstrap.token; \
	      cat > /var/lib/recon-agent/bootstrap.token && chmod 0600 /var/lib/recon-agent/bootstrap.token' && \
	echo "wiped stale cert + revoked prior identity + seeded fresh token; bringing the agent up" && \
	$(COMPOSE) --profile with-agent up -d agent
