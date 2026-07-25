.PHONY: help build test vet lint clean check-secrets tidy run \
        build-darwin-arm64 build-linux-arm64 build-linux-amd64 build-all \
        deploy release-tag install-hooks install-precommit-hook \
        deploy-stage deploy-stage-local stage-status stage-rollback

# Derive a version string from git. Falls back to "dev" if not in a git repo.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)
GOFLAGS     := -trimpath -ldflags="$(LDFLAGS)"

help: ## Show this help
	@awk 'BEGIN{FS=":.*?## "}/^[a-zA-Z_-]+:.*?## /{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ── Dev loop ─────────────────────────────────────────────

build: ## Build for the current host
	go build $(GOFLAGS) -o mosaic-bridge ./cmd/bridge

run: build ## Build and run locally
	./mosaic-bridge

test: ## Run tests with the race detector
	go test -race ./...

vet: ## go vet
	go vet ./...

lint: ## staticcheck (install via: go install honnef.co/go/tools/cmd/staticcheck@latest)
	staticcheck ./...

tidy: ## Tidy go.mod / go.sum
	go mod tidy

check-secrets: ## Run the preflight secret scan
	./scripts/check-secrets.sh

install-hooks: ## Install git pre-push hook that runs check-secrets
	./scripts/check-secrets.sh --install

install-precommit-hook: ## Install git pre-commit hook (runs check-secrets on every commit, incl. TruffleHog)
	./scripts/check-secrets.sh --install-precommit

clean: ## Remove build artifacts
	rm -f mosaic-bridge mosaic-bridge-darwin-* mosaic-bridge-linux-*
	rm -rf dist/

# ── Release builds (cross-compile) ───────────────────────

build-darwin-arm64: ## Build for M-series Macs (gym MacBook)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
		go build $(GOFLAGS) -o dist/mosaic-bridge-darwin-arm64 ./cmd/bridge
	cd dist && shasum -a 256 mosaic-bridge-darwin-arm64 > mosaic-bridge-darwin-arm64.sha256

build-linux-arm64: ## Build for Linux ARM64 (UDM-Pro, Pi)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build $(GOFLAGS) -o dist/mosaic-bridge-linux-arm64 ./cmd/bridge
	cd dist && shasum -a 256 mosaic-bridge-linux-arm64 > mosaic-bridge-linux-arm64.sha256

build-linux-amd64: ## Build for Linux AMD64 (cloud VPS, dev containers)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build $(GOFLAGS) -o dist/mosaic-bridge-linux-amd64 ./cmd/bridge
	cd dist && shasum -a 256 mosaic-bridge-linux-amd64 > mosaic-bridge-linux-amd64.sha256

build-all: build-darwin-arm64 build-linux-arm64 build-linux-amd64 ## Build every release target

# ── Ops shortcuts ────────────────────────────────────────

# Tag a new release. CI on the tag push will build + publish the binaries.
#   make release-tag VERSION=v0.3.2
release-tag: check-secrets test ## Tag, run gates, push
	@if [ -z "$(VERSION)" ]; then echo "VERSION is required: make release-tag VERSION=v0.3.2"; exit 1; fi
	git tag -a $(VERSION) -m "Release $(VERSION)"
	git push origin $(VERSION)

# Trigger the MacBook to pull + install the latest release.
#   make deploy GYM=gym.local [TAG=v0.3.2]
GYM ?= mosaic-gym.local
TAG ?= latest
deploy: ## Tell the gym MacBook to pull + install the latest (or a specific) release
	# -t allocates a TTY so sudo can prompt for the Mac admin password.
	# Without it, sudo errors with "a terminal is required to read the password".
	# Type the password once per deploy — the whole run finishes in that single sudo session.
	ssh -t $(GYM) "sudo /usr/local/mosaic-bridge/update.sh $(TAG)"

# ── Staging ──────────────────────────────────────────────
#
# Staging runs as com.mosaic.bridge.stage on the same gym MacBook,
# in shadow mode, alongside prod. The deploy flow uses the existing
# CI artifact (mosaic-bridge-darwin-arm64 from .github/workflows/ci.yml)
# rather than a release tag — staging exists to soak feature branches
# before they earn a release tag. See CLAUDE.md "Staging" for the
# soak-rule and the per-PR workflow.

# deploy-stage: pull the CI-built darwin-arm64 artifact for the current
# branch, scp it to the gym, and run stage-update.sh. The current branch
# is auto-detected; override BRANCH= if you want to soak a specific one
# without checking it out locally.
#
# Requires `gh` CLI authenticated against the repo (gh auth login).
# CI must have completed for the branch — the workflow runs on PRs
# against main, so open the PR first.
BRANCH ?= $(shell git rev-parse --abbrev-ref HEAD)
deploy-stage: ## Deploy the latest CI-built darwin-arm64 artifact for the current branch to staging
	@if [ "$(BRANCH)" = "main" ]; then \
		echo "refusing to deploy main to staging — staging exists to soak feature branches"; \
		exit 1; \
	fi
	@command -v gh >/dev/null 2>&1 || { echo "gh CLI not found — install via 'brew install gh' and run 'gh auth login'"; exit 1; }
	@echo "fetching CI artifact for branch $(BRANCH)"
	@rm -rf dist/stage-fetch && mkdir -p dist/stage-fetch
	@RUN_ID=$$(gh run list --branch $(BRANCH) --workflow ci.yml --status success --limit 1 --json databaseId -q '.[0].databaseId'); \
	  if [ -z "$$RUN_ID" ]; then \
	    echo "no successful ci.yml run found for branch $(BRANCH) — open a PR or push more commits and wait for CI"; \
	    exit 1; \
	  fi; \
	  echo "using CI run $$RUN_ID"; \
	  gh run download $$RUN_ID --name mosaic-bridge-darwin-arm64 --dir dist/stage-fetch
	@scp dist/stage-fetch/mosaic-bridge-darwin-arm64 $(GYM):/tmp/mosaic-bridge-stage
	# -t per CLAUDE.md SSH+sudo rules: sudo needs a TTY for the admin password prompt.
	ssh -t $(GYM) "sudo /usr/local/mosaic-bridge-stage/stage-update.sh"

# deploy-stage-local: build darwin-arm64 locally and ship it. Escape hatch
# for hot debugging when waiting for CI is too slow. Use sparingly — the
# whole point of deploy-stage is that staging runs *exactly* what would
# go to prod, and a locally-built binary subtly weakens that guarantee.
deploy-stage-local: build-darwin-arm64 ## Build locally and deploy to staging (escape hatch — prefer deploy-stage)
	scp dist/mosaic-bridge-darwin-arm64 $(GYM):/tmp/mosaic-bridge-stage
	ssh -t $(GYM) "sudo /usr/local/mosaic-bridge-stage/stage-update.sh"

# stage-status: probe staging's /health and show the tail of its log.
# The /health response includes "instance":"stage" so you can tell at
# a glance you're talking to the staging process and not prod.
stage-status: ## Print staging /health + tail of bridge.log (over SSH)
	@ssh $(GYM) 'curl -fsS http://127.0.0.1:3600/health && echo && echo "── bridge.log (last 30 lines) ──" && sudo tail -30 /usr/local/mosaic-bridge-stage/bridge.log' || \
	  { echo "stage-status failed — try: ssh -t $(GYM) 'sudo tail -30 /usr/local/mosaic-bridge-stage/bridge.err'"; exit 1; }

# stage-rollback: revert staging to the previously-installed binary.
# Symmetric with `update.sh rollback` for the prod side.
stage-rollback: ## Roll staging back to the previous binary
	ssh -t $(GYM) "sudo /usr/local/mosaic-bridge-stage/stage-update.sh rollback"
