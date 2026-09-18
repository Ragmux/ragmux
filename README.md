# Ragmux — self-hosted AI gateway

<https://ragmux.com>

Ragmux is a single-binary, single-container AI gateway written in Go. It sits between
your applications and LLM providers, exposes one **OpenAI-compatible API**, and can
augment every request with **retrieval (RAG)** from documents you upload. All state
lives in one directory (`/app/data`) backed by embedded SQLite + `sqlite-vec`, so the
whole thing persists with a single volume mount. No Postgres, no Redis, no external
vector database.

```
client  ──►  POST /v1/chat/completions (Bearer sk-proj-…)
             │
             ├─ project → model connection (provider credentials stay server-side)
             ├─ project → RAG store (optional): embed query → top-k chunks → inject context
             └─ adapter: OpenAI · Anthropic · Gemini · DeepSeek · Ollama · custom OpenAI (vLLM…)
                          JSON and SSE streaming, normalised to the OpenAI schema
```

## Features

- **Providers:** `openai`, `anthropic`, `gemini`, `deepseek`, `ollama`, `custom_openai`
  (vLLM, LM Studio, LiteLLM, any OpenAI-compatible server).
- **OpenAI-compatible proxy:** `/v1/chat/completions` (JSON + SSE streaming), `/v1/models`.
  Works with the official OpenAI SDKs by changing `base_url` and `api_key`.
- **Format translation:** Anthropic Messages API and Gemini `generateContent` requests and
  streams are converted to and from the OpenAI schema, including tool calls for Anthropic.
- **RAG stores:** upload PDF / TXT / Markdown, automatic chunking (size/overlap), batch
  embedding, vector search with `sqlite-vec` (cosine), context injection into the system prompt.
- **Projects:** each project has its own `sk-proj-…` key mapped to one model connection and an
  optional RAG store. Provider credentials are AES-256-GCM encrypted at rest and never leave
  the server.
- **Observability:** per-request logs (prompt/completion tokens, latency, status, streaming,
  RAG usage), per-project and global summaries, daily series.
- **Dashboard:** embedded single-page UI at `/admin/` for models, RAG stores, documents,
  projects, metrics and a playground. Everything is also available as a REST API.
- **Persistence:** SQLite (WAL) + vectors + uploads + encryption key under `DATA_DIR`.
  Unfinished document ingestion resumes automatically after a restart.

## Quick start (Docker)

```bash
docker build -t ragmux/ragmux:latest .
docker run -d -p 8080:8080 -v ./gateway_data:/app/data --name ragmux ragmux/ragmux:latest
```

Or with Compose:

```bash
cp .env.example .env      # optional: set ADMIN_PASSWORD etc.
docker compose up -d
```

Open <http://localhost:8080/admin/>. On first start an `admin` user is created. If
`ADMIN_PASSWORD` is not set, a random password is printed **once** to the container log:

```bash
docker logs ragmux 2>&1 | grep -A3 "Initial admin"
```

Everything the gateway writes goes to `./gateway_data` on the host:

```
gateway_data/
├── ragmux.db (+ -wal, -shm)   relational data, chunks and vectors
├── secret.key                 AES key for provider credentials (keep with the DB!)
└── uploads/<doc id>/<file>    original documents
```

Stop, remove and recreate the container with the same mount and every model, project,
document and metric is still there.

> **Permissions.** The image runs as the distroless `nonroot` user (UID 65532). Docker
> creates a missing bind-mount directory as root, which the process cannot write to. If
> you see a startup error about the data directory, run
> `mkdir -p gateway_data && sudo chown 65532:65532 gateway_data`, or add
> `--user $(id -u):$(id -g)` / `user: "1000:1000"` in Compose to run as yourself.

## Configuration

| Variable           | Default      | Description |
|--------------------|--------------|-------------|
| `DATA_DIR`         | `/app/data`  | Directory for DB, uploads and key |
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
| GET | `/system` | Data dir, DB size, vector engine, version |

Public: `GET /healthz`. Client API: `POST /v1/chat/completions`, `GET /v1/models`.

## Development

```bash
make test                 # unit + end-to-end tests (mock upstream, real sqlite-vec)
make run                  # runs on :8080 with DATA_DIR=./data
make docker-build
```

Requires Go 1.27+. The binary is fully static (`CGO_ENABLED=0`): SQLite and `sqlite-vec`
run as a wasm module via `github.com/ncruces/go-sqlite3`. The `go-sqlite3` version is pinned
to `v0.23.x` because the `sqlite-vec` wasm bindings target its wazero-based runtime; newer
`go-sqlite3` releases (v0.33+) switched to a different execution model and cannot load the
extension. If the extension ever fails to load, the gateway logs a warning and falls back to
an exact brute-force cosine search over the stored embeddings, so retrieval keeps working.

## Layout

```
cmd/ragmux/          entrypoint, HTTP server, admin bootstrap
internal/config/     environment configuration
internal/store/      SQLite schema, migrations, encryption, vector search
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
