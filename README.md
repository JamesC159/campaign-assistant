# Campaign Assistant

Session recaps, a living world wiki, and continuity checks for tabletop game masters.
Working name. See [PLAN.md](PLAN.md) for the product and build plan.

## Stack

| Layer | Tech |
|---|---|
| Backend | Go 1.27 (`net/http`, `pgx`, `slog`), forward-only SQL migrations |
| Database | PostgreSQL 18 + pgvector (docker compose locally) |
| Frontend | React 19, TypeScript 7, Vite 8, pnpm, React Router, TanStack Query, CSS Modules |
| API contract | [`api/openapi.yaml`](api/openapi.yaml) → generated TS types |
| AI / storage | AWS Bedrock, Transcribe, S3, SES, **mocked by default** (`PROVIDER=mock`) |

## Prerequisites

- Go 1.22+ on PATH (the `go.mod` toolchain line auto-downloads Go 1.27.3)
- Node 22+ and pnpm (`npm install -g pnpm`)
- Docker with Compose

## Quick start

```bash
make setup   # installs golangci-lint, Go modules, frontend packages
make dev     # Postgres + migrations + Go API (:8090) + Vite (:5173)
```

Open http://127.0.0.1:5173. The header badge and the "System status" card show live API and database health. Vite proxies `/api/*` to the Go server.

## Common tasks

```bash
make help            # every target
make check           # lint + tests + generated-type drift + build (run before pushing)
make test            # Go tests (incl. real-Postgres migration tests) + Vitest
make gen             # regenerate frontend/src/api/schema.d.ts after editing api/openapi.yaml
make migrate-status  # applied vs pending migrations
make db-shell        # psql into the dev database
make db-reset        # wipe local data and re-migrate
```

## Layout

```
api/openapi.yaml        API contract (source of truth for frontend types)
backend/
  cmd/api               HTTP server
  cmd/migrate           migration CLI (up, status)
  internal/config       env config; PROVIDER=mock|aws, prod refuses mocks
  internal/httpapi      routes, middleware (request ID, access log, panic recovery), handlers
  internal/migrate      advisory-locked, checksummed, transactional migrator
  migrations/           NNNN_name.sql, embedded into the binaries
frontend/
  src/api               generated schema types + fetch client
  src/components        layout, status badge
  src/routes            pages
  src/styles/tokens.css design tokens (light + dark)
docs/screenshots/       UI snapshots per milestone
```

## Configuration

All settings come from environment variables with local-dev defaults. Copy `.env.example` to `.env` to override (the Makefile loads it). Key rules:

- `PROVIDER=mock` (default) runs with no AWS account. `APP_ENV=prod` refuses `PROVIDER=mock`.
- Migrations are forward-only. Never edit an applied migration; the migrator rejects checksum changes.

## Endpoints so far

| Route | Purpose |
|---|---|
| `GET /healthz` | Liveness (process is up; no DB check) |
| `GET /readyz` | Readiness (503 if Postgres is unreachable) |
| `GET /api/health` | JSON status for the UI: version, env, provider, DB latency (503 when degraded) |
