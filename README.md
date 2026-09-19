# Ragmux — self-hosted AI gateway

<https://ragmux.com>

Ragmux is a single-binary AI gateway written in Go. It sits between your applications
and LLM providers, exposes one **OpenAI-compatible API**, and can augment every request
with **retrieval (RAG)** from documents you upload. All state — users, model
connections, projects, documents, chunks, vectors and metrics — lives in one
**PostgreSQL** database with the [pgvector](https://github.com/pgvector/pgvector)
extension. One container, no Redis, no separate vector database.

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
  Anthropic and Gemini requests and streams are translated, tool calls included, and remote
  images are fetched for the providers that cannot follow a URL themselves.
- **RAG stores:** PDF / DOCX / HTML / TXT / Markdown, section-aware chunking with contextual
  embeddings, hybrid search fusing pgvector with either Postgres full-text ranking or
  ParadeDB BM25, reranking by your own model or the Cohere / Voyage APIs, distance threshold.
- **Projects:** one `sk-proj-…` key per project mapped to a model connection, a system prompt
  and an optional RAG store; provider credentials are encrypted at rest and never exposed.
- **API keys:** per-user `sk-user-…` keys for `/v1` scoped to the projects you grant them and
  `sk-mgmt-…` keys for the management API, both with scopes, expiry, revocation and optional
  limits under the project's.
- **Users and roles:** `admin`, `editor`, `viewer`, project membership, login rate limiting
  with lockout, audit log of every management action, and `ragmux reset-password` when the
  last administrator is locked out.
- **Cost:** prompt-caching passthrough, cache-aware token accounting and an editable price
  table that estimates what each request cost.
- **Rate limits and budgets:** per-project requests / tokens per minute and daily / monthly
  token budgets shared across replicas, OpenAI-style `429` and `x-ratelimit-*` headers.
- **Observability:** per-request logs (tokens, latency, status, streaming, RAG use, cost),
  summaries, daily series, configurable retention, an authenticated Prometheus `/metrics`
  endpoint and OpenTelemetry traces over OTLP.
- **Scale:** run several replicas against one PostgreSQL — ingestion is claimed under a
  lease, the retention pass elects a leader, and streaming needs no session affinity.
- **Dashboard:** embedded UI at `/admin/` for models, RAG stores, documents, projects,
  metrics, users, audit log and a playground; everything is also a REST API.

## Quick start

Nothing to clone — one container with the gateway and its own PostgreSQL 17 + pgvector
server inside:

```bash
docker run -d --name ragmux \
  -p 127.0.0.1:8765:8765 \
  -e SECRET_KEY="$(openssl rand -hex 32)" \
  -v ragmux-data:/data \
  ragmux/ragmux:latest
```

Open <http://localhost:8765/admin/>. On a fresh database the dashboard asks you to
**create the first administrator** (username and a password of at least 12 characters);
that form only works while no user exists. For unattended installs pass
`-e ADMIN_USER=admin -e ADMIN_PASSWORD=...` instead and the account is created on first
start (see [Configuration](docs/configuration.md#environment-variables)).

Two things worth knowing about that command:

- **`SECRET_KEY` encrypts the provider credentials.** Nothing is required to start, but
  without it the gateway generates `/data/ragmux/secret.key` inside the volume, which you
  then have to back up together with the database. Keep the key with your backups.
- **The port is published on loopback only.** Put a TLS-terminating reverse proxy in
  front for network access and set `TRUST_PROXY_HEADERS` / `SECURE_COOKIES`; see
  [Configuration](docs/configuration.md#behind-a-reverse-proxy). Change the mapping to
  `-p 8765:8765` only deliberately.

Everything persistent is under `/data`: `/data/pg` is the PostgreSQL cluster (`PGDATA`)
and `/data/ragmux` holds the `secret.key` fallback (`DATA_DIR`). That volume plus
`SECRET_KEY` is the whole state. The embedded PostgreSQL has no TCP listener at all, so
`8765` is the only port.

Running models locally? Provider URLs on private networks (Ollama on the Docker host, a
vLLM service on the same Docker network) are refused by default as an SSRF guard. Allow
them by hostname before you add the connection — add
`-e PRIVATE_UPSTREAM_ALLOWLIST=host.docker.internal,ollama` to the command above, or put
the same variable in `.env` under Compose (or `ALLOW_PRIVATE_UPSTREAMS=true` on a trusted
network — see [Configuration](docs/configuration.md#private-upstreams)).

### Which image

Three variants are published from every release:

| Tag | What it is | Choose it when |
|---|---|---|
| `ragmux/ragmux:latest` | **All-in-one:** the gateway plus its own PostgreSQL 17 + pgvector in one container, supervised by a single entrypoint (`Dockerfile.aio`). | You want one container and no database to operate. The default, and the right answer for most installations. |
| `ragmux/ragmux:latest-app` | **Gateway only**, on a distroless base, with no database inside; you point it at your own PostgreSQL with `DATABASE_URL` (`Dockerfile`). | You already run PostgreSQL (managed service, your own cluster, a separate container), or you want **several gateway replicas** against one database — the all-in-one image cannot be scaled that way. |
| `ragmux/ragmux:latest-paradedb` | **All-in-one on ParadeDB:** same gateway and same data layout, but its PostgreSQL carries `pg_search` (BM25) as well as `pgvector` (`Dockerfile.aio.paradedb`). | You want the lexical half of hybrid retrieval answered by **BM25** instead of PostgreSQL full-text ranking. Swapping from the default all-in-one image needs no reprocessing. |

The `-paradedb` image takes exactly the same command and the same volume layout as the
default one. Docker Hub is the primary registry; the full tag matrix, the GHCR mirror and
the platforms are listed under
[Published images](docs/configuration.md#published-images).

**`latest` is for trying it out; pin a version in production.** The commands here use
`latest` so they keep working as you read them, but a deployment you care about should
name the release it was tested against (`ragmux/ragmux:<version>`), or better the
manifest digest that `cosign verify` reports (`ragmux/ragmux@sha256:…`), which no later
tag move can change. Images are signed from v0.3.1 onward; see
[Verifying the container image](SECURITY.md#verifying-the-container-image).

### Docker Compose

Compose is the second way in, and the better one once you want more than the single
container gives you: **your own PostgreSQL**, the **split layout** (the gateway next to a
`pgvector/pgvector:pg17` service you can back up and upgrade on its own schedule), the
**ParadeDB** variant, a **multi-replica** setup behind a load balancer, or the scheduled
**backup** profile and the `scripts/` helpers. It works from a clone of the repository:

```bash
git clone https://github.com/Ragmux/ragmux.git
cd ragmux
cp .env.example .env
echo "SECRET_KEY=$(openssl rand -hex 32)" >> .env   # keep it with your backups
docker compose up -d
```

That is the same all-in-one image and the same `/data` volume as the `docker run` above,
with `.env` as the one place configuration lives. The other files layer on top:

| File | What it runs |
|---|---|
| [`docker-compose.yml`](docker-compose.yml) | the default all-in-one container, plus a `backup` profile |
| [`docker-compose.split.yml`](docker-compose.split.yml) | the gateway (`-app`) next to its own `pgvector/pgvector:pg17`; needs `SECRET_KEY`, `POSTGRES_PASSWORD` and `RAGMUX_DB_PASSWORD` in `.env` |
| [`docker-compose.paradedb.yml`](docker-compose.paradedb.yml) | the split layout with ParadeDB in place of pgvector, for BM25 lexical retrieval |
| [`docker-compose.scale.yml`](docker-compose.scale.yml) | an overlay on the split file: several gateway replicas behind nginx — see [Scaling](docs/scaling.md) |
| [`docker-compose.otel.yml`](docker-compose.otel.yml) | an overlay adding an OpenTelemetry Collector — see [Observability](docs/observability.md) |

For a PostgreSQL you already operate, set `DATABASE_URL` and the default container skips
its embedded server, or run the gateway-only `-app` image with `DATABASE_URL` and
`SECRET_KEY`. The two layouts, the `/data` volume and how to move between them are
described in [Deployment layouts](docs/configuration.md#deployment-layouts).

**Where state lives.** With the default layout everything is in the `ragmux-data`
volume (`/data/pg`: the database, `/data/ragmux`: the `secret.key` fallback) plus
`SECRET_KEY`. With the split layout the gateway container is stateless and the state is
the `pgdata` volume plus `SECRET_KEY`. Either way, back up with `scripts/backup.sh`
(`pg_dump -Fc`, works with both layouts) or the scheduled `backup` Compose profile — see
[Backup and restore](docs/backup-restore.md).

## Using the gateway

**1. Log in** and keep the session token:

```bash
TOKEN=$(curl -s localhost:8765/admin/api/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"YOUR_PASSWORD","bearer":true}' | jq -r .token)
AUTH="Authorization: Bearer $TOKEN"
```

**2. Add model connections** (a chat model and, for RAG, an embedding model):

```bash
curl -s localhost:8765/admin/api/models -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "claude", "provider_type": "anthropic", "api_key": "sk-ant-...", "model_name": "claude-sonnet-4-5"}'
curl -s localhost:8765/admin/api/models -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "openai-embed", "provider_type": "openai", "api_key": "sk-...", "model_name": "text-embedding-3-small"}'
```

**3. Create a RAG store and upload documents** (optional):

```bash
curl -s localhost:8765/admin/api/rag-stores -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "handbook", "embedding_connection_id": 2}'
curl -s localhost:8765/admin/api/rag-stores/1/documents -H "$AUTH" -F file=@handbook.pdf
curl -s localhost:8765/admin/api/rag-stores/1/documents -H "$AUTH"    # status: pending → processing → ready
```

**4. Create a project** and get its `sk-proj-…` key (shown only once):

```bash
curl -s localhost:8765/admin/api/projects -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "support-bot", "model_connection_id": 1, "rag_store_id": 1,
  "system_prompt": "You are the company support assistant."}'
```

**5. Call it like OpenAI.** The `model` field is echoed back but the project decides the
real model; retrieved passages are injected into the system prompt and counted in the
`x-ragmux-rag-hits` header.

```bash
curl -N localhost:8765/v1/chat/completions \
  -H "Authorization: Bearer sk-proj-..." -H 'Content-Type: application/json' \
  -d '{"model":"default","stream":true,"messages":[{"role":"user","content":"How many vacation days do I get?"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8765/v1", api_key="sk-proj-...")
resp = client.chat.completions.create(model="default", stream=True,
    messages=[{"role": "user", "content": "How many vacation days do I get?"}])
for chunk in resp:
    # Guard the array: an upstream may send a frame of its own with no
    # choices (a content filter result, a proxy keep-alive), and asking for
    # usage adds a final chunk that carries only the token counts.
    if chunk.choices:
        print(chunk.choices[0].delta.content or "", end="")
```

## Dashboard

`/admin/` serves a single-page dashboard (vanilla JavaScript, no build step, embedded in
the binary) over the same REST API. On a fresh database it shows the first-run setup
form; afterwards a sign-in screen that reports remaining attempts and lockouts. Screens:

- **Overview:** 24h / 7d / 30d window with deltas against the previous period, requests
  per day, traffic by project, budget watch with exhaustion forecasts, recent requests
  and a CSV export.
- **Models:** connection cards with provider capabilities, last test result and private
  upstream flag; the form can test a connection before saving it.
- **RAG stores:** quota meters, drag-and-drop upload, ingestion progress, per-document
  reprocess and a retrieval tester (hybrid / vector, rerank, per-stage latencies).
- **Projects:** usage rings for the minute, day and month windows with reset countdowns,
  budget forecast banner, members, key rotation and a project CSV export.
- **Playground:** real `/v1/chat/completions` calls with a project key, streaming or JSON,
  optional extra system prompt, RAG sources as citations and the response headers.
- **Users** and **Audit log** (admins): role and failed-login tiles, filters, NDJSON export.

What a user sees follows their role: viewers get read-only screens, editors the forms,
admins the user and audit screens. The page ships its own fonts (Bricolage Grotesque,
IBM Plex Sans, JetBrains Mono, all SIL OFL, `web/fonts/`) and runs under a strict
`Content-Security-Policy`, so it makes no third-party requests.

## Documentation

| Page | Contents |
|---|---|
| [Configuration](docs/configuration.md) | every environment variable and default, `-healthcheck` / `-version` flags, Compose variables, reverse-proxy notes |
| [API reference](docs/api.md) | authentication, roles per endpoint, request/response examples, error envelope, the `/v1` surface, `/healthz`, `/admin/api/system` |
| [Retrieval (RAG)](docs/rag.md) | supported formats, chunking and contextual chunks, hybrid search and `fts_config`, `max_distance`, reranking, context format |
| [Users, roles and limits](docs/users-and-limits.md) | roles matrix, project membership, login protection, audit log, rate limits and budgets, metrics and retention |
| [Providers](docs/providers.md) | provider types and endpoints, Anthropic / Gemini / Ollama translation details, request passthrough, model echo |
| [Backup and restore](docs/backup-restore.md) | what to back up, `scripts/backup.sh` and `scripts/restore.sh`, scheduled backups, PITR, restore runbook |
| [Scaling](docs/scaling.md) | running several replicas, what state is already shared, SECRET_KEY and connection pooling, leased ingestion, rolling restarts, Kubernetes notes |
| [Observability](docs/observability.md) | `/metrics` and why it is authenticated, the metric table, the cardinality checklist, `/readyz` vs `/healthz`, tracing spans and sampling, running a Collector |
| [Changelog](CHANGELOG.md) | release notes |

## Security

See [SECURITY.md](SECURITY.md) for supported versions, how to report a vulnerability
(GitHub private reporting or `security@ragmux.com`) and hardening pointers.

## Development

Requires Go 1.27+ and Docker for the database.

```bash
make dev-db               # pgvector Postgres on localhost:5433 (docker-compose.dev.yml)
make test                 # unit + end-to-end tests (mock upstream, real Postgres)
make run                  # builds and runs on :8765 against the dev database
make docker-build         # local all-in-one image, VERSION from git describe
make docker-build-app     # local gateway-only (distroless) image
make backup               # scripts/backup.sh against the compose stack
make restore FILE=backups/ragmux-<stamp>.dump YES=1   # without YES=1: plan only
```

Tests read `TEST_DATABASE_URL` (the Makefile defaults it to
`postgres://ragmux:ragmux@localhost:5433/ragmux_test?sslmode=disable`) and create a
throwaway schema per test, so they run in parallel against one server; they are skipped
when the variable is unset. CI also runs `gofmt`, `go vet`, `golangci-lint run ./...`
(v2, config in `.golangci.yml`) and `govulncheck`. The binary is pure Go
(`CGO_ENABLED=0`, `pgx`), shipped as a static executable: on a `pgvector/pgvector:pg17`
base with the entrypoint `docker/aio/entrypoint.sh` supervising both processes
(`Dockerfile.aio`, the default image) and on a distroless base (`Dockerfile`, the
`-app` image). Schema
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
internal/metrics/     dependency-free Prometheus registry and text exposition
internal/tracing/     OTLP/HTTP span exporter over the standard library
internal/obs/         the metric set, its labels and the HTTP middlewares
web/                  dashboard (index.html, vanilla JS) and its fonts, embedded in the binary
docker/aio/           entrypoint of the all-in-one image (Postgres + gateway supervision)
docker/postgres-init/ ragmux_app role and vector extension SQL shared by both layouts
```

## Roadmap

Not in this release: an import tool for 0.1 (pre-PostgreSQL) databases, OCR for scanned
PDFs, SSO / OIDC login, cost-denominated budgets, and a cache breakpoint on the gateway's
own system prompt and RAG context block.

## License

Ragmux is free software licensed under the **GNU Affero General Public License v3.0 or
later** (AGPL-3.0-or-later); see [LICENSE](LICENSE). The AGPL's network-service clause
applies: if you run a modified version as a service that users interact with over a
network, you must offer those users the corresponding source of your modified version.

Website and further material: <https://ragmux.com>
