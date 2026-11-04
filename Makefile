# Campaign Assistant — developer entry points. Run `make help` for the list.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# Optional local overrides (see .env.example).
-include .env
export

DB_PORT           ?= 5433
DATABASE_URL      ?= postgres://campaign:campaign@127.0.0.1:$(DB_PORT)/campaign?sslmode=disable
TEST_DATABASE_URL ?= $(DATABASE_URL)
VERSION           ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GOLANGCI_LINT_VERSION := v2.15.0
OPENAPI_TS_VERSION    := 7.13.0
# openapi-typescript needs the TS 5 compiler API; the app itself uses TS 7.
OPENAPI_TS_TYPESCRIPT := 5.9.3

GOBIN         := $(shell go env GOPATH)/bin
GOLANGCI_LINT := $(GOBIN)/golangci-lint
LDFLAGS       := -s -w -X campaign-assistant/backend/internal/version.Version=$(VERSION)
OPENAPI_TS     = pnpm -s dlx --package=openapi-typescript@$(OPENAPI_TS_VERSION) --package=typescript@$(OPENAPI_TS_TYPESCRIPT) openapi-typescript

##@ Setup

.PHONY: setup
setup: tools ## Install tools and dependencies
	cd backend && go mod download
	cd frontend && pnpm install --frozen-lockfile

.PHONY: tools
tools: ## Install pinned dev tools (golangci-lint) and check prerequisites
	@command -v docker >/dev/null || { echo "docker is required"; exit 1; }
	@command -v pnpm   >/dev/null || { echo "pnpm is required: npm install -g pnpm"; exit 1; }
	@if ! $(GOLANGCI_LINT) version 2>/dev/null | grep -q "$(GOLANGCI_LINT_VERSION:v%=%)"; then \
		echo "installing golangci-lint $(GOLANGCI_LINT_VERSION)"; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	fi

##@ Development

.PHONY: dev
dev: db-up migrate ## Run Postgres, the Go API and the Vite dev server (Ctrl-C stops all)
	@echo "→ app: http://127.0.0.1:5173   api: http://$${HTTP_ADDR:-127.0.0.1:8090}"
	@$(MAKE) --no-print-directory -j2 dev-api dev-web

.PHONY: dev-api
dev-api:
	cd backend && go run ./cmd/api

.PHONY: dev-web
dev-web:
	cd frontend && pnpm -s dev

##@ Database

.PHONY: db-up
db-up: ## Start Postgres (docker compose) and wait until healthy
	docker compose up -d --wait db

.PHONY: db-down
db-down: ## Stop Postgres (keeps data)
	docker compose down

.PHONY: db-reset
db-reset: ## Destroy local Postgres data and re-run migrations
	docker compose down -v
	$(MAKE) --no-print-directory migrate

.PHONY: db-shell
db-shell: ## Open psql inside the Postgres container
	docker compose exec db psql -U campaign -d campaign

.PHONY: migrate
migrate: db-up ## Apply pending migrations
	cd backend && go run ./cmd/migrate up

.PHONY: migrate-status
migrate-status: db-up ## Show applied and pending migrations
	cd backend && go run ./cmd/migrate status

##@ Quality

.PHONY: check
check: lint test eval gen-check build ## Everything CI should run

.PHONY: lint
lint: lint-go lint-web ## Lint and type-check backend and frontend

.PHONY: lint-go
lint-go: tools
	cd backend && go vet ./... && $(GOLANGCI_LINT) run ./...

.PHONY: lint-web
lint-web:
	cd frontend && pnpm -s lint && pnpm -s typecheck

.PHONY: test
test: test-go test-web ## Run all tests (backend tests use the compose database)

.PHONY: test-go
test-go: db-up
	cd backend && TEST_DATABASE_URL='$(TEST_DATABASE_URL)' go test -race -count=1 ./...

.PHONY: test-web
test-web:
	cd frontend && pnpm -s test

.PHONY: eval
eval: ## Score the canon pipeline on the eval campaign (PROVIDER=aws for real Bedrock)
	cd backend && go run ./cmd/eval

.PHONY: canon-run
canon-run: ## Run the pipeline on the eval campaign and write results to data/canon-run
	cd backend && go run ./cmd/canon -out ../data/canon-run

.PHONY: gen
gen: ## Regenerate frontend API types from api/openapi.yaml
	cd frontend && $(OPENAPI_TS) ../api/openapi.yaml -o src/api/schema.d.ts

.PHONY: gen-check
gen-check: ## Fail if generated API types are out of date
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	cd frontend && $(OPENAPI_TS) ../api/openapi.yaml -o "$$tmp/schema.d.ts" >/dev/null 2>&1; \
	if ! diff -q src/api/schema.d.ts "$$tmp/schema.d.ts" >/dev/null; then \
		echo "frontend/src/api/schema.d.ts is stale; run 'make gen'"; exit 1; \
	fi; echo "generated API types are up to date"

##@ Build

.PHONY: build
build: build-go build-web ## Build release binaries and the static frontend

.PHONY: build-go
build-go:
	cd backend && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/ ./cmd/...

.PHONY: build-web
build-web:
	cd frontend && pnpm -s build

.PHONY: clean
clean: ## Remove build output
	rm -rf bin frontend/dist

##@ Help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } \
		/^[a-zA-Z_-]+:.*##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
