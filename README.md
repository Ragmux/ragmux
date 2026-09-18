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
- **RAG stores:** upload PDF / TXT / Markdown, automatic chunking (size/overlap), batch
  embedding, pgvector HNSW search (cosine), context injection into the system prompt.
- **Projects:** each project has its own `sk-proj-…` key mapped to one model connection and an
  optional RAG store. Provider credentials are AES-256-GCM encrypted at rest and never leave
  the server.
- **Observability:** per-request logs (prompt/completion tokens, latency, status, streaming,
  RAG usage), per-project and global summaries, daily series.
- **Dashboard:** embedded single-page UI at `/admin/` for models, RAG stores, documents,
  projects, metrics and a playground. Everything is also available as a REST API.
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
   Back it up with `pg_dump` like any other Postgres database.
2. **`SECRET_KEY`**: the 32-byte AES-256-GCM key (64 hex characters) that encrypts
   provider API keys inside the database. Generate it once with `openssl rand -hex 32`
   and keep it next to your backups — without it the stored credentials cannot be read.

Stop, remove and recreate the gateway container as often as you like; as long as the
database and `SECRET_KEY` are the same, every model, project, document and metric is
still there. If `SECRET_KEY` is unset the gateway falls back to generating and reading
`DATA_DIR/secret.key` and logs a warning; that is meant for local development only.

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
  "chunk_size": 1000, "chunk_overlap": 200, "top_k": 5}'

curl -s localhost:8080/admin/api/rag-stores/1/documents -H "$AUTH" -F file=@handbook.pdf
curl -s localhost:8080/admin/api/rag-stores/1/documents -H "$AUTH"      # status: pending → processing → ready

# try retrieval directly
curl -s localhost:8080/admin/api/rag-stores/1/search -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"query":"vacation policy"}'
```

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
the response so SDKs stay happy. When a RAG store is linked, the last user message is
embedded, the top-k chunks are fetched and injected into the system prompt inside a
`<context>` block before the request reaches the provider.

## REST API summary

All management endpoints are under `/admin/api` and need a session (cookie or
`Authorization: Bearer <token>` from `/admin/api/login`).

| Method | Path | Purpose |
|---|---|---|
| POST | `/login`, `/logout` | Session management |
| GET / POST | `/models` | List / create model connections |
| GET / PUT / DELETE | `/models/{id}` | Read / update / delete |
| POST | `/models/{id}/test` | Ping the provider |
| GET / POST | `/rag-stores` | List / create RAG stores |
| GET / PUT / DELETE | `/rag-stores/{id}` | Read / update / delete (cascades documents + vectors) |
| GET / POST | `/rag-stores/{id}/documents` | List / upload (`multipart`, field `file`) |
| POST | `/rag-stores/{id}/search` | Vector search `{query, top_k}` |
| GET / DELETE | `/documents/{id}` | Document status / delete |
| POST | `/documents/{id}/reprocess` | Re-chunk and re-embed |
| GET / POST | `/projects` | List / create (returns key once) |
| GET / PUT / DELETE | `/projects/{id}` | Read / update / delete |
| POST | `/projects/{id}/rotate-key` | Issue a new key |
| GET | `/projects/{id}/metrics?window=24h` | Summary, daily series, recent requests |
| GET | `/metrics/summary`, `/metrics/requests` | Global metrics |
| GET | `/system` | Postgres / pgvector / migration versions, DB size, key source, version |

Public: `GET /healthz`. Client API: `POST /v1/chat/completions`, `GET /v1/models`.

## Development

```bash
make dev-db               # pgvector Postgres on localhost:5433 (docker-compose.dev.yml)
make test                 # unit + end-to-end tests (mock upstream, real Postgres)
make run                  # runs on :8080 against the dev database
make docker-build
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
internal/rag/        parsing (PDF/TXT/MD), chunking, ingestion worker, retrieval
internal/gateway/    /v1 proxy, RAG injection, metrics
internal/admin/      /admin REST API + dashboard hosting
web/index.html       dashboard (vanilla JS, embedded in the binary)
```

## Roadmap / not yet

Multi-user roles, per-project rate limits, Gemini tool calling, reranking, DOCX/HTML
ingestion, prompt caching passthrough.

## License

Ragmux is free software licensed under the **GNU Affero General Public License v3.0 or later**
(AGPL-3.0-or-later). See [LICENSE](LICENSE). If you run a modified version as a network service,
you must make the modified source available to its users.
