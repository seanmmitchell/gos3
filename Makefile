# gos3 build, test and development tasks. Run `make help` for an overview.

SHELL := /bin/bash

GO      ?= go
PNPM    ?= pnpm
BIN     ?= bin/gos3
IMAGE   ?= gos3

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

VERSION_PKG := github.com/seanmmitchell/gos3/internal/version
LDFLAGS     := -s -w -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).Date=$(DATE)

UI_DIST        := internal/webui/dist
UI_PLACEHOLDER := internal/webui/placeholder.html
UI_BUILD       := web/.output/public

# Root credentials of `make dev` (any 3+ char key / 8+ char secret).
DEV_ACCESS_KEY ?= gos3dev
DEV_SECRET_KEY ?= gos3dev-secret
DEV_ADDRESS    ?= :9000
DEV_UI_ORIGIN  ?= http://localhost:3000

.DEFAULT_GOAL := build
.PHONY: help build build-go ui ui-clean notices test test-go test-web e2e lint fmt dev clean docker

help: ## show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ui build-go ## build the web UI and the gos3 binary with the UI embedded (bin/gos3)

build-go: ## build bin/gos3 with whatever is in internal/webui/dist (placeholder or UI build)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/gos3

# The embedded UI replaces the contents of internal/webui/dist. This
# overwrites the committed placeholder index.html: run `make ui-clean` before
# committing to restore it (built assets are git-ignored).
ui: ## build the embedded web UI into internal/webui/dist
	cd web && $(PNPM) install --frozen-lockfile && $(PNPM) generate:embed
	find $(UI_DIST) -mindepth 1 -maxdepth 1 ! -name .keep -exec rm -rf {} +
	cp -R $(UI_BUILD)/. $(UI_DIST)/
	@# The SPA build may only have 200.html; serve it for the UI root too.
	test -f $(UI_DIST)/index.html || cp $(UI_DIST)/200.html $(UI_DIST)/index.html

ui-clean: ## restore the committed placeholder in internal/webui/dist
	find $(UI_DIST) -mindepth 1 -maxdepth 1 ! -name .keep -exec rm -rf {} +
	cp $(UI_PLACEHOLDER) $(UI_DIST)/index.html
	touch $(UI_DIST)/.keep

# Both web UI builds record the npm packages they ship in web/.notices
# (web/modules/third-party-notices.ts); the generator adds the Go modules
# linked into gos3. CI fails when the committed file is out of date.
notices: ## regenerate THIRD_PARTY_NOTICES (builds both web UI variants first)
	cd web && $(PNPM) install --frozen-lockfile && $(PNPM) build && $(PNPM) generate:embed
	$(GO) run ./internal/tools/notices

test: test-go test-web ## run all Go and web unit tests

test-go: ## run the Go tests with the race detector
	$(GO) test -race ./...

test-web: ## run the web UI unit tests (Vitest)
	cd web && $(PNPM) test

e2e: ## run the end-to-end tests against a real server
	$(GO) test -count=1 ./test/e2e/...

lint: ## check formatting and run go vet
	@unformatted="$$(gofmt -l cmd internal test 2>/dev/null)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...

fmt: ## format the Go code
	gofmt -w cmd internal $$(test -d test && echo test)

dev: ## run an ephemeral in-memory server for UI development (pair with `cd web && pnpm dev`)
	@echo "gos3 dev server: http://localhost$(DEV_ADDRESS)  (memory backend, data is lost on exit)"
	@echo "  access key: $(DEV_ACCESS_KEY)"
	@echo "  secret key: $(DEV_SECRET_KEY)"
	@echo "  CORS allows $(DEV_UI_ORIGIN); start the UI with: cd web && pnpm install && pnpm dev"
	@echo "  then add the server http://localhost$(DEV_ADDRESS) with the keys above."
	GOS3_ROOT_ACCESS_KEY=$(DEV_ACCESS_KEY) GOS3_ROOT_SECRET_KEY=$(DEV_SECRET_KEY) \
		$(GO) run ./cmd/gos3 serve --backend memory --address $(DEV_ADDRESS) \
		--cors-origin $(DEV_UI_ORIGIN) --log-level debug

clean: ui-clean ## remove build output (binary, UI build) and restore the UI placeholder
	rm -rf bin web/.output web/.nuxt

docker: ## build the container image (gos3:<git version>, override IMAGE / VERSION) from the Dockerfile
	docker build -t $(IMAGE):$(VERSION) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

# --- additional targets (docs, deployment, compatibility) ---------------------

UI_IMAGE ?= gos3-ui

.PHONY: docker-ui compat e2e-web demo demo-down

docker-ui: ## build the standalone web UI image (gos3-ui:<git version>, override UI_IMAGE / VERSION)
	docker build -f deploy/docker/web.Dockerfile -t $(UI_IMAGE):$(VERSION) .

compat: ## run the boto3 / AWS CLI compatibility suite (test/compat/run.sh)
	test/compat/run.sh

e2e-web: build ## run the Playwright suites (mocked, real server, standalone) against bin/gos3
	cd web && $(PNPM) test:e2e
	cd web && GOS3_BIN=$(CURDIR)/$(BIN) E2E_SKIP_UI_BUILD=1 $(PNPM) test:e2e:real
	cd web && GOS3_BIN=$(CURDIR)/$(BIN) $(PNPM) test:e2e:standalone

demo: ## start the two-node docker compose demo with the standalone UI (localhost ports 9001, 9002, 3000)
	GOS3_VERSION=$(VERSION) docker compose --profile ui up -d --build

demo-down: ## stop the docker compose demo (add -v to the command to delete its volumes)
	docker compose --profile ui down
