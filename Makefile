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

.PHONY: clean
clean:
	rm -rf $(BIN_DIR)
