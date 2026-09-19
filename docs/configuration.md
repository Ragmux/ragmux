# Configuration

Ragmux is configured entirely through environment variables. They are read once at
startup (`internal/config/config.go`); an invalid value makes the binary print
`config: <reason>` and exit with status `2`.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | *(required)* | PostgreSQL connection string, e.g. `postgres://user:pass@host:5432/ragmux?sslmode=require`. The `vector` extension is created on first start if the role may do so; otherwise run `CREATE EXTENSION vector` beforehand (see [Database privileges](#database-privileges)). |
| `DATABASE_URL_FILE` | *(none)* | Path of a file whose trimmed content is used when `DATABASE_URL` is unset (Docker/Compose secrets). |
| `SECRET_KEY` | *(file fallback)* | 32-byte AES-256-GCM key as 64 hex characters that encrypts provider API keys at rest. Generate once with `openssl rand -hex 32` and keep it with your backups. Any other length or non-hex value is rejected. When unset, the gateway generates and reads `DATA_DIR/secret.key` and logs a warning; that fallback is meant for local development only. Rotate with `ragmux rotate-key` (see [Backup and restore](backup-restore.md#rotating-secret_key)). |
| `SECRET_KEY_FILE` | *(none)* | Path of a file holding the key, used when `SECRET_KEY` is unset. |
| `DB_MAX_CONNS` | `10` | Connection pool size (must be `>= 1`). |
| `DATA_DIR` | `/app/data` | Only used for the `secret.key` fallback when `SECRET_KEY` is unset. The all-in-one image sets it to `/data/ragmux` inside its volume. |
| `PORT` | `8765` | HTTP listen port (`1`-`65535`). Also read by `-healthcheck`. |
| `ADMIN_USER` | `admin` | Username of the administrator pre-created on first start when `ADMIN_PASSWORD` is set and the `users` table is empty. Ignored otherwise. |
| `ADMIN_PASSWORD` | *(none)* | Set it for unattended installs: the account is created once with this password and the log says `admin user created from ADMIN_PASSWORD`. When unset, nothing is created; the log says `no users yet: open /admin/ to create the first administrator` and the dashboard shows the first-run setup form (see [`/setup`](api.md#first-run-setup)) until the first account exists. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON lines on stdout; unknown values fall back to `info`. |
| `CORS_ORIGINS` | *(none)* | Comma-separated browser origins allowed to call the API (`*` allows all). Unset means no CORS headers at all. |
| `SESSION_TTL` | `24h` | Dashboard session lifetime (Go duration such as `12h`, `30m`). |
| `UPSTREAM_TIMEOUT` | `5m` | Timeout for a non-streaming provider call. Streaming calls use it for the connect and response-header phase only. |
| `INGEST_WORKERS` | `2` | Parallel document ingestion jobs on this replica (`>= 1`). |
| `INGEST_LEASE` | `2m` | How long this replica owns a document it claimed without renewing the claim (Go duration, minimum `3s`). The worker renews it every third of this while the job runs, so it bounds how long a crashed replica's document stays untouchable, not how long a job may take. See [Ingestion across replicas](scaling.md#ingestion-across-replicas). |
| `INGEST_POLL_INTERVAL` | `5s` | How often the ingestion dispatcher looks for claimable documents when nothing wakes it (Go duration, minimum `100ms`). An upload on this replica kicks it immediately; the poll is what finds work another replica queued. |
| `INGEST_MAX_ATTEMPTS` | `5` | How many times one document may be claimed before the retention job marks it `failed` (`>= 1`). It stops a document that kills the process from becoming a cluster-wide crash loop. A successful ingest, an upload and a reprocess each reset the counter. |
| `MAX_PENDING_DOCUMENTS` | `1024` | Cluster-wide ingestion backlog an upload is still accepted into (`>= 1`). Beyond it the upload gets `503`; the count is `documents` in status `pending` across every replica, cached for about a second. |
| `MAX_UPLOAD_MB` | `50` | Maximum size of one document upload request in MiB (`>= 1`). |
| `MAX_CHUNKS_PER_DOCUMENT` | `20000` | A document that splits into more chunks than this is marked `failed` before anything is embedded (`>= 1`). Bounds the memory and embedding cost of one document. |
| `MAX_DOCUMENTS_PER_STORE` | `0` | Instance-wide ceiling on the documents one RAG store may hold (`0` = unlimited). A store's own `max_documents` can only lower it; uploads over the limit get `422 store_quota`. See [Quotas](rag.md#quotas). |
| `MAX_BYTES_PER_STORE_MB` | `0` | Instance-wide ceiling on the summed upload size of one RAG store in MiB (`0` = unlimited); combined with the store's `max_bytes` the same way. |
| `ALLOW_PRIVATE_UPSTREAMS` | `false` | `true` lets provider `base_url`s point at loopback, link-local and private networks and re-enables `HTTP_PROXY`/`HTTPS_PROXY` for provider calls. See [Private upstreams](#private-upstreams). |
| `PRIVATE_UPSTREAM_ALLOWLIST` | *(empty)* | Comma-separated hostnames (case-insensitive) that may resolve to private addresses while `ALLOW_PRIVATE_UPSTREAMS` stays `false`, e.g. `host.docker.internal,ollama`. |
| `STREAM_MAX_DURATION` | `30m` | Wall-time limit for one streaming provider response (Go duration). The stream ends with a `504 timeout` error when it is reached. |
| `STREAM_MAX_BYTES_MB` | `256` | Maximum bytes read from one streaming provider response in MiB (`>= 1`). |
| `RERANK_TIMEOUT` | — | Bounds one rerank call whichever backend a store uses. Empty gives the `llm` backend 10 s (a full chat completion) and `cohere`/`voyage` 5 s (a single scoring call). A timeout is not an error: retrieval falls back to the fused order. See [Reranking](rag.md#reranking). |
| `PG_SEARCH_TOKENIZER` | `default` | ParadeDB analyser for the BM25 index, used only by stores whose `search_backend` is `pg_search`. `default` splits on unicode word boundaries and lowercases, matching the tsvector backend, so a store can move between the two without changing which words match; `en_stem` adds English stemming. Instance-wide, because the index is global — changing it needs `DROP INDEX IF EXISTS idx_chunks_bm25;` and a rebuild. A value that is not a plain lowercase identifier is refused at startup: it is interpolated into the index DDL. |
| `IMAGE_FETCH` | `true` | Download the `image_url` parts of a chat request for providers whose upstream cannot fetch a URL itself (`gemini`, `ollama`) and send the bytes inline. `false` refuses such a request with `400` instead; inline `data:` images are unaffected, and `anthropic`/`openai` forward the URL either way. Downloads go out over the same hardened transport as provider calls but always with the private-address filter on: `ALLOW_PRIVATE_UPSTREAMS` and `PRIVATE_UPSTREAM_ALLOWLIST` lift it for a `base_url` an editor typed, never for an image URL an API key chose. See [Images](providers.md#images). |
| `IMAGE_FETCH_MAX_MB` | `8` | Maximum size of one fetched image in MiB (`>= 1`). Enforced on `Content-Length` and again while reading, so a response that declares nothing cannot exceed it. |
| `IMAGE_FETCH_TIMEOUT` | `10s` | Timeout for one image fetch (Go duration, `> 0`). |
| `IMAGE_FETCH_MAX_PER_REQUEST` | `8` | Maximum images one chat request may fetch (`>= 1`); they are fetched one at a time. Over the limit the request gets `400`. |
| `IMAGE_CACHE_ENTRIES` | `64` | Size of the in-memory fetched-image cache (`0` disables it). Keyed on the URL alone, so an image whose content changes within the TTL is served stale; the cache also has a fixed 64 MiB ceiling. |
| `IMAGE_CACHE_TTL` | `10m` | How long a fetched image may be reused (Go duration, `> 0`). |
| `SECURE_COOKIES` | `false` | `true` marks the `ragmux_session` cookie `Secure` unconditionally. The flag is set anyway when the request arrived over TLS, or when `TRUST_PROXY_HEADERS` is on and the proxy sends `X-Forwarded-Proto: https`. |
| `TRUST_PROXY_HEADERS` | `false` | `true` takes the client address from the **last** `X-Forwarded-For` entry (the one appended by the nearest proxy), or from `X-Real-IP` when there is no `X-Forwarded-For`. Used by login limits, the audit log and the `Secure` cookie flag. Enable only behind a reverse proxy; see [Behind a reverse proxy](#behind-a-reverse-proxy). |
| `TRUSTED_PROXY_CIDRS` | *(none)* | Comma-separated networks (`10.0.0.0/8,172.16.0.0/12`, single addresses allowed). When set, proxy headers are honoured only for connections from these networks, and `X-Forwarded-For` is walked from the right past addresses inside them, so the first hop that is not one of your proxies wins. |
| `LOGIN_RATE_LIMIT_PER_MIN` | `10` | Failed logins allowed per minute from one IP address (`0` disables). |
| `LOGIN_USER_LIMIT_PER_MIN` | `5` | Failed logins allowed per minute for one username (`0` disables). |
| `LOGIN_LOCKOUT_FAILURES` | `20` | Failures within `LOGIN_LOCKOUT_MINUTES` that lock a username out (`0` disables). |
| `LOGIN_LOCKOUT_MINUTES` | `15` | Lockout window in minutes. |
| `LOG_RETENTION_DAYS` | `90` | Request logs older than this many days are deleted by the hourly retention job (`0` keeps them forever). |
| `AUDIT_RETENTION_DAYS` | `365` | Audit entries older than this many days are deleted (`0` keeps them forever). |
| `METRICS_ENABLED` | `false` | `true` serves the Prometheus text exposition at `/metrics`. Off, the route does not exist and returns `404`. See [Observability](observability.md). |
| `METRICS_TOKEN` | *(none)* | Bearer token `/metrics` requires, compared in constant time. Required whenever `METRICS_LISTEN` is unset or binds a non-loopback address; see [Why the endpoint is authenticated](observability.md#why-the-endpoint-is-authenticated). |
| `METRICS_TOKEN_FILE` | *(none)* | Path of a file whose trimmed content is used when `METRICS_TOKEN` is unset (Docker/Compose secrets). |
| `METRICS_LISTEN` | *(none)* | `host:port` (e.g. `127.0.0.1:9090`). When set, `/metrics` is served by a **second** `http.Server` on that address and is **not mounted on the main router at all**, so no reverse-proxy rule can expose it. A loopback host (`127.0.0.1`, `::1`, `localhost`) is accepted without a token; any other host still needs one. Setting it without `METRICS_ENABLED=true` is a configuration error. |
| `METRICS_MAX_SERIES` | `5000` | Ceiling on the registry's label combinations (`>= 1`). New combinations beyond it are dropped, counted in `ragmux_metrics_series_dropped_total` and logged at `error` (at most once a minute). There is no eviction: once the cap is full, every new series is lost until it is raised or the cardinality comes down, so treat a non-zero drop count as an incident. |
| `TRACING_ENABLED` | *(endpoint set)* | Defaults to `true` when an OTLP endpoint is configured and `false` otherwise; an explicit `false` always wins. `true` without an endpoint is a configuration error. |
| `TRACING_TRUST_INCOMING` | `false` | `true` continues a trace a client started from its `traceparent` header. Off by default: a client could otherwise pin every request into one trace and force the sampled flag on all of it. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(none)* | Base URL of an OpenTelemetry Collector's OTLP/HTTP receiver, e.g. `http://otel-collector:4318`. `/v1/traces` is appended unless the URL already ends in it. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | *(none)* | Traces-signal URL; takes precedence over `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `OTEL_EXPORTER_OTLP_HEADERS` | *(none)* | `k=v,k2=v2` headers sent with every export request (a vendor's auth header). A value may contain `=`; only the first one separates. |
| `OTEL_SERVICE_NAME` | `ragmux` | `service.name` resource attribute. |
| `OTEL_RESOURCE_ATTRIBUTES` | *(none)* | Extra resource attributes in the same `k=v,k2=v2` form, e.g. `deployment.environment=prod`. |
| `OTEL_TRACES_SAMPLER_ARG` | `0.05` | Head sampling probability for new traces, `0` to `1`. |

The integer variables from `LOGIN_RATE_LIMIT_PER_MIN` down accept `0` or any positive
number; a negative or non-numeric value is a configuration error.

Standard `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` variables are honoured for outbound
provider calls (`http.ProxyFromEnvironment`) only when `ALLOW_PRIVATE_UPSTREAMS=true`:
a proxy connects on the gateway's behalf and would bypass the private-address filter
described next.

## Private upstreams

Provider `base_url`s are entered by dashboard editors, so the gateway treats them as
untrusted destinations. By default an outbound provider connection is refused when the
host resolves to a loopback, link-local, multicast, unspecified or private address
(`10/8`, `172.16/12`, `192.168/16`, `100.64/10`, `169.254/16`, `192.0.0/24`,
`198.18/15`, `fc00::/7`, `fe80::/10`, `64:ff9b::/96`, IPv4-mapped IPv6 included). The
gateway resolves the name itself and dials the checked address, so a DNS answer that
changes between the check and the connect does not help an attacker. Redirects are
followed at most three hops, only to `http`/`https` and only to the same host. A rejected
connection is reported as
`upstream host "…" resolves to a private or local address; set ALLOW_PRIVATE_UPSTREAMS=true or add it to PRIVATE_UPSTREAM_ALLOWLIST`,
and the dashboard refuses to save a `base_url` whose host currently resolves only to such
addresses.

Local model servers therefore need to be allowed explicitly:

- `PRIVATE_UPSTREAM_ALLOWLIST=host.docker.internal,ollama` lists the hostnames that may
  resolve to private addresses (an Ollama on the Docker host, a `vllm` service in the same
  Compose network, …). Hostnames only; IP literals such as `127.0.0.1` are not matched.
- `ALLOW_PRIVATE_UPSTREAMS=true` disables the filter for every connection. Use it only
  when everyone who can edit model connections may also reach every host the gateway can.

The same policy applies to the `POST /admin/api/models/{id}/test` ping.

## Command-line flags

| Flag | Effect |
|---|---|
| `-version` | Print `ragmux <version>` and exit. The version is stamped at build time (`-ldflags "-X main.version=..."`, `VERSION` build arg in Docker). |
| `-healthcheck` | Send `GET http://127.0.0.1:$PORT/healthz` with a 3-second timeout and exit `0` on HTTP 200, `1` otherwise. Only `PORT` is read. The Docker image and `docker-compose.yml` use it as the container `HEALTHCHECK`. |

Without flags the binary loads the configuration, connects to the database, applies
pending migrations under an advisory lock (several replicas may start against the same
database), checks that `SECRET_KEY` is the one the database was written with (see
[SECRET_KEY](scaling.md#secret-key)), creates the first admin if needed, starts working
the document ingestion queue and starts listening.

## Subcommands

Both open the database directly and need `DATABASE_URL` plus the current `SECRET_KEY`
(or `SECRET_KEY_FILE` / the `secret.key` fallback); neither needs a running gateway.

| Subcommand | Purpose |
|---|---|
| `ragmux rotate-key --new <hex>` | Re-encrypt the stored provider credentials with a new `SECRET_KEY`. Stop the gateway first; see [Rotating SECRET_KEY](backup-restore.md#rotating-secret_key). |
| `ragmux reset-password <username> …` | Set a dashboard password from the command line, for when every administrator is locked out. |

### reset-password

```
ragmux reset-password <username> [--generate|--stdin] [--force] [--no-revoke] [--revoke-keys]
```

| Flag | Effect |
|---|---|
| `--generate` | Generate a 24-character password and print it after the reset. |
| `--stdin` | Read the new password from the first line of standard input. |
| `--force` | Skip the confirmation prompt. |
| `--no-revoke` | Keep the user's dashboard sessions; by default they are all revoked. |
| `--revoke-keys` | Also set `revoked_at` on every API key the account owns. |

Exactly one of `--generate` and `--stdin` is required. There is deliberately **no
`--password` flag and no interactive prompt**: a flag value lands in `ps` output and the
shell history, and a no-echo prompt would need `golang.org/x/term`, which is not a
dependency of this project and is not worth adding for one emergency command. The
minimum length is 12 characters (the first-run setup's floor, not the dashboard's 8 — an
emergency admin reset should not create a weak credential) and the maximum is bcrypt's
72 bytes.

```bash
# print a fresh password
DATABASE_URL=… SECRET_KEY=… ragmux reset-password admin --generate
# or pipe one in, without a confirmation prompt
printf '%s\n' "$NEW_PASSWORD" | ragmux reset-password admin --stdin
# in the all-in-one container
docker compose run --rm ragmux reset-password admin --generate
```

An unknown username exits `1` with a clear message; the command **never creates an
account**, so a typo cannot quietly add an administrator. Usernames are matched
case-insensitively. A deactivated account is reported as such: the password is set but
the user still cannot sign in until an admin reactivates it. When standard input is a
terminal and neither `--stdin` nor `--force` is given, the reset is confirmed
interactively first.

This grants no new privilege. Anyone holding `DATABASE_URL` and `SECRET_KEY` can already
rewrite any row, provider credentials included; the subcommand only makes the recovery
path an obvious, audited one instead of hand-written SQL. The run is recorded in the
audit log as the actor `cli` with a null `actor_user_id` and `details` carrying `via`,
`host`, `os_user`, `revoked_sessions` and `revoked_keys`.

## Fixed server limits

These are not configurable:

- `/v1/chat/completions` request bodies are limited to 4 MiB; admin JSON bodies to 1 MiB.
- HTTP `ReadHeaderTimeout` is 20 s and `IdleTimeout` 120 s. Request bodies must arrive
  within a per-route budget: 10 s for `/admin/api/login`, 30 s for other admin JSON
  routes, 5 min for document uploads and 60 s for `/v1/chat/completions`. There is no
  write timeout, so streaming responses run as long as the completion takes.
- Every response carries `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`
  and `X-Frame-Options: DENY`; `/admin/api` responses add `Cache-Control: no-store` and
  an `X-Request-Id`; the dashboard pages carry a `Content-Security-Policy` that pins the
  embedded script by hash (`script-src 'sha256-…'`, `frame-ancestors 'none'`).
- Upstream connections: dial timeout 15 s, TLS handshake 15 s, up to 100 idle connections
  (20 per host), idle timeout 90 s, at most 3 same-host redirects.
- Document parsing: PDFs are read for at most 60 s and 2000 pages; a DOCX
  `word/document.xml` may be at most 32 MiB (and at most 100× its compressed size); the
  extracted text of any document is capped at 20 MiB; one ingestion job may run 15 min.
- Graceful shutdown on `SIGINT`/`SIGTERM`: in-flight HTTP requests get 15 s, running
  ingestion jobs 30 s, then the documents this process still owned go back into the
  queue and the retention job and the pool stop. See
  [Rolling restarts and shutdown](scaling.md#rolling-restarts-and-shutdown).
- `GET /` redirects to `/admin/`.

## Deployment layouts

Ragmux ships in two container layouts. Both run the same binary and the same
least-privilege database role `ragmux_app`; they differ in where PostgreSQL runs.

| | All-in-one (default) | Split |
|---|---|---|
| Compose file | `docker-compose.yml` | `docker-compose.split.yml` |
| Image | `ghcr.io/ragmux/ragmux:<version>`, `:latest` (`Dockerfile.aio`) | `ghcr.io/ragmux/ragmux:<version>-app`, `:latest-app` (`Dockerfile`, distroless) + `pgvector/pgvector:pg17` |
| Containers | one: PostgreSQL 17 + pgvector and the gateway, supervised by `docker/aio/entrypoint.sh` | two: gateway and database |
| State | volume `ragmux-data` mounted at `/data` | volume `pgdata` (database only) |
| Required `.env` | nothing (`SECRET_KEY` recommended) | `SECRET_KEY`, `POSTGRES_PASSWORD`, `RAGMUX_DB_PASSWORD` |
| Database access | unix socket inside the container, no TCP listener | TCP inside the Compose network, password authentication |
| Use it for | single-host installs, evaluation, small teams | an existing or managed PostgreSQL, a database you operate separately, scaling the gateway independently |
| More than one gateway replica | **no** - the container bundles PostgreSQL | yes, with `docker-compose.scale.yml` on top; see [Running more than one replica](scaling.md) |

**All-in-one.** `docker compose up -d` starts one container. Its entrypoint runs as
root only long enough to prepare the volume, then starts `postgres` as the `postgres`
user and the gateway as the unprivileged `ragmux` user:

- `/data/pg` is `PGDATA` (owned by `postgres`, mode `0700`), initialised with
  `initdb --encoding=UTF8 --locale=C.UTF-8 --auth-local=trust --auth-host=reject` on the
  first start; `/data/ragmux` holds the `secret.key` fallback (`DATA_DIR`). The whole
  volume plus `SECRET_KEY` is the state to back up.
- PostgreSQL has **no TCP listener** (`listen_addresses = ''`). It accepts connections
  only on the unix socket `/var/run/postgresql`, with `local all all trust` in
  `pg_hba.conf`. Trust is acceptable here because nothing else runs in the container:
  reaching the socket already means running code inside it (or being root on the host).
  The gateway connects as `ragmux_app` over that socket
  (`postgres://ragmux_app@/ragmux?host=/var/run/postgresql&sslmode=disable`); the role
  has no password. The socket directory is a named volume (`ragmux-pgsocket`) so the
  `backup` profile can reach it; do not mount it into anything you would not trust with
  the database superuser.
- The entrypoint runs `docker/postgres-init/01-ragmux.sql` on **every** start (it is
  idempotent): extension, role, grants, and the ownership hand-over of tables that a
  `pg_restore` created as `postgres`.
- `DATABASE_URL` set → the embedded server is not started and the container behaves
  like the `-app` image (`EMBEDDED_POSTGRES=false` does the same explicitly; `true`
  forces the embedded server even with a `DATABASE_URL` in the environment, which is
  rarely what you want).
- Shutdown: `SIGTERM` (`docker compose stop`) stops the gateway first (it drains HTTP
  and finishes ingestion jobs), then PostgreSQL with `pg_ctl stop -m fast`. If either
  process dies, the entrypoint stops the other and exits non-zero so
  `restart: unless-stopped` brings the container back.
- Logs: gateway JSON lines and PostgreSQL lines prefixed with `postgres:` on the same
  `docker compose logs`.
- `ragmux-aio postgres-only` (the entrypoint's only subcommand) starts PostgreSQL alone
  on the volume; `scripts/restore.sh` uses it for restores.

Variables specific to this layout:

| Variable | Default | Meaning |
|---|---|---|
| `EMBEDDED_POSTGRES` | *(auto)* | `false` disables the embedded server (implied by `DATABASE_URL` / `DATABASE_URL_FILE`), `true` forces it. |
| `PG_SHARED_BUFFERS` | `128MB` | `shared_buffers` of the embedded server; raise it to about a quarter of the memory you give the container. Applied on every start. |
| `DB_MAX_CONNS` | `10` | The gateway's pool size, as above; the entrypoint sets `max_connections` to `DB_MAX_CONNS + 10` (at least `100`). |
| `PGDATA`, `DATA_DIR` | `/data/pg`, `/data/ragmux` | set by the image; keep them inside the `/data` volume. |

**Split.** `docker compose -f docker-compose.split.yml up -d` runs the gateway image
(`-app`, distroless, stateless) next to `pgvector/pgvector:pg17` with its own `pgdata`
volume. The database's first start runs `docker/postgres-init/01-ragmux.sh`, which
creates `ragmux_app` with `RAGMUX_DB_PASSWORD` (see
[Database privileges](#database-privileges)); the gateway connects over TCP with that
password. Use it when the database lives elsewhere anyway (managed PostgreSQL: run the
gateway image alone with `DATABASE_URL`), when you want to back up or upgrade the
database on its own schedule, or when several gateway replicas share one database.

**Upgrades.** The PostgreSQL major version inside the all-in-one image stays at **17**
for the 0.3 line; pulling a newer Ragmux image never runs `pg_upgrade`, and the
entrypoint refuses to start on a `PGDATA` of another major version. Migrations of the
Ragmux schema run at gateway start as before (back up first:
`scripts/backup.sh && docker compose pull && docker compose up -d`).

**Moving between layouts** (or from the 0.3.0 two-container file to the default one)
is a dump/restore cycle, never automatic: the volumes have different names and
directory structures. Take a dump with the old layout (`scripts/backup.sh` picks the
layout by itself), start the new layout on a fresh volume, restore with
`scripts/restore.sh --yes <dump>`, keep the same `SECRET_KEY`. Details in
[Backup and restore](backup-restore.md#moving-between-layouts). Existing deployments
that keep using the two-container file only need to add `-f docker-compose.split.yml`
(or `COMPOSE_FILE=docker-compose.split.yml` in `.env`) to their commands.

## Docker Compose variables

Both Compose files read `.env` (copy `.env.example`) and pass these through:

| Variable | Default | Used by |
|---|---|---|
| `SECRET_KEY` | *(recommended)* | gateway. The split file refuses to start without it; the default file falls back to `/data/ragmux/secret.key` in the volume and logs a warning |
| `DATABASE_URL` | *(none)* | default file only: your own PostgreSQL instead of the embedded one (see [Deployment layouts](#deployment-layouts)) |
| `EMBEDDED_POSTGRES`, `PG_SHARED_BUFFERS`, `DB_MAX_CONNS` | *(auto)*, `128MB`, `10` | default file: the embedded server (above) |
| `POSTGRES_PASSWORD` | *(required, split)* | superuser password of the split file's `postgres` service. Only the first-start init script, the `backup` profile and `scripts/backup.sh` / `restore.sh` use it; the gateway never sees it. Compose refuses to start without it (`openssl rand -hex 16`) |
| `RAGMUX_DB_PASSWORD` | *(required, split)* | password of the least-privilege role `ragmux_app` the gateway connects as; the split file builds `DATABASE_URL` from it (`postgres://ragmux_app:<password>@postgres:5432/ragmux?sslmode=disable`). Use characters that are safe in a URL and in `.env` (`openssl rand -hex 16`); see [Database privileges](#database-privileges) |
| `VERSION` | `dev` | build argument stamped into `ragmux -version` when the image is built locally |
| `ADMIN_USER`, `ADMIN_PASSWORD`, `LOG_LEVEL`, `CORS_ORIGINS`, `LOGIN_*`, `TRUST_PROXY_HEADERS`, `TRUSTED_PROXY_CIDRS`, `SECURE_COOKIES` | as above | gateway |
| `PRIVATE_UPSTREAM_ALLOWLIST`, `ALLOW_PRIVATE_UPSTREAMS` | *(empty)*, `false` | gateway; needed for Ollama and other local model servers (see [Private upstreams](#private-upstreams)) |
| `BACKUP_SCHEDULE` | `@daily` | `backup` profile (see [Backup and restore](backup-restore.md)) |
| `BACKUP_KEEP_DAYS`, `BACKUP_KEEP_WEEKS`, `BACKUP_KEEP_MONTHS` | `7`, `4`, `6` | `backup` profile |

Every other variable from the table above can be added to `.env` as well; the gateway
service loads the whole file through `env_file`. The gateway is published on
`127.0.0.1:8765` only, so it is reachable from the host but not from the network; put a
TLS-terminating reverse proxy in front of it (below) or change the mapping to
`8765:8765` deliberately. The database is never published: the embedded one has no
TCP listener at all, the split one stays inside the Compose network.

To keep the secrets out of `.env`, mount them as Compose secrets and point the `_FILE`
variables at them:

```yaml
services:
  ragmux:
    secrets: [ragmux_secret_key, ragmux_database_url]
    environment:
      SECRET_KEY_FILE: /run/secrets/ragmux_secret_key
      DATABASE_URL_FILE: /run/secrets/ragmux_database_url
secrets:
  ragmux_secret_key:
    file: ./secrets/secret_key        # 64 hex characters
  ragmux_database_url:
    file: ./secrets/database_url      # postgres://ragmux:<password>@postgres:5432/ragmux?sslmode=disable
```

The gateway reads each file once at startup and trims surrounding whitespace; the plain
variable wins when both are set. (With the default file, a `DATABASE_URL_FILE` disables
the embedded server just like `DATABASE_URL`.)

Published images, `linux/amd64` and `linux/arm64`, built by the release workflow on
every `v*` tag:

- `ghcr.io/ragmux/ragmux:<version>` (also `:<major>.<minor>` and `:latest`): the
  all-in-one image.
- `ghcr.io/ragmux/ragmux:<version>-app` (also `:<major>.<minor>-app` and
  `:latest-app`): the gateway alone on a distroless base.

From v0.3.1 on both are signed with cosign; see
[Verifying the container image](../SECURITY.md#verifying-the-container-image) for the
`cosign verify` command to run before deploying.

## Behind a reverse proxy

- **Client addresses.** Proxies *append* the address they accepted the connection from
  to `X-Forwarded-For` (nginx: `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`),
  so a request that passes through one proxy arrives as `X-Forwarded-For: <whatever the
  client sent>, <client address>`. With `TRUST_PROXY_HEADERS=true` the gateway therefore
  uses the **last** entry, which the client cannot forge, and ignores `X-Real-IP` unless
  `X-Forwarded-For` is absent. Set `TRUSTED_PROXY_CIDRS` to the proxy's network(s) so the
  headers are only honoured on connections from the proxy and so a chain of your own
  proxies (a load balancer in front of nginx) is skipped when walking the header:

  ```
  TRUST_PROXY_HEADERS=true
  TRUSTED_PROXY_CIDRS=172.18.0.0/16      # the Compose network, or the LB's range
  ```

  Without `TRUSTED_PROXY_CIDRS`, anyone who can reach the gateway directly can spoof the
  address used for login rate limiting and the audit log; keep the port bound to
  loopback or a private network in that case.
- **Cookies.** Send `X-Forwarded-Proto` from the proxy (nginx: `proxy_set_header
  X-Forwarded-Proto $scheme;`); with `TRUST_PROXY_HEADERS=true` the session cookie is then
  marked `Secure` on HTTPS requests. `SECURE_COOKIES=true` forces the flag regardless.
  The cookie is `HttpOnly`, `SameSite=Lax` and scoped to `/`.
- **Host header.** Cookie-authenticated writes to `/admin/api` are accepted only when
  the browser reports `Sec-Fetch-Site: same-origin` or, on older browsers, when the
  `Origin` host matches the request `Host`. Pass the original host through
  (`proxy_set_header Host $host;`); rewriting it breaks the dashboard's writes.
- **Body timeouts.** The gateway gives clients 10 s to deliver a login body, 30 s for
  other admin JSON, 5 min for uploads and 60 s for a chat request. nginx buffers
  request bodies itself; set `client_body_timeout` (default 60 s) to at most those
  values so a stalled client is dropped at the proxy rather than holding both.
- **Streaming.** SSE responses from `/v1/chat/completions` must not be buffered. The
  gateway already sends `X-Accel-Buffering: no`, `Cache-Control: no-cache` and
  `Connection: keep-alive`, which nginx honours for `proxy_pass` upstreams. On other
  proxies disable response buffering for `/v1/` explicitly and make sure the proxy
  read timeout is longer than your longest completion (the gateway itself does not
  time out an established stream). Streams keep no server-side state, so a fleet of
  replicas needs plain round robin and **no** session affinity; see
  [Streaming](scaling.md#streaming).
- **Upload size.** Raise the proxy's body limit to at least `MAX_UPLOAD_MB` for
  `/admin/api/rag-stores/*/documents`.
- **CORS.** If browsers call the API from another origin, set `CORS_ORIGINS`; the
  gateway answers preflight `OPTIONS` requests itself with
  `Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS` and
  `Access-Control-Allow-Headers: Authorization, Content-Type, X-Ragmux-Project`. The
  rate-limit, budget, RAG and `Retry-After` headers are listed in
  `Access-Control-Expose-Headers`, without which browser JavaScript cannot read any of
  them. With `CORS_ORIGINS` empty (the default) no preflight is ever answered, which is
  also what keeps a management key in an `Authorization` header unforgeable from a
  foreign page.

## Database privileges

The gateway does not need a superuser. It needs the `vector` extension, which only a
superuser can create, plus the right to create tables in `public`: migrations create the
schema, and ingestion creates one `chunk_embeddings_<dims>` table per embedding width at
runtime.

**All-in-one image.** The entrypoint runs `docker/postgres-init/01-ragmux.sql` as
`postgres` over the unix socket on every start: it creates the extension, the role
`ragmux_app` (`LOGIN`, no superuser, `CREATEDB` or `CREATEROLE`, `search_path =
public`, **no password**: the socket uses trust authentication and is not reachable
from outside the container), grants it `CONNECT` on the database and `CREATE, USAGE` on
schema `public`, and hands over any tables that already exist. The superuser
`postgres` is only used by the entrypoint and by the backup/restore scripts
(`docker compose exec -u postgres ragmux psql -d ragmux`).

**Split stack.** `docker-compose.split.yml` mounts `docker/postgres-init/01-ragmux.sh`
into the Postgres image's `/docker-entrypoint-initdb.d`. On the first start of an empty
data volume it runs the same `01-ragmux.sql` as the superuser (`ragmux`,
`POSTGRES_PASSWORD`), this time with the password from `RAGMUX_DB_PASSWORD` for
`ragmux_app`. The gateway's `DATABASE_URL` uses that role; the superuser password stays
with the init step, the `backup` profile and the backup/restore scripts. Init scripts
only run on an **empty** data volume: a deployment upgraded from 0.2.2 keeps its
superuser `DATABASE_URL` and keeps working. To switch it, run the SQL once by hand and
then set `RAGMUX_DB_PASSWORD`:

```bash
docker compose -f docker-compose.split.yml exec -T postgres psql -v ON_ERROR_STOP=1 -U ragmux -d ragmux \
  -v pw="$RAGMUX_DB_PASSWORD" -v db=ragmux < docker/postgres-init/01-ragmux.sql
docker compose -f docker-compose.split.yml up -d ragmux
```

The script is idempotent (`CREATE ROLE` only when missing, `ALTER ROLE` otherwise; the
password is only set when the `pw` variable is passed) and its last block changes the
owner of every table and sequence in `public` to `ragmux_app`, which is what lets later
migrations `ALTER TABLE`.

**External Postgres.** Create the extension as a superuser once, then a plain role that
may create objects in `public`; the same file works there with the role name adjusted
in the SQL, or by hand:

```sql
-- as a superuser, once per database
CREATE ROLE ragmux_app LOGIN PASSWORD '...';
CREATE DATABASE ragmux;
\c ragmux
CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;
GRANT CONNECT, TEMPORARY ON DATABASE ragmux TO ragmux_app;
GRANT CREATE, USAGE ON SCHEMA public TO ragmux_app;
ALTER ROLE ragmux_app SET search_path = public;
```

On start the gateway looks the extension up in `pg_extension` and only issues
`CREATE EXTENSION vector` when it is missing; a permission error at that point stops the
start with `the vector extension is missing and the database role may not create it; run
"CREATE EXTENSION vector" as a superuser`. Migrations and the runtime tables need
`CREATE` on the schema plus ownership of what the role created. On managed Postgres
(RDS, Cloud SQL, …) the extension is enabled through the provider's console or the
`rds_superuser`-style role, and the application role is configured the same way. After
a `pg_restore` run as another role, hand the tables back to the application role (see
[Backup and restore](backup-restore.md#scriptsrestoresh)).
