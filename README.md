# Ragmux — self-hosted AI gateway

<https://ragmux.com>

Ragmux is a single-binary AI gateway written in Go. It sits between your applications
and LLM providers, exposes one **OpenAI-compatible API**, and can augment every request
with **retrieval (RAG)** from documents you upload. All state — models, projects,
documents, chunks, vectors and metrics — lives in one **PostgreSQL** database with the
[pgvector](https://github.com/pgvector/pgvector) extension. Two containers, no Redis, no
separate vector database.

```
client  ──►  POST /v1/chat/completions (Bearer sk-proj-…)
             │
             ├─ project → model connection (provider credentials stay server-side)
             ├─ project → RAG store (optional): embed query → top-k chunks → inject context
             └─ adapter: OpenAI · Anthropic · Gemini · DeepSeek · Ollama · custom OpenAI (vLLM…)
                          JSON and SSE streaming, normalised to the OpenAI schema
             │
             └─ PostgreSQL + pgvector: users, connections (AES-256-GCM keys), projects,
                documents (bytea), chunks, HNSW vector indexes, request logs
```

## Features

- **Providers:** `openai`, `anthropic`, `gemini`, `deepseek`, `ollama`, `custom_openai`
  (vLLM, LM Studio, LiteLLM, any OpenAI-compatible server).
- **OpenAI-compatible proxy:** `/v1/chat/completions` (JSON + SSE streaming), `/v1/models`.
  Works with the official OpenAI SDKs by changing `base_url` and `api_key`.
- **Format translation:** Anthropic Messages API and Gemini `generateContent` requests and
  streams are converted to and from the OpenAI schema, including tool calls for Anthropic.
- **RAG stores:** upload PDF / DOCX / HTML / TXT / Markdown, section-aware chunking with
  contextual embeddings, hybrid vector + full-text search (RRF), optional LLM reranking and a
  similarity threshold, context injection into the system prompt. See [Retrieval (RAG)](#retrieval-rag).
- **Projects:** each project has its own `sk-proj-…` key mapped to one model connection and an
  optional RAG store. Provider credentials are AES-256-GCM encrypted at rest and never leave
  the server.
- **Observability:** per-request logs (prompt/completion tokens, latency, status, streaming,
  RAG usage), per-project and global summaries, daily series.
- **Dashboard:** embedded single-page UI at `/admin/` for models, RAG stores, documents,
  projects, metrics, users, the audit log and a playground. Everything is also available
  as a REST API.
- **Users and roles:** `admin`, `editor` and `viewer` accounts, per-project membership,
  login rate limiting with lockout, and an audit trail of every management action.
- **Rate limits and budgets:** per-project requests / tokens per minute and daily / monthly
  token budgets, enforced across replicas with OpenAI-style `429` responses and
  `x-ratelimit-*` headers.
- **Persistence:** everything in PostgreSQL + pgvector; uploaded files are kept as `bytea`
  so no volume is needed on the gateway. Unfinished document ingestion resumes
  automatically after a restart.

## Quick start (Docker Compose)

The Compose file runs the gateway next to a `pgvector/pgvector:pg17` database.

```bash
cp .env.example .env
echo "SECRET_KEY=$(openssl rand -hex 32)" >> .env   # required, see below
docker compose up -d
```

Open <http://localhost:8080/admin/>. On first start an `admin` user is created. If
`ADMIN_PASSWORD` is not set, a random password is printed **once** to the container log:

```bash
docker logs ragmux 2>&1 | grep -A3 "Initial admin"
```

To use an existing PostgreSQL server instead, run only the image and point it at your
database (the `vector` extension is created automatically; the role needs permission to
do that, or create it beforehand with `CREATE EXTENSION vector`):

```bash
docker build -t ragmux/ragmux:latest .
docker run -d -p 8080:8080 --name ragmux \
  -e DATABASE_URL='postgres://ragmux:secret@db.internal:5432/ragmux?sslmode=require' \
  -e SECRET_KEY="$(openssl rand -hex 32)" \
  ragmux/ragmux:latest
```

### Data and persistence

The gateway itself is stateless. Two things make up your state:

1. **The PostgreSQL database** (`pgdata` volume in Compose): users, model connections,
   projects, uploaded documents (stored as `bytea`), chunks, vectors and request logs.
   Back it up with `pg_dump` like any other Postgres database (see
   [Backup & restore](#backup--restore)).
2. **`SECRET_KEY`**: the 32-byte AES-256-GCM key (64 hex characters) that encrypts
   provider API keys inside the database. Generate it once with `openssl rand -hex 32`
   and keep it next to your backups — without it the stored credentials cannot be read.

Stop, remove and recreate the gateway container as often as you like; as long as the
database and `SECRET_KEY` are the same, every model, project, document and metric is
still there. If `SECRET_KEY` is unset the gateway falls back to generating and reading
`DATA_DIR/secret.key` and logs a warning; that is meant for local development only.

### Backup & restore

Everything to back up is the database plus `SECRET_KEY`. The repository ships two
scripts and a scheduled-backup Compose profile; the full operator guide (formats,
PITR, managed Postgres, restore runbook, DR drill) is in
[docs/backup-restore.md](docs/backup-restore.md).

```bash
scripts/backup.sh                                  # pg_dump -Fc via the postgres service -> ./backups/ragmux-<stamp>.dump
scripts/restore.sh --yes backups/ragmux-<stamp>.dump   # stop ragmux, pg_restore --clean, start, wait for /healthz
docker compose --profile backup up -d              # daily dumps into ./backups with 7d/4w/6m retention
```

`GET /admin/api/system` includes a `backup` block (vector table count, document bytes,
last migration time) and `ragmux -version` prints the build version. Restoring an older
dump into a newer Ragmux is fine (missing migrations are applied on start); the reverse is
not. Without the original `SECRET_KEY` the stored provider keys cannot be decrypted and
must be re-entered.

## Configuration

| Variable           | Default      | Description |
|--------------------|--------------|-------------|
| `DATABASE_URL`     | *(required)* | PostgreSQL connection string (`postgres://user:pass@host:5432/db?sslmode=…`) |
| `SECRET_KEY`       | *(file fallback)* | 64 hex chars (32 bytes) encrypting provider credentials; `openssl rand -hex 32` |
| `DB_MAX_CONNS`     | `10`         | Connection pool size |
| `DATA_DIR`         | `/app/data`  | Only used for the `secret.key` fallback when `SECRET_KEY` is unset |
| `PORT`             | `8080`       | HTTP port |
| `ADMIN_USER`       | `admin`      | Username created on first run |
| `ADMIN_PASSWORD`   | *(random)*   | Password for the first-run user |
| `CORS_ORIGINS`     | *(none)*     | Comma-separated allowed browser origins (`*` = all) |
| `LOG_LEVEL`        | `info`       | `debug`, `info`, `warn`, `error` |
| `SESSION_TTL`      | `24h`        | Dashboard session lifetime |
| `UPSTREAM_TIMEOUT` | `5m`         | Timeout for non-streaming provider calls |
| `INGEST_WORKERS`   | `2`          | Parallel document ingestion jobs |
| `MAX_UPLOAD_MB`    | `50`         | Max upload size |
| `SECURE_COOKIES`   | `false`      | Mark the session cookie `Secure` (behind HTTPS) |
| `LOGIN_RATE_LIMIT_PER_MIN` | `10` | Failed logins allowed per minute from one IP address (`0` disables) |
| `LOGIN_USER_LIMIT_PER_MIN` | `5`  | Failed logins allowed per minute for one username (`0` disables) |
| `LOGIN_LOCKOUT_FAILURES`   | `20` | Failures within `LOGIN_LOCKOUT_MINUTES` that lock a username out (`0` disables) |
| `LOGIN_LOCKOUT_MINUTES`    | `15` | Lockout window |

## Using the gateway

### 1. Log in

```bash
TOKEN=$(curl -s localhost:8080/admin/api/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"YOUR_PASSWORD"}' | jq -r .token)
AUTH="Authorization: Bearer $TOKEN"
```

### 2. Add model connections

```bash
# Chat model (Anthropic)
curl -s localhost:8080/admin/api/models -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "claude", "provider_type": "anthropic",
  "api_key": "sk-ant-...", "model_name": "claude-sonnet-4-5"}'

# Embedding model (OpenAI)
curl -s localhost:8080/admin/api/models -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "openai-embed", "provider_type": "openai",
  "api_key": "sk-...", "model_name": "text-embedding-3-small"}'

# Local vLLM / Ollama
curl -s localhost:8080/admin/api/models -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "local-qwen", "provider_type": "custom_openai",
  "base_url": "http://host.docker.internal:8000/v1", "model_name": "qwen2.5-7b"}'
```

`POST /admin/api/models/{id}/test` with `{"mode":"chat"}` or `{"mode":"embedding"}` sends a
ping through the connection.

### 3. Create a RAG store and upload documents

```bash
curl -s localhost:8080/admin/api/rag-stores -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "handbook", "embedding_connection_id": 2,
  "chunk_size": 1000, "chunk_overlap": 200, "top_k": 5,
  "search_mode": "hybrid", "fts_config": "english", "rerank": false, "max_distance": 0}'

curl -s localhost:8080/admin/api/rag-stores/1/documents -H "$AUTH" -F file=@handbook.pdf
curl -s localhost:8080/admin/api/rag-stores/1/documents -H "$AUTH" -F file=@policies.docx -F file=@faq.html
curl -s localhost:8080/admin/api/rag-stores/1/documents -H "$AUTH"      # status: pending → processing → ready

# try retrieval directly (mode / rerank / max_distance override the store settings for this call)
curl -s localhost:8080/admin/api/rag-stores/1/search -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"query":"vacation policy", "top_k": 3, "mode": "hybrid", "rerank": true}'
```

The settings and how retrieval works are described in [Retrieval (RAG)](#retrieval-rag).

### 4. Create a project (get an `sk-proj-…` key)

```bash
curl -s localhost:8080/admin/api/projects -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "support-bot", "model_connection_id": 1, "rag_store_id": 1,
  "system_prompt": "You are the company support assistant."}'
# → {"project":{...},"api_key":"sk-proj-..."}   (shown only once; rotate with POST /projects/{id}/rotate-key)
```

### 5. Call it like OpenAI

```bash
curl localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-proj-..." -H 'Content-Type: application/json' \
  -d '{"model":"default","messages":[{"role":"user","content":"How many vacation days do I get?"}]}'

# streaming
curl -N localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-proj-..." -H 'Content-Type: application/json' \
  -d '{"model":"default","stream":true,"messages":[{"role":"user","content":"Summarise the handbook"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-proj-...")
resp = client.chat.completions.create(
    model="default",   # the project decides the real model; this value is echoed back
    messages=[{"role": "user", "content": "How many vacation days do I get?"}],
    stream=True,
)
for chunk in resp:
    print(chunk.choices[0].delta.content or "", end="")
```

The `model` field is ignored for routing (the project's connection decides) and echoed in
the response so SDKs stay happy. When a RAG store is linked, the last user message is used
as the query, the top-k chunks are retrieved and injected into the system prompt inside a
`<context>` block before the request reaches the provider; the response carries an
`x-ragmux-rag-hits` header with the number of passages injected.

## Retrieval (RAG)

A RAG store is a set of documents embedded with one model connection. Projects link to at
most one store.

### Formats and parsing

| Format | Extensions | What becomes a block |
|--------|------------|----------------------|
| PDF | `.pdf` | paragraphs per page; the page number is kept |
| Word | `.docx` | paragraphs; `Heading 1-9` / `Title` styles form the section path; table rows become `cell | cell` lines |
| HTML | `.html`, `.htm` | `p`, `li`, `td`, `pre`, `blockquote`, `div`…; `h1`-`h3` form the section path; `script`, `style`, `nav`, `header`, `footer`, `svg` are dropped; `<title>` is the document title |
| Markdown | `.md`, `.markdown` | paragraphs; `#` headings form the section path |
| Text | `.txt` | paragraphs |

No external tools are needed: DOCX is read from `word/document.xml`, HTML with
`golang.org/x/net/html`. Uploads are checked by extension and, for DOCX, by the zip
signature. Scanned PDFs without a text layer are rejected.

### Chunking and contextual chunks

Blocks are packed into chunks of `chunk_size` characters with `chunk_overlap` characters
carried over between consecutive chunks. A chunk never spans two sections or two pages, so
every chunk has exactly one section path (the last two heading levels, e.g.
`Install > Docker`) and page. Blocks larger than `chunk_size` are split on sentence or word
boundaries.

With `contextual_chunks` (default on) the text that is *embedded* is
`<filename> · <section>` followed by the chunk content, so the vector reflects where the
passage sits in the document; the stored content and the context sent to the model stay
unchanged. Changing `chunk_size`, `chunk_overlap` or `contextual_chunks` on a store with
documents returns `"reprocess_recommended": true` — use **Reprocess all**
(`POST /rag-stores/{id}/reprocess`) to re-parse, re-chunk and re-embed every document.

### Search modes

- `vector` — cosine nearest neighbours on the pgvector HNSW index.
- `hybrid` (default) — the vector top-N and a PostgreSQL full-text top-N are fused with
  reciprocal rank fusion: `score = 1/(60+vector_rank) + 1/(60+fts_rank)`. A chunk that is
  close in embedding space *and* contains the query terms ranks first; an exact product name,
  error code or identifier the embedding model does not know still surfaces through the
  full-text side. Hybrid retrieves `3 × top_k` candidates before fusing.

Full-text indexing always uses the `simple` configuration (language-agnostic, no stemming,
no stop words) because the index column is generated once per chunk. `fts_config` only
selects the configuration used to parse the *query* (`websearch_to_tsquery`), which makes a
difference when it drops stop words or stems: with `english`, "policies" is looked up as
`polici` — which will not match an index built with `simple`. Keep `fts_config = simple`
unless you know your corpus benefits; any configuration listed in `pg_ts_config` is accepted.
Plain queries are OR-ed (chunks matching more terms rank higher); quoted phrases, `or` and
`-term` follow the `websearch_to_tsquery` syntax and are passed through unchanged.

`max_distance` (0-2, default 0 = off) drops every candidate whose cosine distance to the
query exceeds it, in both modes and before reranking. Use it to keep unrelated passages out
of the prompt when a question has no answer in the store; the right value depends on the
embedding model (try the dashboard's search test: it shows the distance of every hit).

### Reranking

With `rerank` on, the top `rerank_candidates` (default 15) fused hits are sent to the
project's chat model in one listwise prompt (each passage cut to ~800 characters,
`temperature 0`, `max_tokens 200`); the model returns the passage numbers ordered by
relevance and the result is cut to `top_k`. Passages the model omits are appended in their
original order, so nothing is lost. This costs one extra model call per request (roughly
`rerank_candidates × chunk_size / 4` prompt tokens) and adds its latency; failures and
timeouts (10 s) fall back to the fused order and are logged at warn level. From the admin
search endpoint reranking uses the chat model of the first project linked to the store.

### Response headers and context format

`x-ragmux-rag-hits: <n>` is set on every chat response of a project with a linked store
(`0` when nothing matched; absent when the project has no store). Passages are injected as

```
[1] (handbook.pdf · Leave > Vacation · p.12)
…chunk text…
```

so the model can cite `[n]`; the label carries only the parts that exist.

### Search endpoint

`POST /admin/api/rag-stores/{id}/search` takes `{query, top_k, mode, rerank, max_distance}`
(everything but `query` optional; unset fields use the store settings) and returns

```json
{"mode":"hybrid","reranked":true,"latency_ms":412,
 "hits":[{"chunk_id":8,"document_id":2,"filename":"handbook.pdf","index":3,"section":"Leave > Vacation","page":12,
          "content":"…","distance":0.18,"score":0.0325,"vector_rank":1,"fts_rank":2}]}
```

`vector_rank` / `fts_rank` are 0 when the chunk was not a candidate on that side.

## Users, roles and projects

The first-run account is an `admin`. Admins create further users in the dashboard
(**Users** tab) or via the API. Every user has one role:

| Role     | Model connections, RAG stores, documents | Projects | Users, audit log |
|----------|------------------------------------------|----------|------------------|
| `admin`  | full access | all projects, all metrics | full access |
| `editor` | create, edit, delete, upload, test, search | create (becomes a member); read, edit, delete, rotate key, metrics and members only for projects it belongs to | — |
| `viewer` | read, test and search only | read and metrics only for projects it belongs to | — |

Projects have a member list (`member_ids`). Non-admins only see projects they are a member
of; any other project id answers `404`, and the global metrics endpoints are limited to
their projects. Editors keep themselves in the member list of projects they manage.
Writes that the role does not allow answer `403 {"error":{"type":"forbidden"}}`.

Deactivated users cannot sign in and their existing sessions stop working immediately.
The last active admin cannot be demoted or deactivated, and nobody can deactivate or
delete their own account.

### Login protection

Every login attempt is recorded in the database (so all replicas share the counters).
After `LOGIN_USER_LIMIT_PER_MIN` failures for a username or `LOGIN_RATE_LIMIT_PER_MIN`
failures from an IP within a minute, and after `LOGIN_LOCKOUT_FAILURES` failures for a
username within `LOGIN_LOCKOUT_MINUTES`, `/admin/api/login` answers
`429 {"error":{"type":"rate_limited"}}` with a `Retry-After` header. Successful logins do
not reset the counters; the windows simply expire. Attempts older than 24 hours are purged
hourly. The client IP is taken from `X-Forwarded-For` / `X-Real-IP` when present, so run
the gateway behind a proxy that sets them or make sure clients cannot spoof them.

### Audit log

Logins (success, failure, lockout), logouts, password changes and every create, update,
delete, key rotation, upload, reprocess, member change and connection test are written to
`audit_logs` with the actor, target, IP and a small JSON `details` object that never
contains credentials. Admins read it with
`GET /admin/api/audit?limit=100&action=project.&actor_user_id=1&before=2026-09-18T10:00:00Z`
(`action` is a prefix match, `before` pages backwards).

## Rate limits & budgets

Every project has four optional limits (0 = unlimited), set at creation or with
`PUT /projects/{id}` and shown in the dashboard's project form:

| Field | Meaning |
|---|---|
| `rate_limit_rpm` | Requests per UTC minute |
| `rate_limit_tpm` | Prompt + completion tokens per UTC minute |
| `budget_daily_tokens` | Prompt + completion tokens per UTC day |
| `budget_monthly_tokens` | Prompt + completion tokens per UTC calendar month |

Counters live in the `project_usage` table, so several gateway replicas share them. All
windows are aligned to UTC boundaries (`date_trunc` of minute, day and month), not sliding.
When a limit is hit `/v1/chat/completions` answers

```json
HTTP 429  Retry-After: 42
{"error":{"message":"Rate limit reached: 2 requests per minute for this project. Retry after 42 seconds.",
          "type":"rate_limit_exceeded","code":"rate_limit_rpm"}}
```

`type` is `rate_limit_exceeded` for the per-minute limits and `insufficient_quota` for
budgets; `code` is one of `rate_limit_rpm`, `rate_limit_tpm`, `budget_daily`,
`budget_monthly`. `Retry-After` is the number of seconds until the violated window resets
(next minute, next UTC day or next month). Rejected requests are recorded in the request
log with status 429 (visible as `rate_limited` in the metrics summaries and the
dashboard) and do not consume tokens.

Response headers on every chat completion (only for limits that are set):

| Header | Value |
|---|---|
| `x-ratelimit-limit-requests` | configured requests per minute |
| `x-ratelimit-remaining-requests` | requests left in the current minute |
| `x-ratelimit-reset-requests` | integer seconds until the minute window resets |
| `x-ragmux-budget-daily-remaining` | tokens left in today's budget |
| `x-ragmux-budget-monthly-remaining` | tokens left in this month's budget |

How the checks work: the requests-per-minute slot is reserved atomically **before** the
upstream call, so concurrent requests cannot exceed the limit; a rejected reservation is
released again so `remaining` stays accurate. Tokens are only known after the provider
answers, so the token limit and the budgets are checked against what has been recorded so
far. For the per-minute token limit the request's own prompt is estimated at four
characters per token (client messages plus the project system prompt, before RAG context
is added); budgets use recorded usage only. A single request can therefore overshoot a
budget by its own size — the *next* request is refused. Providers that do not report
usage fall back to the same character estimate.

`GET /admin/api/projects/{id}/usage` returns the live counters:

```json
{"project_id":3,"generated_at":"2026-09-18T12:00:30Z",
 "minute":{"period_start":"…","resets_at":"…","requests":2,"tokens":24,"request_limit":2,"request_percent":100,"token_limit":0,"token_percent":0, …},
 "day":{"…":"…","tokens":12,"token_limit":5,"token_percent":100,"resets_at":"2026-09-19T00:00:00Z"},
 "month":{"…":"…"}}
```

Minute rows are purged after two hours, day rows after 400 days and month rows after
three years by the hourly retention job.

## REST API summary

All management endpoints are under `/admin/api` and need a session (cookie or
`Authorization: Bearer <token>` from `/admin/api/login`). The **Role** column is the
minimum role; `member` means the project membership rule above applies too.

| Method | Path | Role | Purpose |
|---|---|---|---|
| POST | `/login`, `/logout` | — | Session management (login returns `token` and `user` with `role`) |
| GET | `/me` | viewer | Current user (`role`, `is_active`, `last_login_at`) |
| POST | `/me/password` | viewer | Change own password `{current_password, new_password}` |
| GET / POST | `/models` | viewer / editor | List / create model connections |
| GET / PUT / DELETE | `/models/{id}` | viewer / editor / editor | Read / update / delete |
| POST | `/models/{id}/test` | viewer | Ping the provider |
| GET / POST | `/rag-stores` | viewer / editor | List / create RAG stores |
| GET / PUT / DELETE | `/rag-stores/{id}` | viewer / editor / editor | Read / update / delete (cascades documents + vectors) |
| GET / POST | `/rag-stores/{id}/documents` | viewer / editor | List / upload (`multipart`, field `file`) |
| POST | `/rag-stores/{id}/search` | viewer | Retrieval test `{query, top_k, mode, rerank, max_distance}` |
| POST | `/rag-stores/{id}/reprocess` | editor | Re-parse, re-chunk and re-embed every document |
| GET / DELETE | `/documents/{id}` | viewer / editor | Document status / delete |
| POST | `/documents/{id}/reprocess` | editor | Re-chunk and re-embed |
| GET / POST | `/projects` | member / editor | List own projects (admin: all) / create (`member_user_ids` optional, returns key once) |
| GET / PUT / DELETE | `/projects/{id}` | member (+editor for writes) | Read / update / delete (`rate_limit_rpm`, `rate_limit_tpm`, `budget_daily_tokens`, `budget_monthly_tokens`) |
| POST | `/projects/{id}/rotate-key` | member + editor | Issue a new key |
| GET / PUT | `/projects/{id}/members` | member (+editor for PUT) | List / replace members `{"user_ids":[...]}` |
| GET | `/projects/{id}/metrics?window=24h` | member | Summary, daily series, recent requests |
| GET | `/projects/{id}/usage` | member | Current minute / day / month counters against the project's limits |
| GET | `/metrics/summary`, `/metrics/requests` | viewer | Metrics over the projects the user can see |
| GET | `/users/lite` | editor | `{id, username, role}` of active users (for member pickers) |
| GET / POST | `/users` | admin | List / create users `{username, password, role}` |
| GET / PUT / DELETE | `/users/{id}` | admin | Read / update `{role, is_active}` / delete |
| POST | `/users/{id}/reset-password` | admin | `{new_password}`; revokes the user's sessions |
| POST | `/users/{id}/sessions/revoke` | admin | Sign the user out everywhere |
| GET | `/audit` | admin | Audit log (`limit`, `action`, `actor_user_id`, `before`) |
| GET | `/system` | viewer | Postgres / pgvector / migration versions, DB size, `backup` sizing, key source, version |

Public: `GET /healthz`. Client API: `POST /v1/chat/completions`, `GET /v1/models`.

## Development

```bash
make dev-db               # pgvector Postgres on localhost:5433 (docker-compose.dev.yml)
make test                 # unit + end-to-end tests (mock upstream, real Postgres)
make run                  # runs on :8080 against the dev database
make docker-build
make backup               # scripts/backup.sh against the compose stack
make restore FILE=backups/ragmux-<stamp>.dump YES=1   # scripts/restore.sh (without YES=1: plan only)
```

Requires Go 1.27+ and Docker for the database. Tests read `TEST_DATABASE_URL` (the
Makefile defaults it to `postgres://ragmux:ragmux@localhost:5433/ragmux_test?sslmode=disable`)
and create a throwaway schema per test, so they can run in parallel against one server;
they are skipped when the variable is unset. The binary is pure Go (`CGO_ENABLED=0`, `pgx`),
so the image is a single static executable on a distroless base.

Schema changes are embedded SQL files in `internal/store/migrations/` applied at startup
under an advisory lock, so several replicas can start against the same database. Each
embedding width gets its own `chunk_embeddings_<dims>` table with an HNSW cosine index,
created on first ingest.

## Layout

```
cmd/ragmux/          entrypoint, HTTP server, admin bootstrap
internal/config/     environment configuration
internal/store/      PostgreSQL schema, migrations, encryption, pgvector search
internal/testdb/     per-test schemas on TEST_DATABASE_URL
internal/provider/   OpenAI-compatible, Anthropic, Gemini adapters + embedders
internal/rag/        parsing (PDF/DOCX/HTML/TXT/MD), chunking, ingestion worker, retrieval, reranking
internal/gateway/    /v1 proxy, RAG injection, metrics
internal/limits/     per-project rate limits, token budgets, usage counters
internal/admin/      /admin REST API + dashboard hosting
web/index.html       dashboard (vanilla JS, embedded in the binary)
```

## Roadmap / not yet

Gemini tool calling, prompt caching passthrough.

## License

Ragmux is free software licensed under the **GNU Affero General Public License v3.0 or later**
(AGPL-3.0-or-later). See [LICENSE](LICENSE). If you run a modified version as a network service,
you must make the modified source available to its users.
