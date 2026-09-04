GO               ?= go
BIN              := $(CURDIR)/bin
BINARY           := charon
VERSION          ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS          := -s -w -X main.version=$(VERSION)
GOLANGCI_VERSION ?= latest
GOLANGCI         := $(BIN)/golangci-lint

.DEFAULT_GOAL := help
.PHONY: help tools generate fmt fmt-check vet lint test test-race build ci hooks db-up db-down db-logs clean

help: ## Show available targets
	grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

tools: ## Install pinned dev tools into ./bin
	GOBIN=$(BIN) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

generate: ## Regenerate code from SQL and templates (no generators wired yet)
	@echo "generate: nothing to do until phase 1 introduces sqlc"

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

test: ## Run tests
	$(GO) test ./...

test-race: ## Run tests with the race detector
	$(GO) test -race -count=1 ./...

build: ## Build the charon binary into ./bin
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN)/$(BINARY) ./cmd/charon

ci: fmt-check vet lint test-race build ## Everything CI runs, in the same order
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
