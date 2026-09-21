# NikuCooker
#
# The web application is built into web/dist, which web/embed.go embeds into the
# Go binary. `make build` therefore depends on `make web-build`: building Go
# without it produces a binary that serves the "frontend not built" page.

SHELL := /bin/sh
.DEFAULT_GOAL := help

# Injected into the binary. codeRevision is not cosmetic: it participates in
# every artifact cache key, which is what stops a rebuilt binary from serving
# output produced by an older implementation. See docs/artifact-cache.md §2.2.
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT       ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
CODE_REVISION?= $(COMMIT)
BUILD_DATE   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.codeRevision=$(CODE_REVISION) \
	-X main.buildDate=$(BUILD_DATE)

BIN := nikucooker
BIN_WIN := nikucooker.exe

# CGO_ENABLED=0 is what makes the five-target release matrix a loop rather than
# a per-target toolchain problem. It is also why the SQLite driver is
# modernc.org/sqlite rather than mattn/go-sqlite3.
export CGO_ENABLED := 0

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

.PHONY: build
build: web-build ## Build the binary, with the web application embedded
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/nikucooker

.PHONY: build-go
build-go: ## Build the binary only, embedding whatever is already in web/dist
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/nikucooker

.PHONY: build-all
build-all: ## Cross-compile every release target into dist/
	@mkdir -p dist
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		out=dist/$(BIN)-$$os-$$arch; \
		[ "$$os" = windows ] && out=$$out.exe; \
		echo "  building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/nikucooker || exit 1; \
	done
	@echo "  built $$(ls dist | wc -l | tr -d ' ') targets"

# ---------------------------------------------------------------------------
# Frontend
# ---------------------------------------------------------------------------

.PHONY: web-install
web-install: ## Install web dependencies from the lockfile
	pnpm --dir web install --frozen-lockfile

.PHONY: web-build
web-build: ## Build the Vue application into web/dist
	pnpm --dir web install --frozen-lockfile
	pnpm --dir web build

.PHONY: web-dev
web-dev: ## Run the Vite dev server, proxying /api to a local core
	pnpm --dir web dev

# ---------------------------------------------------------------------------
# Python
# ---------------------------------------------------------------------------

.PHONY: ai-install
ai-install: ## Create the AI worker environment from uv.lock
	cd ai && uv sync --extra dev

.PHONY: ai-selfcheck
ai-selfcheck: ## Report whether the AI worker environment is usable
	cd ai && uv run python -m nikucooker_ai --selfcheck

# ---------------------------------------------------------------------------
# Quality
# ---------------------------------------------------------------------------

.PHONY: test
test: test-go test-ai test-web ## Run every test suite

.PHONY: test-go
test-go: ## Run the Go tests
	go test ./...

.PHONY: test-ai
test-ai: ## Run the Python tests
	cd ai && uv run pytest

.PHONY: test-web
test-web: ## Run the frontend tests
	pnpm --dir web test

.PHONY: lint
lint: lint-go lint-ai lint-web ## Lint every language

.PHONY: lint-go
lint-go: ## gofmt and go vet
	@out=$$(gofmt -l cmd internal pkg web 2>/dev/null); \
		if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi
	go vet ./...

.PHONY: lint-ai
lint-ai: ## ruff, format check, and pyright
	cd ai && uv run ruff check . && uv run ruff format --check . && uv run pyright

.PHONY: lint-web
lint-web: ## ESLint, then a full type check
	pnpm --dir web lint
	pnpm --dir web typecheck

.PHONY: fmt
fmt: ## Format Go and Python sources
	gofmt -w cmd internal pkg web
	cd ai && uv run ruff format .

# ---------------------------------------------------------------------------
# Phase 0 helpers
# ---------------------------------------------------------------------------

.PHONY: protocol-manifest
protocol-manifest: ## Regenerate the protocol fixture manifest after editing fixtures
	go test ./pkg/protocol -run TestManifest -update

# ---------------------------------------------------------------------------
# Docker
# ---------------------------------------------------------------------------

.PHONY: docker-cpu
docker-cpu: ## Build the CPU image
	docker build --target runtime-cpu --build-arg CODE_REVISION=$(CODE_REVISION) -t nikucooker:cpu .

.PHONY: docker-cuda
docker-cuda: ## Build the CUDA image
	docker build --target runtime-cuda --build-arg CODE_REVISION=$(CODE_REVISION) -t nikucooker:cuda .

# ---------------------------------------------------------------------------
# Housekeeping
# ---------------------------------------------------------------------------

.PHONY: clean
clean: ## Remove build output. Never touches data/, models/ or config/
	rm -rf $(BIN) $(BIN_WIN) dist web/dist/assets web/dist/index.html
	cd ai && rm -rf .pytest_cache .ruff_cache
