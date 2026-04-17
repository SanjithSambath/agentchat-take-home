.PHONY: help build run test test-integration tidy sqlc lint clean fmt

BINARY := bin/server
PKG    := ./...

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-10s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the server binary into bin/server.
	@mkdir -p bin
	go build -o $(BINARY) ./cmd/server

run: ## Run the server locally (reads .env via dotenv, falls back to process env).
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/server

test: ## Run unit tests.
	go test $(PKG) -race -count=1

test-integration: ## Run integration tests against live Neon/S2/Anthropic (reads .env).
	@set -a; [ -f .env ] && . ./.env; set +a; \
	go test ./tests/... -race -count=1 -tags integration -timeout 120s

tidy: ## Run go mod tidy.
	go mod tidy

sqlc: ## Regenerate sqlc code under internal/store/db/.
	sqlc generate

fmt: ## Run gofmt on the tree.
	gofmt -s -w .

lint: ## Run go vet.
	go vet $(PKG)

clean: ## Remove build artifacts.
	rm -rf bin

