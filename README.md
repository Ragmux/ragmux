# Ragmux

A self-hosted AI gateway that exposes one OpenAI-compatible API for several LLM providers and adds retrieval from uploaded documents.

[![Designed & Maintained with Tan](https://muhammetsafak.com/badges/designed-maintained-with-tan.svg)](https://muhammetsafak.com/tan/)

Ragmux sits between your applications and LLM providers. Provider credentials stay on the
server, and a request can be augmented with passages retrieved from documents you upload
(RAG). All state lives in one PostgreSQL database with the
[pgvector](https://github.com/pgvector/pgvector) extension: no Redis and no separate vector
database. More at <https://ragmux.com>.

## Features

- **Providers:** `openai`, `azure_openai`, `anthropic`, `gemini`, `deepseek`, `ollama` and
  `custom_openai` (vLLM, LM Studio, LiteLLM or any OpenAI-compatible server), plus
  `cohere_rerank` and `voyage_rerank`. Presets cover Mistral, xAI and Groq; see
  [Providers](docs/providers.md).
- **OpenAI-compatible API:** `/v1/chat/completions` (JSON and SSE streaming), `/v1/models`,
  `/v1/embeddings`, `/v1/retrieve` and `/v1/rerank`. The official SDKs work after changing
  `base_url` and `api_key`; Anthropic and Gemini requests are translated, tool calls included.
- **Routes, retry and fallback:** named routes with prioritised, weighted targets, retries
  before the first byte and a circuit breaker per connection; see
  [Routing](docs/providers.md#routing).
- **RAG stores:** PDF, DOCX, HTML, TXT and Markdown, section-aware chunking, hybrid search
  (pgvector with Postgres full-text ranking or ParadeDB BM25) and reranking; see
  [Retrieval](docs/rag.md).
- **Projects and API keys:** one `sk-proj-` key per project, mapped to a model connection, a
  system prompt and an optional RAG store; scoped `sk-user-` and `sk-mgmt-` keys with expiry
  and revocation.
- **Access control:** `admin`, `editor` and `viewer` roles, project membership, login
  lockout and an audit log of every management action.
- **Limits and cost:** per-project rate limits and token budgets shared across replicas,
  prompt-caching passthrough and an editable price table that estimates request cost.
- **Observability and scale:** request logs, an authenticated Prometheus `/metrics`
  endpoint, OpenTelemetry traces, and several replicas against one PostgreSQL; see
  [Observability](docs/observability.md) and [Scaling](docs/scaling.md).

## Quick Start

Requires Docker. One container runs the gateway with its own PostgreSQL 17 and pgvector:

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
`-e ADMIN_USER=admin -e ADMIN_PASSWORD=<ADMIN_PASSWORD>` instead and the account is
created on first start (the password is 12 characters at least, 72 bytes at most; on an
empty database a shorter one stops the start with an error).

- **`SECRET_KEY` encrypts the provider credentials.** Without it the gateway generates
  `/data/ragmux/secret.key` inside the volume, which you then have to back up together
  with the database. Keep the key with your backups.
- **The port is published on loopback only.** Put a TLS-terminating reverse proxy in front
  for network access; see [Deployment](#deployment). Change the mapping to `-p 8765:8765`
  only deliberately.
- **Local models:** provider URLs on private networks (Ollama on the Docker host, a vLLM
  service on the same Docker network) are refused by default as an SSRF guard. Add
  `-e PRIVATE_UPSTREAM_ALLOWLIST=host.docker.internal,ollama` to the command above; see
  [Private upstreams](docs/configuration.md#private-upstreams).

`latest` is for trying it out. Pin a version in production; see [Which image](#which-image).

## Installation

### Which image

Three variants are published from every release:

| Tag | What it is | Choose it when |
|---|---|---|
| `ragmux/ragmux:latest` | **All-in-one:** the gateway plus its own PostgreSQL 17 + pgvector in one container (`Dockerfile.aio`). | You want one container and no database to operate. The default for most installations. |
| `ragmux/ragmux:latest-app` | **Gateway only**, on a distroless base; you point it at your own PostgreSQL with `DATABASE_URL` (`Dockerfile`). | You already run PostgreSQL, or you want **several gateway replicas** against one database. |
| `ragmux/ragmux:latest-paradedb` | **All-in-one on ParadeDB:** its PostgreSQL carries `pg_search` (BM25) as well as `pgvector` (`Dockerfile.aio.paradedb`). | You want the lexical half of hybrid retrieval answered by BM25. |

The `-paradedb` image takes the same command and volume layout as the default one. Docker
Hub is the primary registry; the tag matrix, the GHCR mirror and the platforms are under
[Published images](docs/configuration.md#published-images).

A deployment you care about should name the release it was tested against
(`ragmux/ragmux:<version>`), or the manifest digest that `cosign verify` reports. Images
are signed from v0.3.1 onward; see
[Verifying the container image](SECURITY.md#verifying-the-container-image).

### Docker Compose

Compose works from a clone of the repository and is the better route for your own
PostgreSQL, the split layout, ParadeDB, several replicas or the scheduled backup profile:

```bash
git clone https://github.com/ragmux/ragmux.git
cd ragmux
cp .env.example .env
echo "SECRET_KEY=$(openssl rand -hex 32)" >> .env
docker compose up -d
```

That is the same all-in-one image and `/data` volume as the `docker run` above, with `.env`
as the one place configuration lives. Keep the generated `SECRET_KEY` with your backups.
The other files layer on top:

| File | What it runs |
|---|---|
| [`docker-compose.yml`](docker-compose.yml) | the default all-in-one container, plus a `backup` profile |
| [`docker-compose.split.yml`](docker-compose.split.yml) | the gateway (`-app`) next to its own `pgvector/pgvector:pg17`; needs `SECRET_KEY`, `POSTGRES_PASSWORD` and `RAGMUX_DB_PASSWORD` in `.env` |
| [`docker-compose.paradedb.yml`](docker-compose.paradedb.yml) | the split layout with ParadeDB in place of pgvector, for BM25 lexical retrieval |
| [`docker-compose.scale.yml`](docker-compose.scale.yml) | an overlay on the split file: several gateway replicas behind nginx; see [Scaling](docs/scaling.md) |
| [`docker-compose.otel.yml`](docker-compose.otel.yml) | an overlay adding an OpenTelemetry Collector; see [Observability](docs/observability.md) |

For a PostgreSQL you already operate, set `DATABASE_URL` and the default container skips
its embedded server, or run the `-app` image with `DATABASE_URL` and `SECRET_KEY`. See
[Deployment layouts](docs/configuration.md#deployment-layouts).

## Usage

The steps below use the management API; the dashboard at `/admin/` does the same through
forms. It is a single-page UI embedded in the binary: viewers get read-only screens,
editors the forms, administrators the user and audit screens. Every screen is listed in
[Dashboard](docs/dashboard.md).

**1. Log in** and keep the session token:

```bash
TOKEN=$(curl -s localhost:8765/admin/api/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<ADMIN_PASSWORD>","bearer":true}' | jq -r .token)
AUTH="Authorization: Bearer $TOKEN"
```

**2. Add model connections** (a chat model and, for RAG, an embedding model):

```bash
curl -s localhost:8765/admin/api/models -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "claude", "provider_type": "anthropic", "api_key": "<ANTHROPIC_API_KEY>", "model_name": "claude-sonnet-4-5"}'
curl -s localhost:8765/admin/api/models -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "openai-embed", "provider_type": "openai", "api_key": "<OPENAI_API_KEY>", "model_name": "text-embedding-3-small"}'
```

**3. Create a RAG store and upload documents** (optional):

```bash
curl -s localhost:8765/admin/api/rag-stores -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "handbook", "embedding_connection_id": 2}'
curl -s localhost:8765/admin/api/rag-stores/1/documents -H "$AUTH" -F file=@handbook.pdf
curl -s localhost:8765/admin/api/rag-stores/1/documents -H "$AUTH"    # status: pending → processing → ready
```

**4. Create a project** and get its `sk-proj-` key (shown only once):

```bash
curl -s localhost:8765/admin/api/projects -H "$AUTH" -H 'Content-Type: application/json' -d '{
  "name": "support-bot", "model_connection_id": 1, "rag_store_id": 1,
  "system_prompt": "You are the company support assistant."}'
```

**5. Call it like OpenAI.** The `model` field is echoed back but the project decides the
real model (a project with [routes](docs/providers.md#routing) uses it to pick one).
Retrieved passages are injected into the system prompt and counted in the
`x-ragmux-rag-hits` header.

```bash
curl -N localhost:8765/v1/chat/completions \
  -H "Authorization: Bearer <PROJECT_KEY>" -H 'Content-Type: application/json' \
  -d '{"model":"default","stream":true,"messages":[{"role":"user","content":"How many vacation days do I get?"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8765/v1", api_key="<PROJECT_KEY>")
resp = client.chat.completions.create(model="default", stream=True,
    messages=[{"role": "user", "content": "How many vacation days do I get?"}])
for chunk in resp:
    # Guard the array: an upstream may send a frame with no choices, and
    # asking for usage adds a final chunk that carries only the token counts.
    if chunk.choices:
        print(chunk.choices[0].delta.content or "", end="")
```

Endpoints, roles per endpoint and the error envelope are in the
[API reference](docs/api.md).

## How It Works

```text
client  ──►  POST /v1/chat/completions (Bearer <PROJECT_KEY>)
             │
             ├─ project → model connection (provider credentials stay server-side)
             ├─ project → RAG store (optional): embed query → top-k chunks → inject context
             └─ adapter: OpenAI · Anthropic · Gemini · DeepSeek · Ollama · custom OpenAI (vLLM…)
                          JSON and SSE streaming, normalised to the OpenAI schema
             │
             └─ PostgreSQL + pgvector: users, connections (AES-256-GCM keys), projects,
                documents (bytea), chunks, HNSW vector indexes, request logs
```

The binary is pure Go and ships as a static executable. Schema changes are embedded SQL
files applied at startup under an advisory lock, so several replicas can start against one
database.

## Configuration

Configuration is environment variables. The ones most installations touch:

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | required for `-app` image | PostgreSQL connection string; the all-in-one image uses its embedded server when unset |
| `SECRET_KEY` | `DATA_DIR/secret.key` fallback | 64 hex characters; encrypts provider credentials at rest |
| `PORT` | `8765` | HTTP listen port |
| `ADMIN_USER`, `ADMIN_PASSWORD` | `admin`, none | Create the first administrator on an empty database |
| `TRUST_PROXY_HEADERS` | `false` | Take the client address from `X-Forwarded-For`; enable only behind a reverse proxy |
| `TRUSTED_PROXY_CIDRS` | none | Networks whose proxy headers are honoured |
| `SECURE_COOKIES` | `false` | Mark the session cookie `Secure` unconditionally |
| `ALLOW_PRIVATE_UPSTREAMS` | `false` | Let provider URLs point at private networks |
| `PRIVATE_UPSTREAM_ALLOWLIST` | none | Hostnames allowed to resolve to private addresses |

Every variable, default and the `-healthcheck` / `-version` flags are in
[Configuration](docs/configuration.md).

## Deployment

- **State is the `/data` volume plus `SECRET_KEY`.** In the all-in-one image `/data/pg` is
  the PostgreSQL cluster and `/data/ragmux` holds the `secret.key` fallback. With the split
  layout the gateway is stateless and the state is the `pgdata` volume plus `SECRET_KEY`.
- **Terminate TLS in front.** Put a reverse proxy before the gateway and set
  `TRUST_PROXY_HEADERS` and `SECURE_COOKIES`; see
  [Behind a reverse proxy](docs/configuration.md#behind-a-reverse-proxy).
- **Back up.** Use `scripts/backup.sh` (`pg_dump -Fc`, both layouts) or the scheduled
  `backup` Compose profile; see [Backup and restore](docs/backup-restore.md).
- **Scale out** with the `-app` image and an external PostgreSQL; see
  [Scaling](docs/scaling.md).

## Upgrading

Back up first, then pull and restart:

```bash
scripts/backup.sh && docker compose pull && docker compose up -d
```

With `docker run`, `scripts/backup.sh` does not apply; dump the database directly, then pull,
recreate the container on the same volume with the **same** `SECRET_KEY` (a new key cannot
decrypt stored credentials). See [Backup and restore](docs/backup-restore.md).

```bash
docker exec -u postgres ragmux pg_dump -Fc --no-owner --no-privileges -d ragmux > ragmux.dump
```

Migrations run at gateway start. The PostgreSQL major version inside the all-in-one image
never changes within a minor series, and pulling a newer image never runs `pg_upgrade`; see
[Deployment layouts](docs/configuration.md#deployment-layouts). Release notes are in
[CHANGELOG.md](CHANGELOG.md).

## Documentation

Reference pages are on <https://ragmux.com> and in this repository.

| Page | Contents |
|---|---|
| [Configuration](docs/configuration.md) | every environment variable and default, flags, subcommands, Compose variables, reverse-proxy notes |
| [API reference](docs/api.md) | authentication, roles per endpoint, the `/v1` surface, error envelope, `/healthz` |
| [Retrieval (RAG)](docs/rag.md) | formats, chunking, hybrid search, reranking, context format |
| [Users, roles and limits](docs/users-and-limits.md) | roles matrix, login protection, audit log, rate limits, budgets, retention |
| [Providers](docs/providers.md) | provider types and presets, translation details, routing, retry, fallback |
| [Backup and restore](docs/backup-restore.md) | what to back up, scripts, scheduled backups, restore runbook |
| [Scaling](docs/scaling.md) | several replicas, shared state, leased ingestion, rolling restarts |
| [Observability](docs/observability.md) | `/metrics`, the metric table, `/readyz` vs `/healthz`, tracing |
| [Changelog](CHANGELOG.md) | release notes |

## Project Status

**Beta.** Ragmux is pre-1.0: the public API, configuration keys and metric names can still
change between minor versions, and each change is listed in the
[CHANGELOG](CHANGELOG.md). The direction after the current release is in
[ROADMAP.md](ROADMAP.md); OCR for scanned PDFs, SSO / OIDC login, cost-denominated budgets
and an import tool for 0.1 databases are not scheduled.

## Contributing

Bug reports, documentation fixes and pull requests are welcome; see
[CONTRIBUTING.md](CONTRIBUTING.md) for the development setup and the pull request process.

## Security

Do not report security vulnerabilities in public issues; see [SECURITY.md](SECURITY.md).

## License

Licensed under the [GNU Affero General Public License v3.0 or later](LICENSE)
(AGPL-3.0-or-later). If you run a modified version as a network service, the AGPL requires
you to offer its users the corresponding source.

The fonts in `web/fonts/` are third-party and licensed under the SIL Open Font License 1.1;
the licence texts sit next to them.
