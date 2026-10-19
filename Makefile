BIN      := rysh
BIN_ALT  := rysh
BIN_DIR  := .
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

GO       := go
GOFLAGS  :=
LDFLAGS  := -s -w -X main.version=$(VERSION)

# Isolate from the monorepo go.work — rysh-cli has its own pinned dependency tree
# that conflicts with the server's charmbracelet/x/ansi version.
export GOWORK=off

.PHONY: build build-alt install \
        deps \
        test test-cover wire-test wire-test-real registry registry-serve \
        lint vet \
        bundle-check bundle-stamp-check bundle-rebuild build-frontend frontend-dev \
        clean \
        release-snapshot release-check goreleaser-install \
        help

# ── Default ───────────────────────────────────────────────────────────────────

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ── Build ─────────────────────────────────────────────────────────────────────

build: facetime-helper ## Build the rysh binary
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN) ./cmd/rysh
	@echo "Built $(BIN_DIR)/$(BIN)"

build-alt: facetime-helper ## Build legacy 'ry' alias binary
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN_ALT) ./cmd/rysh
	@echo "Built $(BIN_DIR)/$(BIN_ALT)"

build-script-shim: ## Build the rysh-script shebang shim (#!/usr/bin/env rysh-script)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/rysh-script ./cmd/rysh-script
	@echo "Built $(BIN_DIR)/rysh-script"

install: build ## Build and install rysh to ~/.local/bin
	install -d $(HOME)/.local/bin
	install $(BIN_DIR)/$(BIN) $(HOME)/.local/bin/$(BIN)
	@echo "Installed to $(HOME)/.local/bin/$(BIN)"

# ── Dependencies ──────────────────────────────────────────────────────────────

deps: ## Tidy Go module dependencies
	$(GO) mod tidy
	$(GO) mod download

# ── Frontend (embedded web assets) ────────────────────────────────────────────
# internal/web serves the SHARED rysh-cli-app renderer (web target), built into
# internal/web/static. There is no separate frontend app anymore — the old
# internal/web/frontend duplicate was removed (see web_electron_roadmap). The
# canonical build is `make -f Makefile.internal_web all` at the repo root; these
# targets are conveniences that defer to the shared renderer.

# bundle-check answers "is internal/web/static what the renderer actually builds
# to?" — the thing nothing verified while the bundle went stale twice in one
# afternoon (E-50). It is deliberately NOT part of `test`: it needs node and npm,
# and the Go suite must not grow a JavaScript toolchain dependency.
bundle-check: ## Verify internal/web/static matches a fresh build of the renderer (E-50)
	@./scripts/check-embedded-bundle.sh

# bundle-stamp-check answers the other half: is the RUNNING daemon serving the
# bundle in this checkout? //go:embed bakes static/ in at compile time, so a
# binary built before a bundle refresh keeps serving the old UI. /health reports
# web_bundle = sha256(embedded static/index.html)[:12]; this compares it with the
# checkout's file. Local daemon only (127.0.0.1); not part of `test`.
BUNDLE_STAMP_PORT ?= 23232
bundle-stamp-check: ## Check the running daemon's embedded web bundle matches internal/web/static (MATCH/STALE)
	@want=$$(shasum -a 256 internal/web/static/index.html | cut -c1-12); \
	got=$$(curl -s --max-time 5 http://127.0.0.1:$(BUNDLE_STAMP_PORT)/health | sed -n 's/.*"web_bundle":"\([0-9a-f]*\)".*/\1/p'); \
	if [ -z "$$got" ]; then \
		echo "UNKNOWN: no web_bundle from 127.0.0.1:$(BUNDLE_STAMP_PORT)/health (daemon down, or built before the stamp existed)"; exit 2; \
	elif [ "$$got" = "$$want" ]; then \
		echo "MATCH: running daemon serves web bundle $$got"; \
	else \
		echo "STALE: daemon serves $$got, checkout has $$want — rebuild the rysh binary"; exit 1; \
	fi

bundle-rebuild: ## Rebuild internal/web/static from the renderer, then commit it
	@./scripts/check-embedded-bundle.sh --write

# build-frontend used to test for ../rysh-cli-app-code and, when it was missing,
# print a note and exit 0. From a worktree that path never exists, so it
# rebuilt nothing and reported success — which is how the bundle went stale
# while everyone believed they had refreshed it. It now goes through the same
# script, which finds the renderer from a worktree too and FAILS when it cannot.
build-frontend: ## Build embedded web assets from the shared rysh-cli-app renderer
	@./scripts/check-embedded-bundle.sh --write
	@echo "NOTE: rebuild the rysh binary so //go:embed static/* refreshes."

frontend-dev: ## Run the shared renderer dev server (rysh-cli-app)
	cd ../rysh-cli-app-code && npm run dev

# ── Test ──────────────────────────────────────────────────────────────────────

test: ## Run all tests
	$(GO) test -v ./...

test-live: ## Run LIVE channel round-trips (LV1; needs RYSH_LIVE_* creds, skips when absent)
	$(GO) test -tags livechannels -run TestLive -v ./internal/channels/

registry: ## Build a publishable package registry from packages/ into .build/registry
	GOWORK=off $(GO) run ./cmd/registry-index -src packages -out .build/registry

registry-serve: registry ## Build it and serve it on :8791 for local `rysh install @rysh/...`
	@echo "RYSH_REGISTRY_URL=http://127.0.0.1:8791/index.json rysh install @rysh/code-reviewer"
	@cd .build/registry && python3 -m http.server 8791

wire-test: ## Prove a wrapped CLI's provider traffic is governed (writes wire.log + .cast)
	@mkdir -p .build/wire
	GOWORK=off $(GO) run ./cmd/wire-harness -out .build/wire

wire-test-real: ## Same, but drives the real `claude` binary (the stronger claim)
	@mkdir -p .build/wire-real
	GOWORK=off $(GO) run ./cmd/wire-harness -client=real -out .build/wire-real

wire-test-openai: ## OpenAI-dialect mechanism proof (builtin client)
	@mkdir -p .build/wire-openai
	GOWORK=off $(GO) run ./cmd/wire-harness -dialect openai -out .build/wire-openai

wire-test-codex: ## Drive the real `codex` binary through the proxy (OpenAI dialect)
	@mkdir -p .build/wire-codex
	GOWORK=off $(GO) run ./cmd/wire-harness -dialect openai -client=real -timeout 120s -out .build/wire-codex

test-cover: ## Run tests with coverage report
	@mkdir -p .build
	$(GO) test -coverprofile=.build/coverage.out ./...
	$(GO) tool cover -html=.build/coverage.out -o .build/coverage.html
	@echo "Coverage report: .build/coverage.html"

# ── Lint ──────────────────────────────────────────────────────────────────────

vet: ## Run go vet
	$(GO) vet ./...

lint: vet ## Run linters (go vet + staticcheck if available)
	@which staticcheck >/dev/null 2>&1 && staticcheck ./... || true

# ── Clean ─────────────────────────────────────────────────────────────────────

clean: ## Remove build artefacts
	rm -f $(BIN_DIR)/$(BIN) $(BIN_DIR)/$(BIN_ALT)
	rm -rf .build dist
	@echo "Cleaned."

# ── Release (GoReleaser) ──────────────────────────────────────────────────────

goreleaser-install: ## Install goreleaser if not present
	@which goreleaser >/dev/null 2>&1 || (echo "Installing goreleaser..." && \
		curl -sSfL https://goreleaser.com/static/run | bash -s -- -b $(HOME)/.local/bin)

release-check: goreleaser-install ## Validate .goreleaser.yml configuration
	goreleaser check

release-snapshot: goreleaser-install ## Build all platform binaries locally (no publish)
	goreleaser release --snapshot --clean

.PHONY: facetime-helper
facetime-helper: ## Build the embedded macOS Contacts helper (macOS hosts only)
	@if [ "$$(uname -s)" = Darwin ]; then ./scripts/build-facetime-helper.sh; fi
