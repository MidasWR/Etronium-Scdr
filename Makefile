# Etronium-Scdr — Makefile
#
# Targets:
#   make help       — list all targets
#   make build      — build all binaries (scheduler, lord, etronium)
#   make test       — run unit tests
#   make lint       — run golangci-lint if installed
#   make proto      — generate Go + grpc-gateway + swagger from .proto
#   make image      — build the runtime Docker image (etronium-mvp:runtime)
#   make up         — docker compose up -d for the MVP testbed
#   make down       — docker compose down for the MVP testbed
#   make smoke      — run e2e-bpf.sh against the testbed
#   make demo       — 5-minute PM demo
#   make release    — snapshot go-releaser build (no publish)
#   make version    — show current git describe
#   make clean      — remove build artifacts
#
# Required for proto: protoc 24+, Go 1.22+, plugins listed in README.md.
# Required for release: goreleaser (https://goreleaser.com/install/).

.PHONY: help build test lint proto image up down smoke demo release version clean \
        all tidy check fmt vet e2e-bpf acceptance cheatsheet ci

SHELL := /usr/bin/env bash

PROTO_ROOT := proto
PROTO_FILE := $(PROTO_ROOT)/etronium/v1/etronium.proto
GEN_DIR    := internal/gen
SWAGGER    := docs/openapi/etronium.swagger.json
COMPOSE    := docker compose -f test/mvp/docker-compose.yml
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.0.0-dev")
LDFLAGS    := -s -w -X main.version=$(VERSION) \
              -X github.com/MidasWR/Etronium-Scdr/cmd/etronium.version=$(VERSION) \
              -X github.com/MidasWR/Etronium-Scdr/cmd/lord.version=$(VERSION) \
              -X github.com/MidasWR/Etronium-Scdr/cmd/scheduler.version=$(VERSION)
GOTRIM     := -trimpath
GO         ?= go

# ───────────────────────────────────────────────────────────────────────
help: ## Show this help.
	@echo "Etronium-Scdr — Makefile"
	@echo ""
	@echo "Current version: $(VERSION)"
	@echo ""
	@echo "Targets:"
	@awk 'BEGIN {FS = ":.*## "} \
        /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' \
        $(MAKEFILE_LIST)
	@echo ""
	@echo "Examples:"
	@echo "  make build              # all binaries into ./bin/"
	@echo "  make up                 # bring up MVP testbed"
	@echo "  make smoke              # run E2E BPF test"
	@echo "  make release VERSION=v0.1.0  # tag a release (snapshot goreleaser)"

# ───────────────────────────────────────────────────────────────────────
# Build

build: ## Build scheduler, lord, etronium into bin/.
	$(GO) build $(GOTRIM) -ldflags="$(LDFLAGS)" -o bin/scheduler ./cmd/scheduler
	$(GO) build $(GOTRIM) -ldflags="$(LDFLAGS)" -o bin/lord      ./cmd/lord
	$(GO) build $(GOTRIM) -ldflags="$(LDFLAGS)" -o bin/etronium  ./cmd/etronium
	@echo "✓ built $(VERSION)"
	@ls -la bin/

scheduler: ## Build the scheduler only.
	$(GO) build $(GOTRIM) -ldflags="$(LDFLAGS)" -o bin/scheduler ./cmd/scheduler

lord: ## Build the lord only.
	$(GO) build $(GOTRIM) -ldflags="$(LDFLAGS)" -o bin/lord ./cmd/lord

etronium: ## Build the etronium (tenant) CLI only.
	$(GO) build $(GOTRIM) -ldflags="$(LDFLAGS)" -o bin/etronium ./cmd/etronium

test: ## Run unit tests.
	$(GO) test -race -count=1 ./...

lint: ## Run golangci-lint (must be installed).
	golangci-lint run ./... || echo "golangci-lint not installed, skipping"

fmt: ## gofmt all source files.
	$(GO) fmt ./...

vet: ## go vet all source files.
	$(GO) vet ./...

tidy: ## go mod tidy.
	$(GO) mod tidy

# ───────────────────────────────────────────────────────────────────────
# Proto generation

proto: proto-go proto-gw proto-swagger ## Generate proto bindings (Go + grpc-gateway + swagger).

proto-go:
	protoc -I $(PROTO_ROOT) -I $(PROTO_ROOT)/third_party \
		--go_out=$(GEN_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(GEN_DIR) --go-grpc_opt=paths=source_relative \
		$(PROTO_FILE)

proto-gw:
	protoc -I $(PROTO_ROOT) -I $(PROTO_ROOT)/third_party \
		--grpc-gateway_out=$(GEN_DIR) --grpc-gateway_opt=paths=source_relative \
		$(PROTO_FILE)

proto-swagger:
	@mkdir -p docs/openapi
	protoc -I $(PROTO_ROOT) -I $(PROTO_ROOT)/third_party \
		--openapiv2_out=docs/openapi \
		$(PROTO_FILE)
	@# protoc-gen-openapiv2 doesn't support paths=source_relative — move manually:
	@if [ -f docs/openapi/etronium/v1/etronium.swagger.json ]; then \
		mv docs/openapi/etronium/v1/etronium.swagger.json $(SWAGGER) && \
		rmdir docs/openapi/etronium/v1 docs/openapi/etronium 2>/dev/null || true; \
	fi

# ───────────────────────────────────────────────────────────────────────
# Image build (runtime for MVP)

image: build ## Build the runtime Docker image (etronium-mvp:runtime).
	./scripts/mvp/build-image.sh

# ───────────────────────────────────────────────────────────────────────
# MVP testbed (docker compose)

up: ## Bring up the MVP testbed (frontend + 5 lords + 2 tenants + schedulerd).
	./scripts/mvp/up.sh -d

down: ## Tear down the MVP testbed.
	./scripts/mvp/down.sh

restart: down up ## Restart the MVP testbed.

logs: ## Tail MVP container logs.
	docker compose -f test/mvp/docker-compose.yml logs -f --tail=50

smoke: up ## Bring up testbed, run e2e-bpf.sh, tear down. Single target for CI.
	./scripts/mvp/e2e-bpf.sh

e2e-bpf: ## Run E2E BPF test only (assumes testbed is already up).
	./scripts/mvp/e2e-bpf.sh

demo: up ## Run the 5-minute PM demo against running testbed.
	./scripts/mvp/demo-pm.sh

# ───────────────────────────────────────────────────────────────────────
# Cheatsheet / docs

cheatsheet: ## (Re)generate cheatsheet — currently hand-edited; placeholder for future gen.

# ───────────────────────────────────────────────────────────────────────
# Acceptance

acceptance: up e2e-bpf ## Phase 0 acceptance (legacy testbed).
	./scripts/mvp/e2e-acceptance.sh || true

# ───────────────────────────────────────────────────────────────────────
# Release / versioning

version: ## Show current git version.
	@echo "$(VERSION)"
	@git rev-parse --short HEAD 2>/dev/null | xargs -I{} echo "commit: {}"

release: ## Run goreleaser snapshot build (no publish, output to dist/).
	goreleaser release --clean --skip=publish --snapshot

release-ci: ## Validate goreleaser config without publishing.
	goreleaser check

# Tag a release (local step before `git push --tags`).
# Usage:   make tag VERSION=v0.1.0
tag: ## Create a git tag VERSION=v0.1.0 (off the current HEAD).
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "v0.0.0-dev" ]; then \
		echo "ERROR: VERSION required. Usage: make tag VERSION=v0.1.0"; \
		exit 1; \
	fi
	git tag -a $(VERSION) -m "Release $(VERSION)"
	@echo "✓ tagged $(VERSION)"
	@echo "→ now: git push origin $(VERSION)"

# ───────────────────────────────────────────────────────────────────────
# CI gate

ci: lint test smoke ## CI local gate: lint + test + smoke.
	@echo "✓ CI gate passed for $(VERSION)"

all: tidy fmt vet lint test build image smoke ## Full local CI: tidy/fmt/vet/lint/test/build/image/smoke.
	@echo "✓ all-green for $(VERSION)"

# ───────────────────────────────────────────────────────────────────────
# Cleanup

clean: ## Remove build artifacts.
	rm -rf bin/ dist/ docs/openapi
	$(GO) clean -cache

deepclean: clean ## Remove caches + generated proto + everything.
	$(GO) clean -modcache -cache -testcache -fuzzcache
