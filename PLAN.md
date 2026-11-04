# Campaign Assistant — Build Plan (v2)

> Working name only. Pick a real name before launch, and keep "D&D" / "Dungeons & Dragons" out of it (they're trademarks). See Risks.

**Stack constraints (fixed):**
- Go backend, and a React + Vite + TypeScript frontend managed with pnpm.
- AWS Bedrock for AI. Every AWS dependency is **mocked** until an account exists.
- No platform providers (no Vercel, Supabase, Firebase, Inngest, Auth0, etc.). Auth, job queue, metering and the API are built in-house. A new codebase, sharing no code with other projects.

---

## 1. The product in one sentence

**After every session, the GM uploads notes or audio. They get a recap, an updated wiki, and warnings when something contradicts what's already been established. Players get a spoiler-safe version of the wiki.**

### Why this wins against what exists
| Existing tool | What it does | What it lacks |
|---|---|---|
| World Anvil, Kanka, LegendKeeper, Obsidian, Notion | Great wikis | You write every page by hand. Most GMs give up by session 6. |
| Discord recap bots / generic AI chat | Summaries | No memory across sessions, no structure, invents things |
| VTTs (Foundry, Roll20) | Running the game | Weak on lore and continuity |

**The moat is the "Canon" model:** structured facts, each citing the session it came from, with GM-only vs party-known visibility and a GM approval step.

### Core principles
1. **The GM approves everything.** AI *proposes* canon changes and never edits silently.
2. **Every fact cites its source**: session number plus the quote it came from.
3. **Secrets never leak.** Players only ever see party-known facts. This is enforced by Postgres row-level security, not just the UI.
4. **System-agnostic.** No rules content, so no IP problems.
5. **Runs fully offline in dev.** Every external dependency has a mock, so `make dev` works with no AWS account and no network.

---

## 2. MVP scope

### In v1
- **Campaigns → Sessions.** Paste notes, or upload an audio recording.
- **Session pipeline:** transcript → recap → extract entities and facts → match them to existing canon → flag contradictions → **review queue**.
- **Review queue:** "14 new facts, 3 updates, 1 conflict." Approve, edit or reject each one, or approve all.
- **Auto-wiki:** one page per NPC, place, faction, item or plot thread, built from approved facts, with citations.
- **Two recaps per session:** GM (full) and player (spoiler-safe).
- **Ask the Canon:** questions answered with session citations.
- **NPC generator** grounded in existing canon.
- **Player share link:** read-only, party-visible wiki and recaps.
- **Subscriptions + usage limits** behind a billing interface (mocked first, see §9).

### Explicitly NOT in v1
Maps, dice, character sheets, rules lookup, VTT integration, image generation, live voice bot, mobile app.

---

## 3. Technology decisions

| Concern | Choice | Why |
|---|---|---|
| Backend language | **Go 1.27**, standard library `net/http` (method + path routing) | No framework needed. One static binary per role. |
| Database | **PostgreSQL 18 + pgvector** | See below |
| DB access | `pgx/v5` + **sqlc** (typed queries generated from SQL) | Plain SQL with compile-time safety, no ORM |
| Migrations | Small in-house migrator: embedded `migrations/*.sql`, `schema_migrations` table | No extra dependency; ~100 lines |
| Background jobs | **In-house Postgres queue** (`FOR UPDATE SKIP LOCKED`) | Transactional with app data; nothing else to run |
| AI | **AWS Bedrock** via `aws-sdk-go-v2` (Converse API + tool-use for structured JSON) | Behind a `llm.Model` interface, with a mock implementation |
| Embeddings | Bedrock embeddings model (1024-dim) | Behind an `embed.Embedder` interface, with a mock |
| Transcription | AWS Transcribe | Behind a `transcribe.Service` interface, with a mock |
| File storage | S3 | Behind a `blob.Store` interface, with a local-disk implementation |
| Email | AWS SES | Behind a `mail.Sender` interface. The mock logs messages and lists them at `/dev/mailbox`. |
| Auth | In-house: argon2id passwords + email magic links, server-side sessions in Postgres, HTTP-only cookies | No auth provider |
| Realtime status | Server-Sent Events from Go | Simpler than WebSockets for one-way pipeline progress |
| API contract | Hand-written `api/openapi.yaml`. TS types generated with `openapi-typescript`. | One source of truth for front and back end |
| Frontend | React 19 + TypeScript 7 + Vite 8, **pnpm**, React Router, TanStack Query, CSS Modules, oxlint | Few dependencies, no UI kit |
| Testing | Go `testing` + Postgres in docker compose; Vitest; Playwright for e2e | — |
| Local env | docker compose (Postgres+pgvector only), `make dev` | — |
| Prod (later) | AWS: ECS Fargate (api + worker), RDS Postgres (pgvector), S3, CloudFront for the SPA. Terraform/OpenTofu IaC. | All AWS, no other providers |

### Why PostgreSQL (not MongoDB)
- **The data is relational:** campaigns → members → sessions → entities → facts → relationships → contradictions. Joins and foreign keys matter.
- **Row-level security** makes "players never see GM secrets" a database guarantee. MongoDB has no equivalent.
- **Transactions:** "approve all 14 proposals" either fully applies or fully rolls back.
- **pgvector** handles entity matching and Ask-the-Canon retrieval in the same database.
- **Full-text search** (`tsvector`) for the wiki search box.
- **Job queue:** `SKIP LOCKED` replaces a separate queue service.

One database instead of four separate services (DB, vector store, search, queue).

---

## 4. Repository layout

```
campaign-assistant/
├── Makefile                  # dev, test, lint, gen, migrate, eval, build
├── docker-compose.yml        # postgres+pgvector for local dev
├── api/openapi.yaml          # API contract
├── backend/                  # Go module
│   ├── cmd/
│   │   ├── api/              # HTTP server
│   │   ├── worker/           # job runner (pipeline steps)
│   │   ├── migrate/          # applies migrations
│   │   ├── canon/            # CLI: run the pipeline on a folder of session files
│   │   └── eval/             # runs eval suite, prints scorecard
│   ├── internal/
│   │   ├── config/           # env-based config; PROVIDER=mock|aws
│   │   ├── httpapi/          # handlers, middleware (auth, CSRF, rate limit, request log)
│   │   ├── auth/             # argon2id, sessions, magic links
│   │   ├── store/            # sqlc-generated queries + tx helpers (sets RLS context)
│   │   ├── jobs/             # postgres queue: enqueue, claim, retry w/ backoff
│   │   ├── canon/            # pipeline: chunk, extract, resolve, contradict, recap, wiki
│   │   ├── prompts/          # versioned prompt templates (embedded)
│   │   ├── llm/              # Model interface; bedrock.go; mock.go
│   │   ├── embed/            # Embedder interface; bedrock.go; mock.go
│   │   ├── transcribe/       # Service interface; aws.go; mock.go
│   │   ├── blob/             # Store interface; s3.go; localdisk.go
│   │   ├── mail/             # Sender interface; ses.go; mock.go
│   │   ├── billing/          # Provider interface; mock.go (see §9)
│   │   └── metering/         # usage_events, plan limits
│   ├── migrations/           # 0001_init.sql, ...
│   └── testdata/             # fixtures for mocks + eval campaign
└── frontend/                 # pnpm + vite + react + ts
    ├── src/
    │   ├── api/              # generated types + fetch client
    │   ├── routes/           # auth, campaigns, session, review, wiki, ask, share
    │   ├── components/
    │   └── styles/           # tokens.css (light/dark), CSS Modules
    └── vite.config.ts        # proxies /api → Go server in dev
```

---

## 5. Mocks (develop everything with no AWS account)

`PROVIDER=mock` (the default) swaps every AWS client for a local implementation. Each interface gets a **contract test** that runs against both implementations, so switching to `aws` later is a config change, not a rewrite.

| Interface | Mock behavior |
|---|---|
| `llm.Model` | **Fixture mode:** returns recorded responses for the eval campaign, keyed by `(prompt_version, step, input hash)`. **Fallback mode:** schema-valid output from a simple heuristic extractor (capitalized names → entities, sentences → facts), so any input works end to end. Reports fake token usage. Can inject latency and errors to exercise retries. |
| `embed.Embedder` | Deterministic 1024-dim vectors from hashed character n-grams, so similar names land close together and entity matching behaves realistically |
| `transcribe.Service` | Returns the paired `.txt` fixture for a known audio file. Otherwise returns placeholder text sized to the audio length. Async start/poll flow, the same as Transcribe. |
| `blob.Store` | Local `./data/blobs` directory, with presigned-URL-style upload endpoint |
| `mail.Sender` | Logs the message and stores it in memory, viewable at `/dev/mailbox` (dev builds only) |
| `billing.Provider` | Fake checkout page that flips the plan; emits the same webhook events a real processor would |

When the Bedrock account is ready: set `PROVIDER=aws`, set the model IDs in config, run `make eval PROVIDER=aws`, and record new fixtures from the real responses.

---

## 6. Data model (first pass)

```
users(id, email, password_hash, created_at)
auth_sessions(id, user_id, token_hash, expires_at)
magic_links(id, user_id, token_hash, expires_at, used_at)
campaigns(id, owner_id, name, share_token, created_at)
campaign_members(campaign_id, user_id, role: gm|player)
sessions(id, campaign_id, number, played_on, notes, audio_key, transcript, status)
entities(id, campaign_id, type: npc|place|faction|item|thread|pc, name, aliases[], summary,
         visibility: gm|party, search tsvector, name_embedding vector(1024))
facts(id, entity_id, text, source_session_id, source_quote, visibility: gm|party,
      state: proposed|canon|rejected|retconned, embedding vector(1024), prompt_version)
relationships(id, from_entity, to_entity, kind, fact_id)
contradictions(id, new_fact_id, existing_fact_id, explanation, resolution)
recaps(id, session_id, audience: gm|party, body)
jobs(id, kind, payload jsonb, run_at, attempts, max_attempts, locked_until, last_error)
usage_events(id, user_id, campaign_id, kind, tokens_in, tokens_out, audio_seconds, cost_micros)
subscriptions(user_id, plan, status, current_period_end, provider_ref)
```

**Row-level security:** every request runs in a transaction that sets `app.user_id` with `set_config(..., true)`. Policies allow players to `SELECT` only `visibility='party' AND state='canon'`. The API connects as a non-superuser role, so RLS can't be bypassed. The share link uses a dedicated read-only role with the same policy.

---

## 7. The pipeline

Each step is a job. Steps are idempotent (keyed by session + step + prompt version) and retry with backoff. Progress streams to the UI over SSE.

1. **Ingest:** store notes or audio (blob store). Audio is deleted after transcription by default.
2. **Transcribe** (audio only): start, then poll, then save the transcript.
3. **Chunk:** split into overlapping chunks of about 3k tokens.
4. **Extract:** for each chunk, Bedrock Converse with a forced tool call returns strict JSON `{entities[], facts[], relationships[]}`. Go structs validate it, and every fact must include a source quote found verbatim in the chunk, or it's dropped.
5. **Resolve:** match against existing entities by alias, then pgvector similarity on name embeddings, then an LLM tiebreak on close calls. Low confidence → ask the GM in the review queue.
6. **Check contradictions:** compare new facts only against canon for the touched entities (vector top-k, then LLM judgment).
7. **Recap:** GM version from everything; player version from party-visible inputs only.
8. **Propose:** everything is written as `state='proposed'`.
9. **Publish:** when the GM approves, in one transaction: mark facts canon, then enqueue wiki page rebuilds for the affected entities.

Prompts live in `internal/prompts/` with a version number stored on every fact, so a regression can be traced to a prompt change.

---

## 8. API sketch (REST + JSON, cookie auth)

```
POST   /api/auth/signup | /login | /logout | /magic-link | /magic-link/verify
GET    /api/me
GET    /api/campaigns                      POST /api/campaigns
GET    /api/campaigns/{id}                 PATCH/DELETE
POST   /api/campaigns/{id}/sessions        (notes or audio upload ticket)
GET    /api/sessions/{id}                  GET /api/sessions/{id}/events  (SSE)
GET    /api/campaigns/{id}/review          POST /api/review/{proposalId}/{approve|reject|edit}
POST   /api/campaigns/{id}/review/approve-all
GET    /api/campaigns/{id}/entities?type=&q=   GET /api/entities/{id}
POST   /api/campaigns/{id}/ask             POST /api/campaigns/{id}/npc
GET    /api/share/{token}/...              (read-only, party view)
POST   /api/billing/checkout               POST /api/billing/webhook
```

Security baseline: SameSite=Lax cookies plus a CSRF token on mutating requests, per-IP and per-user rate limits (in-process token bucket), request-size limits, an upload MIME/size allowlist, and audit log rows for canon changes.

---

## 9. Billing: the one unavoidable outside provider

Processing cards yourself means PCI compliance, so no one realistically builds that from scratch. The plan keeps billing behind `billing.Provider` with a **mock** for all of development. Before launch, choose a processor (Stripe, or Paddle if you want it to handle sales tax as merchant of record) and write one adapter. Nothing else in the codebase changes.

| Tier | Price | Limits |
|---|---|---|
| Free | $0 | 1 campaign, 3 processed sessions, text only |
| GM | $8/mo ($80/yr) | Unlimited campaigns, ~10 audio hrs/mo, player links |
| GM+ | $15/mo | ~30 audio hrs/mo, co-GMs, Discord bot (later) |

Players are always free.

**Cost watch:** AWS Transcribe is likely the biggest cost, more than Bedrock. A 3–4 hr session is about 30–45k words. Every Bedrock and Transcribe call writes a `usage_events` row with tokens or seconds and an estimated cost. Plan limits are checked before a job runs. Check current Bedrock and Transcribe pricing before setting the caps; target ≥70% margin for heavy users.

---

## 10. Quality: evals from day one

`backend/testdata/eval-campaign/` holds a hand-written 5-session campaign with an answer key: expected entities, facts, visibility, and planted contradictions. `make eval` runs the full pipeline and prints a scorecard. In mock mode it tests the plumbing; once the account exists, `PROVIDER=aws` tests real quality.

| Metric | Target |
|---|---|
| Fact extraction recall | ≥ 85% |
| Wrong entity merges | ≤ 3% |
| False contradiction alarms per session | ≤ 1 |
| **Secret leaks into player recap, wiki or share link** | **0, a hard fail** (also covered by an RLS integration test) |
| Estimated cost per session | reported every run |

---

## 11. Milestones

| When | Deliverable | Done when |
|---|---|---|
| **Week 0** | Landing page + waitlist. Post in GM communities. Run the extraction prompts by hand on 5 real GMs' notes. | 3 of 5 GMs ask to keep using it |
| **Week 1: Foundation** | Monorepo, Makefile, docker compose, migrator, config, slog logging, `/healthz`, OpenAPI skeleton, Vite app shell, lint/test in `make check` | `make dev` serves the React shell talking to the Go API |
| **Week 2: Canon engine** | `llm`/`embed` interfaces + mocks + Bedrock adapters (untested against real AWS), extract → resolve → contradict → recap, `cmd/canon` CLI, eval harness + campaign | `make eval` passes end to end in mock mode |
| **Week 3: Accounts & jobs** | Auth (signup, login, magic link via mail mock), campaigns/sessions API, Postgres job queue + worker, SSE progress, RLS policies + leak tests | Paste notes in the API → proposals appear in the DB |
| **Week 4: Core UI** | Auth pages, campaign list, new session, live pipeline status, review queue | A GM can process a session in the browser |
| **Week 5: Wiki & Ask** | Entity pages with citations, search, Ask the Canon, NPC generator, retcon/undo | Your own campaign runs through it comfortably |
| **Week 6: Players** | Share links, party-only views, invite players as members | Playwright test confirms no GM secret appears in the share view |
| **Week 7: Audio** | Upload flow, blob store, transcribe mock + AWS adapter, chunking, audio retention policy | A long audio fixture → reviewed wiki |
| **Week 8: Launch prep** | Billing interface + mock + plan limits, metering dashboard (admin), onboarding with a sample campaign, Terraform for AWS. Turn on the real Bedrock account and re-run evals. | Private beta with 20–50 GMs |
| **After** | Real payment adapter, Discord bot, Markdown export, relationship graph, timeline | Driven by beta feedback |

---

## 12. Go-to-market

- **Communities:** r/DMAcademy, r/rpg, r/DnD, r/Pathfinder2e, GM Discords. Post helpful "how I keep my campaign consistent" content.
- **Viral loop:** every player share link carries a "Made with ___ — start your campaign free" footer.
- **Free SEO tools:** tavern/NPC/town name generators as standalone pages.
- **Creators:** free GM+ for small TTRPG YouTubers and streamers.
- **Demo:** use your own sample campaign, never transcripts of famous actual-play shows (copyright).

---

## 13. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Bad extraction or merges erode trust | Review queue, verbatim-quote check, citations, undo/retcon, eval suite |
| Mocks drift from real AWS behavior | Contract tests per interface; record real fixtures as soon as the account exists; Week 8 buffer for adapter fixes |
| Building auth/queue/etc. in-house costs time | Keep each small and boring. Use standard primitives (argon2id, SKIP LOCKED). Security-review auth before beta. |
| Audio costs eat margin | Per-tier caps, metering before every job, audio auto-delete |
| Trademark / IP | Never use "D&D" in name or branding. Stay system-agnostic. Get real legal advice before launch. |
| Privacy (recording friends) | Consent checkbox per session, auto-delete audio, account export/delete |
| Competitors bolt on AI | Compete on canon + visibility + review workflow |
| Scope creep | The "NOT in v1" list in §2 is the contract |

---

## 14. Remaining decisions

1. **Name + domain.**
2. **Payment processor** (only needed before launch; mocked until then).
3. **Bedrock models:** a stronger model for extract/resolve/contradict and a cheaper one for recaps/NPCs/Q&A. Model IDs are config values, picked once the account is live.
4. **Bedrock region** (affects which models are available and latency).
