# Ragmux — self-hosted AI gateway

Ragmux is a single-binary AI gateway written in Go. It sits between your applications and
LLM providers, exposes one **OpenAI-compatible API**, and can augment every request with
**retrieval (RAG)** from documents you upload. All state — users, model connections,
projects, documents, chunks, vectors and metrics — lives in one **PostgreSQL** database
with the `pgvector` extension. One container, no Redis, no separate vector database.

Providers: `openai`, `anthropic`, `gemini`, `deepseek`, `ollama` and `custom_openai`
(vLLM, LM Studio, LiteLLM). JSON and SSE streaming, normalised to the OpenAI schema, with
an embedded dashboard at `/admin/`.

- Website: https://ragmux.com
- Source and issues: https://github.com/Ragmux/ragmux
- Documentation: https://github.com/Ragmux/ragmux/tree/main/docs

## Which tag do I want?

Three variants are published from every release. Each gets the same three tag forms:
the exact version (`ragmux/ragmux:1.2.3`), the minor series (`ragmux/ragmux:1.2`) and
`ragmux/ragmux:latest`. The Tags tab above lists what is actually published.

| Tag | What it is | Choose it when |
|---|---|---|
| `ragmux/ragmux:latest` | **All-in-one.** The gateway plus its own PostgreSQL 17 + pgvector server in one container, supervised by a single entrypoint. State lives in the `/data` volume. Built from `Dockerfile.aio`. | You want one container and no database to operate. This is the default and the right answer for most installations. |
| `ragmux/ragmux:latest-app` | **Gateway only**, on a distroless base — about 6 MB. No database inside; you point it at your own PostgreSQL with `DATABASE_URL`. Built from `Dockerfile`. | You already run PostgreSQL (managed service, your own cluster, a separate container), or you want to run **several gateway replicas** against one database. The all-in-one image cannot be scaled that way: a second replica would start a second database on its own volume. |
| `ragmux/ragmux:latest-paradedb` | **All-in-one on ParadeDB**: same gateway and same data layout, but its PostgreSQL carries `pg_search` (BM25) as well as `pgvector`. Built from `Dockerfile.aio.paradedb`. | You want the lexical half of hybrid retrieval answered by **BM25** instead of PostgreSQL full-text ranking. Upgrading from the default all-in-one image is a plain image swap and needs no reprocessing. |

The `-app` image needs a PostgreSQL with the `vector` extension available and a role that
may create tables in `public`; see
[Database privileges](https://github.com/Ragmux/ragmux/blob/main/docs/configuration.md#database-privileges).

**`latest` is for trying it out; pin a version in production.** The commands below use
`latest` so they keep working as you read them, but a deployment you care about should
name the release it was tested against — `ragmux/ragmux:1.2.3`, or better the manifest
digest that `cosign verify` reports (`ragmux/ragmux@sha256:…`), which no later tag move
can change. Pick the newest version from the Tags tab and substitute it wherever a command
below says `latest`.

## Run it

**All-in-one** — one container, its own database:

```sh
docker run -d --name ragmux \
  -p 127.0.0.1:8765:8765 \
  -e SECRET_KEY="$(openssl rand -hex 32)" \
  -v ragmux-data:/data \
  ragmux/ragmux:latest
```

Then open http://localhost:8765/admin/ and create the first administrator (username and a
password of at least 12 characters); that form only works while no user exists. For an
unattended install pass `-e ADMIN_USER=admin -e ADMIN_PASSWORD=...` instead and the
account is created on first start.

Two things worth knowing about that command:

- **`SECRET_KEY` encrypts the provider credentials.** Nothing is required, but without it
  the gateway generates `/data/ragmux/secret.key` inside the volume, which you then have
  to back up together with the database. Keep the key with your backups.
- **The port is bound to loopback.** Put a TLS-terminating reverse proxy in front for
  network access and set `TRUST_PROXY_HEADERS` / `SECURE_COOKIES`; see
  [Behind a reverse proxy](https://github.com/Ragmux/ragmux/blob/main/docs/configuration.md#behind-a-reverse-proxy).
  Change the mapping to `-p 8765:8765` only deliberately.

Everything persistent is under `/data`: `/data/pg` is the PostgreSQL cluster and
`/data/ragmux` the `secret.key` fallback. That volume plus `SECRET_KEY` is the whole
state. The embedded PostgreSQL has no TCP listener at all, so `8765` is the only port.
`ragmux/ragmux:latest-paradedb` takes exactly the same command and the same volume layout.

**Gateway only**, against your own PostgreSQL:

```sh
docker run -d --name ragmux \
  -p 127.0.0.1:8765:8765 \
  -e DATABASE_URL="postgres://ragmux:PASSWORD@postgres:5432/ragmux?sslmode=disable" \
  -e SECRET_KEY="$(openssl rand -hex 32)" \
  ragmux/ragmux:latest-app
```

Setting `DATABASE_URL` on the **all-in-one** image is also supported and skips its
embedded server (`EMBEDDED_POSTGRES=false` does the same explicitly).

Compose files for both layouts — plus the split, ParadeDB, multi-replica, OpenTelemetry
and scheduled-backup variants — are in the repository:
[`docker-compose.yml`](https://github.com/Ragmux/ragmux/blob/main/docker-compose.yml),
[`docker-compose.split.yml`](https://github.com/Ragmux/ragmux/blob/main/docker-compose.split.yml),
[`docker-compose.paradedb.yml`](https://github.com/Ragmux/ragmux/blob/main/docker-compose.paradedb.yml).

## Calling the gateway

Create a project in the dashboard, take its `sk-proj-…` key and change `base_url`:

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8765/v1", api_key="sk-proj-...")
resp = client.chat.completions.create(
    model="default",
    messages=[{"role": "user", "content": "How many vacation days do I get?"}],
)
```

The `model` field is echoed back, but the project decides the real model, the provider
credentials and whether retrieved passages are injected into the system prompt.

## Configuration

Nothing is required to start. The environment variables the images read, their defaults
and the `-healthcheck` / `-version` flags are documented in
[Configuration](https://github.com/Ragmux/ragmux/blob/main/docs/configuration.md). The
ones you are most likely to set: `SECRET_KEY`, `DATABASE_URL`, `ADMIN_USER` /
`ADMIN_PASSWORD`, `LOG_LEVEL`, `CORS_ORIGINS`, `TRUST_PROXY_HEADERS`, and
`PRIVATE_UPSTREAM_ALLOWLIST` when your model server (Ollama, vLLM) is on a private
network — private upstreams are refused by default as an SSRF guard.

All three images carry a `HEALTHCHECK` that runs `ragmux -healthcheck`.

## Platforms and signatures

Published for **`linux/amd64`** and **`linux/arm64`**.

Images from **v0.3.1 onward** are signed with [cosign](https://docs.sigstore.dev/)
(keyless, through the release workflow's GitHub OIDC identity). Verify a tag before
pulling it into production:

```sh
cosign verify docker.io/ragmux/ragmux:latest \
  --certificate-identity-regexp '^https://github.com/[Rr]agmux/ragmux/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The certificate identity must be `release.yml` in this repository running for a `v*` tag;
anything else means the image did not come from the release pipeline. The signature is
attached to the manifest digest, so it also covers the `<major>.<minor>` and `latest` tags
pointing at the same release. See
[Verifying the container image](https://github.com/Ragmux/ragmux/blob/main/SECURITY.md#verifying-the-container-image).

The same images are also published to GitHub Container Registry as
`ghcr.io/ragmux/ragmux` with identical tags.

## Documentation

- [Configuration](https://github.com/Ragmux/ragmux/blob/main/docs/configuration.md) — every environment variable, deployment layouts, reverse-proxy notes
- [API reference](https://github.com/Ragmux/ragmux/blob/main/docs/api.md) — authentication, the `/v1` surface, error envelope, health endpoints
- [Retrieval (RAG)](https://github.com/Ragmux/ragmux/blob/main/docs/rag.md) — formats, chunking, hybrid search and the `pgvector` / `pg_search` backends
- [Users, roles and limits](https://github.com/Ragmux/ragmux/blob/main/docs/users-and-limits.md) — roles, API keys, rate limits and budgets
- [Providers](https://github.com/Ragmux/ragmux/blob/main/docs/providers.md) — provider types and translation details
- [Backup and restore](https://github.com/Ragmux/ragmux/blob/main/docs/backup-restore.md) — what to back up, scripts, restore runbook
- [Scaling](https://github.com/Ragmux/ragmux/blob/main/docs/scaling.md) — several replicas against one PostgreSQL
- [Observability](https://github.com/Ragmux/ragmux/blob/main/docs/observability.md) — `/metrics`, OpenTelemetry traces, `/readyz` vs `/healthz`
- [Changelog](https://github.com/Ragmux/ragmux/blob/main/CHANGELOG.md)
- [Security policy](https://github.com/Ragmux/ragmux/blob/main/SECURITY.md)

## License

Ragmux is free software licensed under the **GNU Affero General Public License v3.0 or
later** (AGPL-3.0-or-later). The AGPL's network-service clause applies: if you run a
modified version as a service that users interact with over a network, you must offer
those users the corresponding source of your modified version. See
[LICENSE](https://github.com/Ragmux/ragmux/blob/main/LICENSE).
