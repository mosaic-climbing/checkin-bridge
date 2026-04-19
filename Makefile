.PHONY: help build test vet lint clean check-secrets tidy run \
        build-darwin-arm64 build-linux-arm64 build-linux-amd64 build-all \
        deploy release-tag install-hooks install-precommit-hook

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
