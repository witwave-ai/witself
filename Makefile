# Dev helpers. Postgres runs in Docker Compose; the server and CLI run natively
# for fast iteration. Production uses the Helm chart + an external database, not
# this. See compose.yaml.

DEV_DSN   := postgres://witself:witself@localhost:5432/witself?sslmode=disable
DEV_TOKEN := .dev/bootstrap.token
ENDPOINT  := http://localhost:8080

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
	WITSELF_DATABASE_URL="$(DEV_DSN)" WITSELF_BOOTSTRAP_TOKEN="$$(cat $(DEV_TOKEN))" \
		go run ./cmd/witself-server serve

login: ## Exchange the dev bootstrap token for an operator token
	go run ./cmd/ws auth login --endpoint $(ENDPOINT) --bootstrap-token-file $(DEV_TOKEN)

build: ## Build both binaries into ./bin
	@mkdir -p bin
	go build -o bin/ ./cmd/...

test: ## Run the Go tests
	go test ./...
