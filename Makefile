GO               ?= go
BIN              := $(CURDIR)/bin
BINARY           := charon
VERSION          ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS          := -s -w -X main.version=$(VERSION)
GOLANGCI_VERSION ?= latest
GOLANGCI         := $(BIN)/golangci-lint
SQLC             := $(BIN)/sqlc
SQLC_VERSION     ?= latest
TEST_DB          := charon-test-postgres
TEST_DB_PORT     ?= 5440
TEST_DATABASE_URL := postgres://charon:charon@localhost:$(TEST_DB_PORT)/charon?sslmode=disable
TEMPL            := $(BIN)/templ
TEMPL_VERSION    ?= latest

.DEFAULT_GOAL := help
.PHONY: help tools generate generate-check test-db test-db-stop fmt fmt-check vet lint test test-race build ci hooks db-up db-down db-logs clean

help: ## Show available targets
	grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

tools: ## Install dev tools into ./bin
	GOBIN=$(BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	GOBIN=$(BIN) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	GOBIN=$(BIN) $(GO) install github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION)

generate: $(SQLC) $(TEMPL) ## Regenerate the database layer and the templates
	$(SQLC) generate
	$(TEMPL) generate --log-level error

generate-check: ## Fail if regenerating would change anything
	before="$$(find internal/postgres/db internal/web -name '*.go' -exec shasum {} + | shasum)"; \
	$(MAKE) --no-print-directory generate >/dev/null; \
	after="$$(find internal/postgres/db internal/web -name '*.go' -exec shasum {} + | shasum)"; \
	if [ "$$before" != "$$after" ]; then \
	  echo "generated code is stale: run 'make generate' and commit the result"; exit 1; \
	fi

$(SQLC) $(TEMPL):
	$(MAKE) tools

fmt: ## Format all code
	gofmt -w .
	@test -x $(GOLANGCI) && $(GOLANGCI) fmt || echo "fmt: run 'make tools' for import grouping"

fmt-check: ## Fail if any file is not gofmt-clean
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
	  echo "not gofmt-clean:"; echo "$$out"; exit 1; \
	fi

vet: ## Run go vet
	$(GO) vet ./...

lint: $(GOLANGCI) ## Run golangci-lint
	$(GOLANGCI) run

$(GOLANGCI):
	$(MAKE) tools

test-db: ## Start the PostgreSQL every test package shares
	@if [ -z "$$(docker ps -q -f name=^/$(TEST_DB)$$)" ]; then \
		docker rm -f $(TEST_DB) >/dev/null 2>&1 || true; \
		docker run -d --name $(TEST_DB) --shm-size=1g \
			-e POSTGRES_USER=charon -e POSTGRES_PASSWORD=charon -e POSTGRES_DB=charon \
			-p $(TEST_DB_PORT):5432 postgres:18-alpine >/dev/null; \
		until docker exec $(TEST_DB) pg_isready -U charon -d charon >/dev/null 2>&1; do sleep 1; done; \
	fi

test-db-stop: ## Remove the PostgreSQL the tests share
	@docker rm -f $(TEST_DB) >/dev/null 2>&1 || true

test: test-db ## Run tests
	CHARON_TEST_DATABASE_URL=$(TEST_DATABASE_URL) $(GO) test ./...

test-race: test-db ## Run tests with the race detector
	CHARON_TEST_DATABASE_URL=$(TEST_DATABASE_URL) $(GO) test -race -count=1 ./...

build: ## Build the charon binary into ./bin
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/$(BINARY) ./cmd/charon

ci: fmt-check vet lint generate-check test-race build ## Everything CI runs, in the same order
	echo "ci: ok ($(VERSION))"

hooks: ## Install the local git hooks
	git config core.hooksPath .githooks
	echo "hooks: core.hooksPath -> .githooks"

db-up: ## Start Postgres
	docker compose up -d --wait postgres

db-down: ## Stop Postgres and drop its volume
	docker compose down -v

db-logs: ## Tail Postgres logs
	docker compose logs -f postgres

clean: ## Remove build output
	rm -rf $(BIN)
