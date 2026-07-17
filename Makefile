# Dev helpers. Postgres runs in Docker Compose; the server and CLI run natively
# for fast iteration. Production uses the Helm chart + an external database, not
# this. See compose.yaml.

DEV_DSN       := postgres://witself:witself@localhost:5432/witself?sslmode=disable
DEV_TOKEN     := .dev/bootstrap.token
DEV_OPERATOR  := .dev/operator.token
# Fixed dev-only provision token so account-provisioning is hand-testable
# locally (see docs/runbooks.md). Never a real credential.
DEV_PROVISION := witself_prv_dev-local-only
ENDPOINT      := http://localhost:8080

# Pin golangci-lint to the same version ci.yml installs so `make check`
# and CI can never disagree about what clean means.
GOLANGCI_LINT_VERSION := v2.12.2

MEMORY_LOAD_QUALITY_RESULTS     ?= /tmp/witself-memory-load-quality.json
MEMORY_LOAD_QUALITY_SEED        ?= 20260717
MEMORY_LOAD_QUALITY_NOISE       ?= 250
MEMORY_LOAD_QUALITY_ITERATIONS  ?= 25
MEMORY_LOAD_QUALITY_CONCURRENCY ?= 4
MEMORY_LOAD_QUALITY_RELEASE     ?= $(shell git describe --tags --always --dirty)
MEMORY_LOAD_QUALITY_COMMIT      ?= $(shell git rev-parse HEAD)
MEMORY_LOAD_QUALITY_PROVIDER    ?= local
MEMORY_LOAD_QUALITY_HARDWARE    ?= unspecified

.PHONY: help db-up db-down db-reset serve login test test-integration test-memory-cloud-conformance test-memory-load-quality build check

help: ## List targets
	@grep -hE '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed -E 's/:[^#]*## /\t/' | sort

db-up: ## Start the dev Postgres (waits until healthy)
	docker compose up -d --wait

db-down: ## Stop the dev Postgres
	docker compose down

db-reset: ## Stop the dev Postgres and wipe its data volume
	docker compose down -v

serve: db-up ## Run witself-server against the dev DB (mints a fresh bootstrap token)
	@mkdir -p .dev
	@go run ./cmd/witself gen-bootstrap-token --out $(DEV_TOKEN)
	@echo "bootstrap token written to $(DEV_TOKEN); run 'make login' in another terminal"
	@echo "account provisioning enabled with dev token: $(DEV_PROVISION)"
	WITSELF_DATABASE_URL="$(DEV_DSN)" WITSELF_BOOTSTRAP_TOKEN="$$(cat $(DEV_TOKEN))" \
		WITSELF_PROVISION_TOKEN="$(DEV_PROVISION)" \
		go run ./cmd/witself-server serve

login: ## Exchange the dev bootstrap token for an operator token (saved to .dev/operator.token)
	go run ./cmd/witself auth login --endpoint $(ENDPOINT) --bootstrap-token-file $(DEV_TOKEN) --out $(DEV_OPERATOR)

psql: ## Open psql against the dev database
	docker compose exec postgres psql -U witself -d witself

build: ## Build every ./cmd/... binary into ./bin (witself, witself-server, witself-control-plane, witself-admin)
	@mkdir -p bin
	go build -o bin/ ./cmd/...

test: ## Run the Go tests
	go test ./...

test-integration: db-up ## Run the PostgreSQL-backed store tests
	WITSELF_TEST_DATABASE_URL="$(DEV_DSN)" go test ./internal/store -count=1

test-memory-cloud-conformance: ## Run the opt-in 3x3 memory/account-move rehearsal or certification
	WITSELF_MEMORY_CLOUD_CONFORMANCE=1 go test ./internal/store \
		-run '^TestNarrativeMemoryManagedCloudConformance$$' -count=1 -v -timeout 90m

test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY := 1
test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY_RESULTS := $(MEMORY_LOAD_QUALITY_RESULTS)
test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY_SEED := $(MEMORY_LOAD_QUALITY_SEED)
test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY_NOISE_MEMORIES := $(MEMORY_LOAD_QUALITY_NOISE)
test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY_QUERY_ITERATIONS := $(MEMORY_LOAD_QUALITY_ITERATIONS)
test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY_CONCURRENCY := $(MEMORY_LOAD_QUALITY_CONCURRENCY)
test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY_RELEASE := $(MEMORY_LOAD_QUALITY_RELEASE)
test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY_COMMIT := $(MEMORY_LOAD_QUALITY_COMMIT)
test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY_PROVIDER := $(MEMORY_LOAD_QUALITY_PROVIDER)
test-memory-load-quality: export WITSELF_MEMORY_LOAD_QUALITY_HARDWARE_TIER := $(MEMORY_LOAD_QUALITY_HARDWARE)
test-memory-load-quality: ## Run the opt-in deterministic PostgreSQL memory load/quality baseline
	@test -n "$$WITSELF_TEST_DATABASE_URL" || { \
		echo "WITSELF_TEST_DATABASE_URL is required (use a dedicated test database principal)"; \
		exit 2; \
	}
	@go test ./internal/store -run '^TestNarrativeMemoryLoadQualityPostgres$$' \
			-count=1 -v -timeout 30m
	@printf 'sanitized result: %s\n' "$$WITSELF_MEMORY_LOAD_QUALITY_RESULTS"

check: ## Run CI's go gates locally (gofmt, vet, build, test -race, golangci-lint) — run before every push
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needs to run on:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	go build ./...
	go test ./... -race
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
	$(MAKE) check-infra
	@echo "check: all gates green"

check-infra: ## Gates for the NESTED infra/pulumi module (root ./... never descends into it)
	cd infra/pulumi && go vet ./...
	cd infra/pulumi && go build ./...
	cd infra/pulumi && go test ./...
	cd infra/pulumi && go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
	@echo "check-infra: infra gates green"
