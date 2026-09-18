# Ragmux — self-hosted AI gateway

<https://ragmux.com>

Ragmux is a single-binary AI gateway written in Go. It sits between your applications
and LLM providers, exposes one **OpenAI-compatible API**, and can augment every request
with **retrieval (RAG)** from documents you upload. All state — users, model
connections, projects, documents, chunks, vectors and metrics — lives in one
**PostgreSQL** database with the [pgvector](https://github.com/pgvector/pgvector)
extension. Two containers, no Redis, no separate vector database.

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

- **Providers:** `openai`, `anthropic`, `gemini`, `deepseek`, `ollama` (native `/api/chat`)
  and `custom_openai` for vLLM, LM Studio, LiteLLM or any OpenAI-compatible server.
- **OpenAI-compatible proxy:** `/v1/chat/completions` (JSON + SSE streaming) and
  `/v1/models`; works with the official SDKs by changing `base_url` and `api_key`.
  Anthropic and Gemini requests and streams are translated, including tool calls for Anthropic.
- **RAG stores:** PDF / DOCX / HTML / TXT / Markdown, section-aware chunking with contextual
  embeddings, hybrid vector + full-text search (RRF), optional LLM reranking, distance threshold.
- **Projects:** one `sk-proj-…` key per project mapped to a model connection, a system prompt
  and an optional RAG store; provider credentials are encrypted at rest and never exposed.
- **Users and roles:** `admin`, `editor`, `viewer`, project membership, login rate limiting
  with lockout, audit log of every management action.
- **Rate limits and budgets:** per-project requests / tokens per minute and daily / monthly
  token budgets shared across replicas, OpenAI-style `429` and `x-ratelimit-*` headers.
- **Observability:** per-request logs (tokens, latency, status, streaming, RAG use),
  summaries, daily series, configurable retention.
- **Dashboard:** embedded UI at `/admin/` for models, RAG stores, documents, projects,
  metrics, users, audit log and a playground; everything is also a REST API.

## Quick start

```bash
cp .env.example .env
echo "SECRET_KEY=$(openssl rand -hex 32)" >> .env   # encrypts provider keys; keep it with your backups
docker compose up -d
```

Open <http://localhost:8080/admin/>. On first start an `admin` user is created; if
`ADMIN_PASSWORD` is not set in `.env`, a random password is printed **once**:

```bash
docker logs ragmux 2>&1 | grep -A3 "Initial admin"
```

The Compose file runs the gateway next to a `pgvector/pgvector:pg17` database. To use
your own PostgreSQL instead, run the image alone with `DATABASE_URL` and `SECRET_KEY`
set (the `vector` extension is created automatically when the role may do so). Prebuilt
images are published as `ghcr.io/ragmux/ragmux`.

**Where state lives.** The gateway container is stateless. Your state is the PostgreSQL
database (`pgdata` volume) plus `SECRET_KEY`; keep both and you can rebuild the gateway
anywhere. Back up with `scripts/backup.sh` (`pg_dump -Fc`) or the scheduled `backup`
Compose profile — see [Backup and restore](docs/backup-restore.md).

## Using the gateway

**1. Log in** and keep the session token:

```bash
TOKEN=$(curl -s localhost:8080/admin/api/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"YOUR_PASSWORD"}' | jq -r .token)
AUTH="Authorization: Bearer $TOKEN"
```

**2. Add model connections** (a chat model and, for RAG, an embedding model):

```bash
curl -s localhost:8080/admin/api/models -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "claude", "provider_type": "anthropic", "api_key": "sk-ant-...", "model_name": "claude-sonnet-4-5"}'
curl -s localhost:8080/admin/api/models -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "openai-embed", "provider_type": "openai", "api_key": "sk-...", "model_name": "text-embedding-3-small"}'
```

**3. Create a RAG store and upload documents** (optional):

```bash
curl -s localhost:8080/admin/api/rag-stores -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "handbook", "embedding_connection_id": 2}'
curl -s localhost:8080/admin/api/rag-stores/1/documents -H "$AUTH" -F file=@handbook.pdf
curl -s localhost:8080/admin/api/rag-stores/1/documents -H "$AUTH"    # status: pending → processing → ready
```

**4. Create a project** and get its `sk-proj-…` key (shown only once):

```bash
curl -s localhost:8080/admin/api/projects -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "support-bot", "model_connection_id": 1, "rag_store_id": 1,
  "system_prompt": "You are the company support assistant."}'
```

**5. Call it like OpenAI.** The `model` field is echoed back but the project decides the
real model; retrieved passages are injected into the system prompt and counted in the
`x-ragmux-rag-hits` header.

```bash
curl -N localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-proj-..." -H 'Content-Type: application/json' \
  -d '{"model":"default","stream":true,"messages":[{"role":"user","content":"How many vacation days do I get?"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-proj-...")
resp = client.chat.completions.create(model="default", stream=True,
    messages=[{"role": "user", "content": "How many vacation days do I get?"}])
for chunk in resp:
    print(chunk.choices[0].delta.content or "", end="")
```

## Documentation

| Page | Contents |
|---|---|
| [Configuration](docs/configuration.md) | every environment variable and default, `-healthcheck` / `-version` flags, Compose variables, reverse-proxy notes |
| [API reference](docs/api.md) | authentication, roles per endpoint, request/response examples, error envelope, the `/v1` surface, `/healthz`, `/admin/api/system` |
| [Retrieval (RAG)](docs/rag.md) | supported formats, chunking and contextual chunks, hybrid search and `fts_config`, `max_distance`, reranking, context format |
| [Users, roles and limits](docs/users-and-limits.md) | roles matrix, project membership, login protection, audit log, rate limits and budgets, metrics and retention |
| [Providers](docs/providers.md) | provider types and endpoints, Anthropic / Gemini / Ollama translation details, request passthrough, model echo |
| [Backup and restore](docs/backup-restore.md) | what to back up, `scripts/backup.sh` and `scripts/restore.sh`, scheduled backups, PITR, restore runbook |
| [Changelog](CHANGELOG.md) | release notes |

## Development

Requires Go 1.27+ and Docker for the database.

```bash
make dev-db               # pgvector Postgres on localhost:5433 (docker-compose.dev.yml)
make test                 # unit + end-to-end tests (mock upstream, real Postgres)
make run                  # builds and runs on :8080 against the dev database
make docker-build         # local image, VERSION from git describe
make backup               # scripts/backup.sh against the compose stack
make restore FILE=backups/ragmux-<stamp>.dump YES=1   # without YES=1: plan only
```

Tests read `TEST_DATABASE_URL` (the Makefile defaults it to
`postgres://ragmux:ragmux@localhost:5433/ragmux_test?sslmode=disable`) and create a
throwaway schema per test, so they run in parallel against one server; they are skipped
when the variable is unset. CI also runs `gofmt`, `go vet`, `golangci-lint run ./...`
(v2, config in `.golangci.yml`) and `govulncheck`. The binary is pure Go
(`CGO_ENABLED=0`, `pgx`), shipped as a static executable on a distroless base. Schema
changes are embedded SQL files in `internal/store/migrations/` applied at startup under
an advisory lock.

```
cmd/ragmux/           entrypoint, HTTP server, admin bootstrap
internal/config/      environment configuration
internal/store/       PostgreSQL schema, migrations, encryption, pgvector search
internal/provider/    OpenAI-compatible, Anthropic, Gemini, Ollama adapters + embedders
internal/rag/         parsing, chunking, ingestion worker, retrieval, reranking
internal/gateway/     /v1 proxy, RAG injection, metrics
internal/limits/      per-project rate limits, token budgets, usage counters
internal/maintenance/ hourly retention job
internal/admin/       /admin REST API + dashboard hosting
web/index.html        dashboard (vanilla JS, embedded in the binary)
```

## Roadmap

Not in this release: an import tool for 0.1 (pre-PostgreSQL) databases, Gemini tool calling,
OCR for scanned PDFs, a Prometheus metrics endpoint, SSO / OIDC login, prompt caching
passthrough.

## License

Ragmux is free software licensed under the **GNU Affero General Public License v3.0 or
later** (AGPL-3.0-or-later); see [LICENSE](LICENSE). The AGPL's network-service clause
applies: if you run a modified version as a service that users interact with over a
network, you must offer those users the corresponding source of your modified version.

Website and further material: <https://ragmux.com>
