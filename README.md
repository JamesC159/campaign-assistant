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
make check           # lint + tests + eval + generated-type drift + build (run before pushing)
make eval            # score the canon pipeline on the eval campaign (mock by default)
make canon-run       # run the pipeline and write canon, party view and recaps to data/canon-run
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
  cmd/canon             run the canon pipeline over a campaign folder
  cmd/eval              score the pipeline against testdata/eval-campaign/answer-key.json
  internal/config       env config; PROVIDER=mock|aws, prod refuses mocks
  internal/httpapi      routes, middleware (request ID, access log, panic recovery), handlers
  internal/migrate      advisory-locked, checksummed, transactional migrator
  internal/llm          Model interface, Bedrock Converse adapter, fixture record/replay
  internal/embed        Embedder interface, Titan adapter, offline hash embedder
  internal/canon        the pipeline: notes → extract → resolve → visibility → contradictions → recaps
  internal/eval         answer-key scoring (recall, merges, contradictions, secret leaks)
  internal/providers    builds mock or AWS clients from config
  migrations/           NNNN_name.sql, embedded into the binaries
  testdata/eval-campaign  5-session sample campaign with planted secrets and contradictions
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

## Canon pipeline

Each session runs these steps. Nothing becomes canon until approved; the CLI and eval approve everything automatically.

1. **Parse notes.** Text under a `## GM only` / `## GM notes` / `## Secrets` heading, or a paragraph starting `GM:`, `[GM]` or `Secret:`, is GM-only. Everything else is what happened at the table.
2. **Extract** entities and facts (strong model, forced tool call). Every fact must quote the notes verbatim, or it is dropped. Facts quoted from GM-only text are forced to GM visibility.
3. **Resolve** entities: exact name/alias match, then embedding similarity, then a model tiebreak for close calls (flagged for review when not high-confidence).
4. **Visibility.** Players know a name only if it appears in party text and is linked to a name they already use. A second, unrelated name ("a hooded stranger") stays GM-only, so the party view never connects a disguise to the real identity. Model-written statements that reveal any part of a secret name are replaced by their verbatim quote.
5. **Contradictions** are checked per entity against established canon (strong model).
6. **Recaps.** The GM recap sees everything. The player recap sees only party-visible facts, never the raw notes. If it still mentions a secret, it is regenerated once, then replaced with a plain list.

`PROVIDER=mock` uses `canon.HeuristicModel` and a hash embedder, so the pipeline runs with no AWS account. Its output is deliberately simple; the `mock` thresholds in the answer key guard the plumbing, and the `aws` thresholds measure real quality. Use `-record <dir>` / `-replay <dir>` on `cmd/eval` and `cmd/canon` to capture real Bedrock answers once and replay them offline.

## Endpoints so far

| Route | Purpose |
|---|---|
| `GET /healthz` | Liveness (process is up; no DB check) |
| `GET /readyz` | Readiness (503 if Postgres is unreachable) |
| `GET /api/health` | JSON status for the UI: version, env, provider, DB latency (503 when degraded) |
