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
| `DATA_DIR` | `/app/data` | Only used for the `secret.key` fallback when `SECRET_KEY` is unset. |
| `PORT` | `8080` | HTTP listen port (`1`-`65535`). Also read by `-healthcheck`. |
| `ADMIN_USER` | `admin` | Username of the administrator pre-created on first start when `ADMIN_PASSWORD` is set and the `users` table is empty. Ignored otherwise. |
| `ADMIN_PASSWORD` | *(none)* | Set it for unattended installs: the account is created once with this password and the log says `admin user created from ADMIN_PASSWORD`. When unset, nothing is created; the log says `no users yet: open /admin/ to create the first administrator` and the dashboard shows the first-run setup form (see [`/setup`](api.md#first-run-setup)) until the first account exists. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON lines on stdout; unknown values fall back to `info`. |
| `CORS_ORIGINS` | *(none)* | Comma-separated browser origins allowed to call the API (`*` allows all). Unset means no CORS headers at all. |
| `SESSION_TTL` | `24h` | Dashboard session lifetime (Go duration such as `12h`, `30m`). |
| `UPSTREAM_TIMEOUT` | `5m` | Timeout for a non-streaming provider call. Streaming calls use it for the connect and response-header phase only. |
| `INGEST_WORKERS` | `2` | Parallel document ingestion jobs (`>= 1`). |
| `MAX_UPLOAD_MB` | `50` | Maximum size of one document upload request in MiB (`>= 1`). |
| `MAX_CHUNKS_PER_DOCUMENT` | `20000` | A document that splits into more chunks than this is marked `failed` before anything is embedded (`>= 1`). Bounds the memory and embedding cost of one document. |
| `MAX_DOCUMENTS_PER_STORE` | `0` | Instance-wide ceiling on the documents one RAG store may hold (`0` = unlimited). A store's own `max_documents` can only lower it; uploads over the limit get `422 store_quota`. See [Quotas](rag.md#quotas). |
| `MAX_BYTES_PER_STORE_MB` | `0` | Instance-wide ceiling on the summed upload size of one RAG store in MiB (`0` = unlimited); combined with the store's `max_bytes` the same way. |
| `ALLOW_PRIVATE_UPSTREAMS` | `false` | `true` lets provider `base_url`s point at loopback, link-local and private networks and re-enables `HTTP_PROXY`/`HTTPS_PROXY` for provider calls. See [Private upstreams](#private-upstreams). |
| `PRIVATE_UPSTREAM_ALLOWLIST` | *(empty)* | Comma-separated hostnames (case-insensitive) that may resolve to private addresses while `ALLOW_PRIVATE_UPSTREAMS` stays `false`, e.g. `host.docker.internal,ollama`. |
| `STREAM_MAX_DURATION` | `30m` | Wall-time limit for one streaming provider response (Go duration). The stream ends with a `504 timeout` error when it is reached. |
| `STREAM_MAX_BYTES_MB` | `256` | Maximum bytes read from one streaming provider response in MiB (`>= 1`). |
| `SECURE_COOKIES` | `false` | `true` marks the `ragmux_session` cookie `Secure` unconditionally. The flag is set anyway when the request arrived over TLS, or when `TRUST_PROXY_HEADERS` is on and the proxy sends `X-Forwarded-Proto: https`. |
| `TRUST_PROXY_HEADERS` | `false` | `true` takes the client address from the **last** `X-Forwarded-For` entry (the one appended by the nearest proxy), or from `X-Real-IP` when there is no `X-Forwarded-For`. Used by login limits, the audit log and the `Secure` cookie flag. Enable only behind a reverse proxy; see [Behind a reverse proxy](#behind-a-reverse-proxy). |
| `TRUSTED_PROXY_CIDRS` | *(none)* | Comma-separated networks (`10.0.0.0/8,172.16.0.0/12`, single addresses allowed). When set, proxy headers are honoured only for connections from these networks, and `X-Forwarded-For` is walked from the right past addresses inside them, so the first hop that is not one of your proxies wins. |
| `LOGIN_RATE_LIMIT_PER_MIN` | `10` | Failed logins allowed per minute from one IP address (`0` disables). |
| `LOGIN_USER_LIMIT_PER_MIN` | `5` | Failed logins allowed per minute for one username (`0` disables). |
| `LOGIN_LOCKOUT_FAILURES` | `20` | Failures within `LOGIN_LOCKOUT_MINUTES` that lock a username out (`0` disables). |
| `LOGIN_LOCKOUT_MINUTES` | `15` | Lockout window in minutes. |
| `LOG_RETENTION_DAYS` | `90` | Request logs older than this many days are deleted by the hourly retention job (`0` keeps them forever). |
| `AUDIT_RETENTION_DAYS` | `365` | Audit entries older than this many days are deleted (`0` keeps them forever). |

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
database), creates the first admin if needed, resumes unfinished document ingestion and
starts listening.

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
- The ingestion queue holds 1024 documents; uploads beyond that get `503`.
- Graceful shutdown on `SIGINT`/`SIGTERM`: in-flight HTTP requests get 15 s, running
  ingestion jobs 30 s, then the retention job and the pool stop. Documents whose
  ingestion was cut short resume on the next start.
- `GET /` redirects to `/admin/`.

## Docker Compose variables

`docker-compose.yml` reads `.env` (copy `.env.example`) and passes these through:

| Variable | Default | Used by |
|---|---|---|
| `SECRET_KEY` | *(required)* | gateway; Compose refuses to start without it |
| `POSTGRES_PASSWORD` | *(required)* | superuser password of the bundled `postgres` service. Only the first-start init script, the `backup` profile and `scripts/backup.sh` / `restore.sh` use it; the gateway never sees it. Compose refuses to start without it (`openssl rand -hex 16`) |
| `RAGMUX_DB_PASSWORD` | *(required)* | password of the least-privilege role `ragmux_app` the gateway connects as; Compose builds `DATABASE_URL` from it (`postgres://ragmux_app:<password>@postgres:5432/ragmux?sslmode=disable`). Use characters that are safe in a URL and in `.env` (`openssl rand -hex 16`); see [Database privileges](#database-privileges) |
| `VERSION` | `dev` | build argument stamped into `ragmux -version` when the image is built locally |
| `ADMIN_USER`, `ADMIN_PASSWORD`, `LOG_LEVEL`, `CORS_ORIGINS`, `LOGIN_*`, `TRUST_PROXY_HEADERS`, `TRUSTED_PROXY_CIDRS`, `SECURE_COOKIES` | as above | gateway |
| `PRIVATE_UPSTREAM_ALLOWLIST`, `ALLOW_PRIVATE_UPSTREAMS` | *(empty)*, `false` | gateway; needed for Ollama and other local model servers (see [Private upstreams](#private-upstreams)) |
| `BACKUP_SCHEDULE` | `@daily` | `backup` profile (see [Backup and restore](backup-restore.md)) |
| `BACKUP_KEEP_DAYS`, `BACKUP_KEEP_WEEKS`, `BACKUP_KEEP_MONTHS` | `7`, `4`, `6` | `backup` profile |

Every other variable from the table above can be added to `.env` as well; the gateway
service loads the whole file through `env_file`. The gateway is published on
`127.0.0.1:8080` only, so it is reachable from the host but not from the network; put a
TLS-terminating reverse proxy in front of it (below) or change the mapping to
`8080:8080` deliberately. The database is not published. All state lives in the
`pgdata` volume plus `SECRET_KEY`.

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
variable wins when both are set.

Published images: `ghcr.io/ragmux/ragmux:<version>` (also `:<major>.<minor>` and
`:latest`, `linux/amd64` and `linux/arm64`), built by the release workflow on every
`v*` tag.

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
  time out an established stream).
- **Upload size.** Raise the proxy's body limit to at least `MAX_UPLOAD_MB` for
  `/admin/api/rag-stores/*/documents`.
- **CORS.** If browsers call the API from another origin, set `CORS_ORIGINS`; the
  gateway answers preflight `OPTIONS` requests itself with
  `Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS` and
  `Access-Control-Allow-Headers: Authorization, Content-Type`.

## Database privileges

The gateway does not need a superuser. It needs the `vector` extension, which only a
superuser can create, plus the right to create tables in `public`: migrations create the
schema, and ingestion creates one `chunk_embeddings_<dims>` table per embedding width at
runtime.

**Bundled Compose stack.** `docker-compose.yml` mounts `docker/postgres-init/01-ragmux.sh`
into the Postgres image's `/docker-entrypoint-initdb.d`. On the first start of an empty
data volume it runs `docker/postgres-init/01-ragmux.sql` as the superuser (`ragmux`,
`POSTGRES_PASSWORD`): it creates the extension, the role `ragmux_app` with the password
from `RAGMUX_DB_PASSWORD` (`LOGIN`, no superuser, `CREATEDB` or `CREATEROLE`,
`search_path = public`), grants it `CONNECT` on the database and `CREATE, USAGE` on
schema `public`, and hands over any tables that already exist. The gateway's
`DATABASE_URL` uses that role; the superuser password stays with the init step, the
`backup` profile and the backup/restore scripts. Init scripts only run on an **empty**
data volume: a deployment upgraded from 0.2.2 keeps its superuser `DATABASE_URL` and
keeps working. To switch it, run the SQL once by hand and then set `RAGMUX_DB_PASSWORD`:

```bash
docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U ragmux -d ragmux \
  -v pw="$RAGMUX_DB_PASSWORD" -v db=ragmux < docker/postgres-init/01-ragmux.sql
docker compose up -d ragmux
```

The script is idempotent (`CREATE ROLE` only when missing, `ALTER ROLE` otherwise) and
its last block changes the owner of every table and sequence in `public` to
`ragmux_app`, which is what lets later migrations `ALTER TABLE`.

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
