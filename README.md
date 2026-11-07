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
make setup   # installs golangci-lint, sqlc, Go modules, frontend packages
make dev     # Postgres + migrations + Go API (:8090) + job worker + Vite (:5173)
```

Open http://127.0.0.1:5173. The header badge and the "System status" card show live API and database health. Vite proxies `/api/*` to the Go server. With `PROVIDER=mock`, emails (sign-in links) go to an in-memory mailbox at http://127.0.0.1:8090/dev/mailbox.

The UI for accounts and sessions arrives in Week 4; until then, drive the API directly:

```bash
curl -c jar -b jar -H 'Content-Type: application/json' \
  -d '{"email":"gm@example.com","password":"a long enough password"}' http://127.0.0.1:5173/api/auth/signup
curl -c jar -b jar -H 'Content-Type: application/json' -d '{"name":"My Campaign"}' http://127.0.0.1:5173/api/campaigns
# then POST notes to /api/campaigns/<id>/sessions and watch /api/sessions/<id>/events
```

## Common tasks

```bash
make help            # every target
make check           # lint + tests + eval + generated-type drift + build (run before pushing)
make eval            # score the canon pipeline on the eval campaign (mock by default)
make canon-run       # run the pipeline and write canon, party view and recaps to data/canon-run
make test            # Go tests (each DB test gets its own migrated Postgres database) + Vitest
make gen             # regenerate sqlc code and frontend API types after editing SQL or api/openapi.yaml
make migrate-status  # applied vs pending migrations
make db-shell        # psql into the dev database
make db-reset        # wipe local data and re-migrate
```

## Layout

```
api/openapi.yaml        API contract (source of truth for frontend types)
backend/
  cmd/api               HTTP server
  cmd/worker            background job runner (session processing); run as many as you like
  cmd/migrate           migration CLI (up, status)
  cmd/canon             run the canon pipeline over a campaign folder
  cmd/eval              score the pipeline against testdata/eval-campaign/answer-key.json
  internal/config       env config; PROVIDER=mock|aws, prod refuses mocks
  internal/app          shared startup: opens the DB as the restricted role, refuses unsafe roles
  internal/httpapi      routes, middleware (request ID, access log, panic recovery, CSRF, rate limits), handlers
  internal/auth         argon2id passwords, magic links, server-side sessions
  internal/mail         SES sender and the dev mailbox
  internal/store        transactions as a user (RLS) or system; sqlc queries in store/queries, generated code in store/dbq
  internal/canonstore   load canon from / apply proposals to Postgres
  internal/jobs         Postgres job queue (SKIP LOCKED, leases, backoff, LISTEN/NOTIFY wakeups)
  internal/processing   the "process_session" job: run the pipeline, store proposal and recaps
  internal/notify       session status events: NOTIFY on change, one LISTEN fanned out to SSE streams
  internal/testdb       a fresh migrated database per test
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

## Security model

- **Row-level security is the gate.** The API and worker run as Postgres role `campaign_app` (no `BYPASSRLS`). Every request runs in a transaction that sets `app.user_id`; policies on campaign tables decide what that user sees. GMs and players call the same endpoints and get different rows back. Players can read only approved, party-visible facts and names, published party recaps, and nothing from raw notes or proposals.
- **Secret columns are revoked.** `entities.description` may hold secrets, so the app role cannot select it; GMs read it through a GM-gated view.
- **The worker runs as the GM** who submitted the session, so RLS applies to background jobs too.
- **Startup checks.** The API and worker refuse to start if their effective role bypasses RLS, and in production if `DATABASE_URL` logs in as a superuser (it could `RESET ROLE`). Locally, the compose superuser login is fine because the pool runs `SET ROLE campaign_app`.
- **Accounts:** argon2id passwords; magic links are single-use and expire in 15 minutes; session and link tokens are stored only as SHA-256 hashes. The session cookie is HttpOnly, SameSite=Lax, and Secure in production. Cross-site browser requests are rejected, JSON bodies are required (no form posts), and auth endpoints are rate-limited per IP.

Tests in `internal/store/rls_test.go`, `internal/canonstore` and `internal/httpapi/api_test.go` check these guarantees against real Postgres.

## Session lifecycle

`queued` → `processing` → `review` → `approved` (or `failed` after 3 attempts).

1. A GM posts notes; the session row and its job are created in one transaction.
2. A worker claims the job (`FOR UPDATE SKIP LOCKED`), loads canon as that GM, runs the pipeline, and stores the proposal and recaps. Status changes are pushed with `NOTIFY` and streamed to the browser over SSE.
3. The GM approves. Sessions are approved in order, since each builds on the canon before it. A proposal computed against older canon is re-queued instead of applied, and approving a session re-queues any later sessions already in review.

## Canon pipeline

Each session runs these steps. Nothing becomes canon until approved; the CLI and eval approve everything automatically.

1. **Parse notes.** Text under a `## GM only` / `## GM notes` / `## Secrets` heading, or a paragraph starting `GM:`, `[GM]` or `Secret:`, is GM-only. Everything else is what happened at the table.
2. **Extract** entities and facts (strong model, forced tool call). Every fact must quote the notes verbatim, or it is dropped. Facts quoted from GM-only text are forced to GM visibility.
3. **Resolve** entities: exact name/alias match, then embedding similarity, then a model tiebreak for close calls (flagged for review when not high-confidence).
4. **Visibility.** Players know a name only if it appears in party text and is linked to a name they already use. A second, unrelated name ("a hooded stranger") stays GM-only, so the party view never connects a disguise to the real identity. Model-written statements that reveal any part of a secret name are replaced by their verbatim quote.
5. **Contradictions** are checked per entity against established canon (strong model).
6. **Recaps.** The GM recap sees everything. The player recap sees only party-visible facts, never the raw notes. If it still mentions a secret, it is regenerated once, then replaced with a plain list.

`PROVIDER=mock` uses `canon.HeuristicModel` and a hash embedder, so the pipeline runs with no AWS account. Its output is deliberately simple; the `mock` thresholds in the answer key guard the plumbing, and the `aws` thresholds measure real quality. Use `-record <dir>` / `-replay <dir>` on `cmd/eval` and `cmd/canon` to capture real Bedrock answers once and replay them offline.

## Endpoints

Full contract: [`api/openapi.yaml`](api/openapi.yaml).

| Route | Purpose |
|---|---|
| `GET /healthz`, `GET /readyz` | Liveness; readiness (503 if Postgres is unreachable) |
| `GET /api/health` | JSON status for the UI (503 when degraded) |
| `POST /api/auth/signup` · `login` · `logout` | Password accounts and sessions |
| `POST /api/auth/magic-link` · `magic-link/verify` | Passwordless sign-in by email |
| `GET /api/me` | The signed-in user |
| `GET/POST /api/campaigns` · `GET/PATCH/DELETE /api/campaigns/{id}` | Campaigns (create seeds the party's PCs) |
| `POST /api/campaigns/{id}/members` · `DELETE …/members/{user}` | Add or remove GMs and players |
| `GET /api/campaigns/{id}/canon` | Approved canon, filtered by your role |
| `GET /api/campaigns/{id}/recaps` | Published player recaps |
| `GET/POST /api/campaigns/{id}/sessions` | List sessions; submit notes (queues processing) |
| `GET /api/sessions/{id}` · `/proposal` | Session detail with recaps; the pending proposal |
| `POST /api/sessions/{id}/approve` · `/reprocess` | Approve into canon; re-run the pipeline |
| `GET /api/sessions/{id}/events` | Server-Sent Events with live status |
| `GET /dev/mailbox` | Dev only: emails the mock sender "sent" |
