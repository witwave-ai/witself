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

.PHONY: help db-up db-down db-reset serve login test build

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
	@go run ./cmd/ws gen-bootstrap-token --out $(DEV_TOKEN)
	@echo "bootstrap token written to $(DEV_TOKEN); run 'make login' in another terminal"
	@echo "account provisioning enabled with dev token: $(DEV_PROVISION)"
	WITSELF_DATABASE_URL="$(DEV_DSN)" WITSELF_BOOTSTRAP_TOKEN="$$(cat $(DEV_TOKEN))" \
		WITSELF_PROVISION_TOKEN="$(DEV_PROVISION)" \
		go run ./cmd/witself-server serve

login: ## Exchange the dev bootstrap token for an operator token (saved to .dev/operator.token)
	go run ./cmd/ws auth login --endpoint $(ENDPOINT) --bootstrap-token-file $(DEV_TOKEN) --out $(DEV_OPERATOR)

psql: ## Open psql against the dev database
	docker compose exec postgres psql -U witself -d witself

build: ## Build every ./cmd/... binary into ./bin (ws, witself-server, witself-control-plane, witself-admin)
	@mkdir -p bin
	go build -o bin/ ./cmd/...

test: ## Run the Go tests
	go test ./...
