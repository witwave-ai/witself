# Witself build and meta targets.
#
# The skeleton uses the Go standard library only; cobra and other runtime
# dependencies arrive with the real implementation. These targets wrap the
# standard toolchain for the two binaries (witself, witself-server) plus the
# packaging tools used by CI and release.

# Module / binary metadata.
MODULE       := github.com/witwave-ai/witself
BIN_CLI      := witself
BIN_SERVER   := witself-server
CMD_CLI      := ./cmd/$(BIN_CLI)
CMD_SERVER   := ./cmd/$(BIN_SERVER)
BIN_DIR      := bin

# Version stamping, injected into internal/version via -ldflags. Defaults match
# the package defaults so a plain `go build` still produces a usable binary.
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT       ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE         ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG  := $(MODULE)/internal/version
LDFLAGS      := -s -w \
	-X '$(VERSION_PKG).Version=$(VERSION)' \
	-X '$(VERSION_PKG).Commit=$(COMMIT)' \
	-X '$(VERSION_PKG).Date=$(DATE)'

# Tool versions / images used by the meta targets.
DOCKER       ?= docker
GO           ?= go
GOLANGCI     ?= golangci-lint
GOVULNCHECK  ?= govulncheck
HELM         ?= helm
TERRAFORM    ?= terraform

GHCR_PREFIX  := ghcr.io/witwave-ai/images
IMG_CLI      := $(GHCR_PREFIX)/$(BIN_CLI)
IMG_SERVER   := $(GHCR_PREFIX)/$(BIN_SERVER)
IMG_TAG      ?= $(VERSION)

.DEFAULT_GOAL := build

.PHONY: help
help: ## Show this help.
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

## --- Build -----------------------------------------------------------------

.PHONY: build
build: build-cli build-server ## Build both binaries into $(BIN_DIR).

.PHONY: build-cli
build-cli: ## Build the witself CLI (includes mcp serve).
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN_CLI) $(CMD_CLI)

.PHONY: build-server
build-server: ## Build the witself-server backend.
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN_SERVER) $(CMD_SERVER)

.PHONY: install
install: ## Install both binaries into GOBIN.
	$(GO) install -trimpath -ldflags "$(LDFLAGS)" $(CMD_CLI) $(CMD_SERVER)

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR) dist

## --- Quality ---------------------------------------------------------------

.PHONY: test
test: ## Run the race-enabled test suite with coverage.
	$(GO) test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...

.PHONY: vet
vet: ## Run go vet over all packages.
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint over all packages.
	$(GOLANGCI) run ./...

.PHONY: fmt
fmt: ## Format all Go sources in place.
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is not gofmt-clean.
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: tidy
tidy: ## Tidy and verify the module graph.
	$(GO) mod tidy
	$(GO) mod verify

.PHONY: vuln
vuln: ## Scan for known vulnerabilities (govulncheck).
	$(GOVULNCHECK) ./...

## --- Packaging -------------------------------------------------------------

.PHONY: docker
docker: docker-cli docker-server ## Build both container images.

.PHONY: docker-cli
docker-cli: ## Build the witself CLI image.
	$(DOCKER) build -f images/$(BIN_CLI)/Dockerfile -t $(IMG_CLI):$(IMG_TAG) .

.PHONY: docker-server
docker-server: ## Build the witself-server image.
	$(DOCKER) build -f images/$(BIN_SERVER)/Dockerfile -t $(IMG_SERVER):$(IMG_TAG) .

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart.
	$(HELM) lint charts/witself

.PHONY: tf-fmt
tf-fmt: ## Check Terraform formatting recursively.
	$(TERRAFORM) -chdir=infra/terraform fmt -recursive -check

## --- Aggregate -------------------------------------------------------------

.PHONY: ci
ci: fmt-check vet lint test tidy vuln ## Run the standard local CI gate.
