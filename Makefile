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

MEMORY_CURATION_LOAD_RESULTS              ?= /tmp/witself-memory-curation-load.json
MEMORY_CURATION_LOAD_SEED                 ?= 20260831
MEMORY_CURATION_LOAD_COALESCING_REQUESTS  ?= 24
MEMORY_CURATION_LOAD_CLAIM_REQUESTS       ?= 6
MEMORY_CURATION_LOAD_CLAIM_WORKERS        ?= 4
MEMORY_CURATION_LOAD_PAGING_CARDINALITIES ?= 4,16,64
MEMORY_CURATION_LOAD_PAGE_SIZE            ?= 8
MEMORY_CURATION_LOAD_CHAIN_BACKLOG        ?= 24
MEMORY_CURATION_LOAD_CHAIN_CAP            ?= 6
MEMORY_CURATION_LOAD_LEASE_CYCLES         ?= 3
MEMORY_CURATION_LOAD_MAX_ATTEMPTS         ?= 3
MEMORY_CURATION_LOAD_RELEASE              ?= $(shell git describe --tags --always --dirty)
MEMORY_CURATION_LOAD_COMMIT               ?= $(shell git rev-parse HEAD)
MEMORY_CURATION_LOAD_PROVIDER             ?= local
MEMORY_CURATION_LOAD_HARDWARE             ?= unspecified

# Leave the recall result path empty by default so ParseRecallOptions can use
# its pid-scoped path. This avoids concurrent Make invocations choosing the
# same retained evidence file.
MEMORY_RECALL_LOAD_RESULTS                     ?=
MEMORY_RECALL_LOAD_SEED                        ?= 20260831
MEMORY_RECALL_LOAD_CARDINALITIES               ?= 100,500,2000
MEMORY_RECALL_LOAD_QUERY_ITERATIONS            ?= 10
MEMORY_RECALL_LOAD_CONCURRENCY                 ?= 4
MEMORY_RECALL_LOAD_VECTOR_DIMENSIONS           ?= 32
MEMORY_RECALL_LOAD_VECTOR_COVERAGE_PERCENTAGES ?= 100,50
MEMORY_RECALL_LOAD_PAGINATION_LIMIT            ?= 64
MEMORY_RECALL_LOAD_RESULT_BUDGET               ?= 256
MEMORY_RECALL_LOAD_RELEASE                     ?= $(shell git describe --tags --always --dirty)
MEMORY_RECALL_LOAD_COMMIT                      ?= $(shell git rev-parse HEAD)
MEMORY_RECALL_LOAD_PROVIDER                    ?= local
MEMORY_RECALL_LOAD_HARDWARE                    ?= unspecified

# Leave the archive result path empty by default so ParseArchiveOptions can use
# its pid-scoped path. This avoids concurrent Make invocations choosing the
# same retained evidence file.
MEMORY_ARCHIVE_LOAD_RESULTS                                ?=
MEMORY_ARCHIVE_LOAD_SEED                                   ?= 20260831
MEMORY_ARCHIVE_LOAD_CARDINALITIES                          ?= 100,500,2000
MEMORY_ARCHIVE_LOAD_VERSIONS_PER_MEMORY                    ?= 2
MEMORY_ARCHIVE_LOAD_EVIDENCE_PER_MEMORY                    ?= 2
MEMORY_ARCHIVE_LOAD_RELATIONS_PER_MEMORY                   ?= 1
MEMORY_ARCHIVE_LOAD_TAGS_PER_VERSION                       ?= 3
MEMORY_ARCHIVE_LOAD_TRANSCRIPT_SHARE_PERCENT               ?= 25
MEMORY_ARCHIVE_LOAD_TRANSCRIPT_ENTRIES_PER_SELECTED_MEMORY ?= 2
MEMORY_ARCHIVE_LOAD_VECTOR_DIMENSIONS                      ?= 32
MEMORY_ARCHIVE_LOAD_RELEASE                                ?= $(shell git describe --tags --always --dirty)
MEMORY_ARCHIVE_LOAD_COMMIT                                 ?= $(shell git rev-parse HEAD)
MEMORY_ARCHIVE_LOAD_PROVIDER                               ?= local
MEMORY_ARCHIVE_LOAD_HARDWARE                               ?= unspecified

# Leave the concurrency result path empty by default so the harness can select
# its pid-scoped path and concurrent invocations cannot replace each other's
# retained evidence.
MEMORY_CONCURRENCY_LOAD_RESULTS                   ?=
MEMORY_CONCURRENCY_LOAD_SEED                      ?= 20260901
MEMORY_CONCURRENCY_LOAD_ACCOUNTS                  ?= 4
MEMORY_CONCURRENCY_LOAD_REALMS_PER_ACCOUNT        ?= 2
MEMORY_CONCURRENCY_LOAD_AGENTS_PER_REALM          ?= 4
MEMORY_CONCURRENCY_LOAD_SEED_MEMORIES_PER_AGENT   ?= 4
MEMORY_CONCURRENCY_LOAD_WORKERS_PER_AGENT         ?= 2
MEMORY_CONCURRENCY_LOAD_OPERATIONS_PER_WORKER     ?= 2
MEMORY_CONCURRENCY_LOAD_ISOLATION_ITERATIONS      ?= 2
MEMORY_CONCURRENCY_LOAD_CLAIM_WORKERS             ?= 4
MEMORY_CONCURRENCY_LOAD_RELEASE                   ?= $(shell git describe --tags --always --dirty)
MEMORY_CONCURRENCY_LOAD_COMMIT                    ?= $(shell git rev-parse HEAD)
MEMORY_CONCURRENCY_LOAD_PROVIDER                  ?= local
MEMORY_CONCURRENCY_LOAD_HARDWARE                  ?= unspecified

.PHONY: help db-up db-down db-reset serve login test test-integration test-memory-cloud-conformance test-memory-load-quality test-memory-curation-load test-memory-recall-load test-memory-archive-load test-memory-concurrency-load feature-status build check check-infra

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

build: ## Build every ./cmd/... binary into ./bin, including the server and worker
	@mkdir -p bin
	go build -o bin/ ./cmd/...

test: ## Run the Go tests
	go test ./...

test-integration: db-up ## Run the PostgreSQL-backed store tests in a disposable database
	@set -eu; \
		integration_db="witself_test_$$(date -u +%Y%m%d%H%M%S)_$$$$"; \
		cleanup_integration_db() { \
			docker compose exec -T postgres dropdb --if-exists -U witself "$$integration_db" >/dev/null; \
		}; \
		trap cleanup_integration_db EXIT HUP INT TERM; \
		docker compose exec -T postgres createdb -U witself "$$integration_db"; \
		WITSELF_TEST_DATABASE_URL="postgres://witself:witself@localhost:5432/$$integration_db?sslmode=disable" \
			go test ./internal/store -count=1 -timeout=30m

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

test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD := 1
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_RESULTS := $(MEMORY_CURATION_LOAD_RESULTS)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_SEED := $(MEMORY_CURATION_LOAD_SEED)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_COALESCING_REQUESTS := $(MEMORY_CURATION_LOAD_COALESCING_REQUESTS)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_CLAIM_REQUESTS := $(MEMORY_CURATION_LOAD_CLAIM_REQUESTS)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_CLAIM_WORKERS := $(MEMORY_CURATION_LOAD_CLAIM_WORKERS)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_PAGING_CARDINALITIES := $(MEMORY_CURATION_LOAD_PAGING_CARDINALITIES)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_PAGE_SIZE := $(MEMORY_CURATION_LOAD_PAGE_SIZE)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_CHAIN_BACKLOG := $(MEMORY_CURATION_LOAD_CHAIN_BACKLOG)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_CHAIN_CAP := $(MEMORY_CURATION_LOAD_CHAIN_CAP)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_LEASE_CYCLES := $(MEMORY_CURATION_LOAD_LEASE_CYCLES)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_MAX_ATTEMPTS := $(MEMORY_CURATION_LOAD_MAX_ATTEMPTS)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_RELEASE := $(MEMORY_CURATION_LOAD_RELEASE)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_COMMIT := $(MEMORY_CURATION_LOAD_COMMIT)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_PROVIDER := $(MEMORY_CURATION_LOAD_PROVIDER)
test-memory-curation-load: export WITSELF_MEMORY_CURATION_LOAD_HARDWARE_TIER := $(MEMORY_CURATION_LOAD_HARDWARE)
test-memory-curation-load: ## Run the opt-in deterministic PostgreSQL memory-curation load/lifecycle harness
	@test -n "$$WITSELF_TEST_DATABASE_URL" || { \
		echo "WITSELF_TEST_DATABASE_URL is required (use a dedicated test database principal)"; \
		exit 2; \
	}
	@go test ./internal/store -run '^TestNarrativeMemoryCurationLoadPostgres$$' \
			-count=1 -v -timeout 12m
	@printf 'sanitized result: %s\n' "$$WITSELF_MEMORY_CURATION_LOAD_RESULTS"

test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD := 1
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_RESULTS := $(MEMORY_RECALL_LOAD_RESULTS)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_SEED := $(MEMORY_RECALL_LOAD_SEED)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_CARDINALITIES := $(MEMORY_RECALL_LOAD_CARDINALITIES)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_QUERY_ITERATIONS := $(MEMORY_RECALL_LOAD_QUERY_ITERATIONS)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_CONCURRENCY := $(MEMORY_RECALL_LOAD_CONCURRENCY)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_VECTOR_DIMENSIONS := $(MEMORY_RECALL_LOAD_VECTOR_DIMENSIONS)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_VECTOR_COVERAGE_PERCENTAGES := $(MEMORY_RECALL_LOAD_VECTOR_COVERAGE_PERCENTAGES)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_PAGINATION_LIMIT := $(MEMORY_RECALL_LOAD_PAGINATION_LIMIT)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_RESULT_BUDGET := $(MEMORY_RECALL_LOAD_RESULT_BUDGET)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_RELEASE := $(MEMORY_RECALL_LOAD_RELEASE)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_COMMIT := $(MEMORY_RECALL_LOAD_COMMIT)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_PROVIDER := $(MEMORY_RECALL_LOAD_PROVIDER)
test-memory-recall-load: export WITSELF_MEMORY_RECALL_LOAD_HARDWARE_TIER := $(MEMORY_RECALL_LOAD_HARDWARE)
test-memory-recall-load: ## Run the opt-in deterministic PostgreSQL memory-recall load/quality harness
	@test -n "$$WITSELF_TEST_DATABASE_URL" || { \
		echo "WITSELF_TEST_DATABASE_URL is required (use a dedicated test database principal)"; \
		exit 2; \
	}
	@go test ./internal/store -run '^TestNarrativeMemoryRecallLoadPostgres$$' \
			-count=1 -v -timeout 12m
	@if [ -n "$$WITSELF_MEMORY_RECALL_LOAD_RESULTS" ]; then \
		printf 'sanitized result: %s\n' "$$WITSELF_MEMORY_RECALL_LOAD_RESULTS"; \
	else \
		printf 'sanitized result: pid-scoped path reported by the harness\n'; \
	fi

test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD := 1
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_RESULTS := $(MEMORY_ARCHIVE_LOAD_RESULTS)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_SEED := $(MEMORY_ARCHIVE_LOAD_SEED)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_CARDINALITIES := $(MEMORY_ARCHIVE_LOAD_CARDINALITIES)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_VERSIONS_PER_MEMORY := $(MEMORY_ARCHIVE_LOAD_VERSIONS_PER_MEMORY)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_EVIDENCE_PER_MEMORY := $(MEMORY_ARCHIVE_LOAD_EVIDENCE_PER_MEMORY)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_RELATIONS_PER_MEMORY := $(MEMORY_ARCHIVE_LOAD_RELATIONS_PER_MEMORY)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_TAGS_PER_VERSION := $(MEMORY_ARCHIVE_LOAD_TAGS_PER_VERSION)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_TRANSCRIPT_SHARE_PERCENT := $(MEMORY_ARCHIVE_LOAD_TRANSCRIPT_SHARE_PERCENT)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_TRANSCRIPT_ENTRIES_PER_SELECTED_MEMORY := $(MEMORY_ARCHIVE_LOAD_TRANSCRIPT_ENTRIES_PER_SELECTED_MEMORY)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_VECTOR_DIMENSIONS := $(MEMORY_ARCHIVE_LOAD_VECTOR_DIMENSIONS)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_RELEASE := $(MEMORY_ARCHIVE_LOAD_RELEASE)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_COMMIT := $(MEMORY_ARCHIVE_LOAD_COMMIT)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_PROVIDER := $(MEMORY_ARCHIVE_LOAD_PROVIDER)
test-memory-archive-load: export WITSELF_MEMORY_ARCHIVE_LOAD_HARDWARE_TIER := $(MEMORY_ARCHIVE_LOAD_HARDWARE)
test-memory-archive-load: ## Run the opt-in deterministic whole-account archive round-trip harness
	@test -n "$$WITSELF_TEST_DATABASE_URL" || { \
		echo "WITSELF_TEST_DATABASE_URL is required (use a dedicated test database principal)"; \
		exit 2; \
	}
	@go test ./internal/store -run '^TestNarrativeMemoryArchiveLoadPostgres$$' \
			-count=1 -v -timeout 15m
	@if [ -n "$$WITSELF_MEMORY_ARCHIVE_LOAD_RESULTS" ]; then \
		printf 'sanitized result: %s\n' "$$WITSELF_MEMORY_ARCHIVE_LOAD_RESULTS"; \
	else \
		printf 'sanitized result: pid-scoped path reported by the harness\n'; \
	fi

test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD := 1
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_RESULTS := $(MEMORY_CONCURRENCY_LOAD_RESULTS)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_SEED := $(MEMORY_CONCURRENCY_LOAD_SEED)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_ACCOUNTS := $(MEMORY_CONCURRENCY_LOAD_ACCOUNTS)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_REALMS_PER_ACCOUNT := $(MEMORY_CONCURRENCY_LOAD_REALMS_PER_ACCOUNT)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_AGENTS_PER_REALM := $(MEMORY_CONCURRENCY_LOAD_AGENTS_PER_REALM)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_SEED_MEMORIES_PER_AGENT := $(MEMORY_CONCURRENCY_LOAD_SEED_MEMORIES_PER_AGENT)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_WORKERS_PER_AGENT := $(MEMORY_CONCURRENCY_LOAD_WORKERS_PER_AGENT)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_OPERATIONS_PER_WORKER := $(MEMORY_CONCURRENCY_LOAD_OPERATIONS_PER_WORKER)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_ISOLATION_ITERATIONS := $(MEMORY_CONCURRENCY_LOAD_ISOLATION_ITERATIONS)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_CLAIM_WORKERS := $(MEMORY_CONCURRENCY_LOAD_CLAIM_WORKERS)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_RELEASE := $(MEMORY_CONCURRENCY_LOAD_RELEASE)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_COMMIT := $(MEMORY_CONCURRENCY_LOAD_COMMIT)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_PROVIDER := $(MEMORY_CONCURRENCY_LOAD_PROVIDER)
test-memory-concurrency-load: export WITSELF_MEMORY_CONCURRENCY_LOAD_HARDWARE_TIER := $(MEMORY_CONCURRENCY_LOAD_HARDWARE)
test-memory-concurrency-load: ## Run the opt-in concurrent-agent and tenant-isolation harness
	@test -n "$$WITSELF_TEST_DATABASE_URL" || { \
		echo "WITSELF_TEST_DATABASE_URL is required (use a dedicated test database principal)"; \
		exit 2; \
	}
	@go test ./internal/store -run '^TestNarrativeMemoryConcurrencyLoadPostgres$$' \
			-count=1 -v -timeout 5h
	@if [ -n "$$WITSELF_MEMORY_CONCURRENCY_LOAD_RESULTS" ]; then \
		printf 'sanitized result: %s\n' "$$WITSELF_MEMORY_CONCURRENCY_LOAD_RESULTS"; \
	else \
		printf 'sanitized result: pid-scoped path reported by the harness\n'; \
	fi

feature-status: ## Regenerate the reviewed feature status scorecard
	go run ./internal/cmd/render-feature-status

check: ## Run CI's go gates locally (gofmt, vet, build, test -race, golangci-lint) — run before every push
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needs to run on:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	go build ./...
	go test ./... -race -timeout=30m
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
	$(MAKE) check-infra
	@echo "check: all gates green"

check-infra: ## Gates for nested Pulumi plus the isolated Cloudflare Workers
	cd infra/pulumi && go vet ./...
	cd infra/pulumi && go build ./...
	cd infra/pulumi && go test ./...
	cd infra/pulumi && go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...
	npm --prefix infra/cloudflare/agent-email test
	npm --prefix infra/cloudflare/agent-email run bundle:check
	npm --prefix infra/cloudflare/agent-email-send test
	npm --prefix infra/cloudflare/agent-email-send run bundle:check
	npm --prefix infra/cloudflare/support-email-intake ci
	npm --prefix infra/cloudflare/support-email-intake test
	npm --prefix infra/cloudflare/support-email-intake run bundle:check
	bash scripts/test-agent-email-cell-operation.sh
	bash scripts/test-agent-email-receipt-proof.sh
	bash scripts/test-agent-email-cell-smoke.sh
	npm --prefix infra/cloudflare/control-plane test
	npm --prefix infra/cloudflare/control-plane run bundle:check
	@echo "check-infra: infra gates green"
