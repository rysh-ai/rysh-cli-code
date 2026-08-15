BIN      := rysh_local
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
        bundle-check bundle-rebuild build-frontend frontend-dev \
        clean \
        release release-snapshot release-check goreleaser-install goreleaser-local \
        setup-dist \
        help

# ── Default ───────────────────────────────────────────────────────────────────

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ── Build ─────────────────────────────────────────────────────────────────────

build: ## Build the rysh binary
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN) ./cmd/rysh
	@echo "Built $(BIN_DIR)/$(BIN)"

build-alt: ## Build legacy 'ry' alias binary
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

# ── Distribution Setup (one-time) ─────────────────────────────────────────────

setup-dist: ## Run the one-time automated distribution channel setup
	@chmod +x scripts/setup-distribution.sh
	@scripts/setup-distribution.sh

# ── Release (GoReleaser) ──────────────────────────────────────────────────────

goreleaser-install: ## Install goreleaser if not present
	@which goreleaser >/dev/null 2>&1 || (echo "Installing goreleaser..." && \
		curl -sSfL https://goreleaser.com/static/run | bash -s -- -b $(HOME)/.local/bin)

release-check: goreleaser-install ## Validate .goreleaser.yml configuration
	goreleaser check

release-snapshot: goreleaser-install ## Build all platform binaries locally (no publish)
	goreleaser release --snapshot --clean

## release: Tag and push a new release (triggers the full GitHub Actions pipeline).
##   Usage: make release VERSION_TAG=0.1.0
release:
	@[ -n "$(VERSION_TAG)" ] || (echo "Usage: make release VERSION_TAG=0.1.0" && exit 1)
	git tag -a "v$(VERSION_TAG)" -m "Release v$(VERSION_TAG)"
	git push origin "v$(VERSION_TAG)"
	@echo ""
	@echo "✓ Release v$(VERSION_TAG) tagged and pushed."
	@echo "  GitHub Actions will build, sign, and publish automatically."
	@echo "  Monitor: https://github.com/rysh-ai/rysh-cli/actions"

goreleaser-local: goreleaser-install ## Full release build locally (requires GITHUB_TOKEN)
	goreleaser release --clean
