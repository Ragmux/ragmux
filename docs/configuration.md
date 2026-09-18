# Configuration

Ragmux is configured entirely through environment variables. They are read once at
startup (`internal/config/config.go`); an invalid value makes the binary print
`config: <reason>` and exit with status `2`.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | *(required)* | PostgreSQL connection string, e.g. `postgres://user:pass@host:5432/ragmux?sslmode=require`. The `vector` extension is created on first start if the role may do so; otherwise run `CREATE EXTENSION vector` beforehand. |
| `SECRET_KEY` | *(file fallback)* | 32-byte AES-256-GCM key as 64 hex characters that encrypts provider API keys at rest. Generate once with `openssl rand -hex 32` and keep it with your backups. Any other length or non-hex value is rejected. When unset, the gateway generates and reads `DATA_DIR/secret.key` and logs a warning; that fallback is meant for local development only. |
| `DB_MAX_CONNS` | `10` | Connection pool size (must be `>= 1`). |
| `DATA_DIR` | `/app/data` | Only used for the `secret.key` fallback when `SECRET_KEY` is unset. |
| `PORT` | `8080` | HTTP listen port (`1`-`65535`). Also read by `-healthcheck`. |
| `ADMIN_USER` | `admin` | Username of the account created on first start (only when the `users` table is empty). |
| `ADMIN_PASSWORD` | *(random)* | Password of that account. When unset, a 20-character random password is printed **once** to stderr; find it with `docker logs ragmux 2>&1 | grep -A3 "Initial admin"`. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON lines on stdout; unknown values fall back to `info`. |
| `CORS_ORIGINS` | *(none)* | Comma-separated browser origins allowed to call the API (`*` allows all). Unset means no CORS headers at all. |
| `SESSION_TTL` | `24h` | Dashboard session lifetime (Go duration such as `12h`, `30m`). |
| `UPSTREAM_TIMEOUT` | `5m` | Timeout for a non-streaming provider call. Streaming calls use it for the connect and response-header phase only. |
| `INGEST_WORKERS` | `2` | Parallel document ingestion jobs (`>= 1`). |
| `MAX_UPLOAD_MB` | `50` | Maximum size of one document upload request in MiB (`>= 1`). |
| `MAX_CHUNKS_PER_DOCUMENT` | `20000` | A document that splits into more chunks than this is marked `failed` before anything is embedded (`>= 1`). Bounds the memory and embedding cost of one document. |
| `ALLOW_PRIVATE_UPSTREAMS` | `false` | `true` lets provider `base_url`s point at loopback, link-local and private networks and re-enables `HTTP_PROXY`/`HTTPS_PROXY` for provider calls. See [Private upstreams](#private-upstreams). |
| `PRIVATE_UPSTREAM_ALLOWLIST` | *(empty)* | Comma-separated hostnames (case-insensitive) that may resolve to private addresses while `ALLOW_PRIVATE_UPSTREAMS` stays `false`, e.g. `host.docker.internal,ollama`. |
| `STREAM_MAX_DURATION` | `30m` | Wall-time limit for one streaming provider response (Go duration). The stream ends with a `504 timeout` error when it is reached. |
| `STREAM_MAX_BYTES_MB` | `256` | Maximum bytes read from one streaming provider response in MiB (`>= 1`). |
| `SECURE_COOKIES` | `false` | `true` marks the `ragmux_session` cookie `Secure`. Set it when the dashboard is served over HTTPS. |
| `TRUST_PROXY_HEADERS` | `false` | `true` takes the client address from `X-Real-IP` or the first `X-Forwarded-For` entry (used by login limits and the audit log). Enable only behind a reverse proxy that overwrites those headers. |
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
- HTTP `ReadHeaderTimeout` is 20 s and `IdleTimeout` 120 s.
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
| `POSTGRES_PASSWORD` | `ragmux` | the bundled `postgres` service and the `DATABASE_URL` Compose builds for the gateway (`postgres://ragmux:<password>@postgres:5432/ragmux?sslmode=disable`) |
| `VERSION` | `dev` | build argument stamped into `ragmux -version` when the image is built locally |
| `ADMIN_USER`, `ADMIN_PASSWORD`, `LOG_LEVEL`, `CORS_ORIGINS`, `LOGIN_*` | as above | gateway |
| `PRIVATE_UPSTREAM_ALLOWLIST`, `ALLOW_PRIVATE_UPSTREAMS` | *(empty)*, `false` | gateway; needed for Ollama and other local model servers (see [Private upstreams](#private-upstreams)) |
| `BACKUP_SCHEDULE` | `@daily` | `backup` profile (see [Backup and restore](backup-restore.md)) |
| `BACKUP_KEEP_DAYS`, `BACKUP_KEEP_WEEKS`, `BACKUP_KEEP_MONTHS` | `7`, `4`, `6` | `backup` profile |

Every other variable from the table above can be added to `.env` as well; the gateway
service loads the whole file through `env_file`. The gateway listens on `8080:8080`,
the database is not published. All state lives in the `pgdata` volume plus `SECRET_KEY`.

Published images: `ghcr.io/ragmux/ragmux:<version>` (also `:<major>.<minor>` and
`:latest`, `linux/amd64` and `linux/arm64`), built by the release workflow on every
`v*` tag.

## Behind a reverse proxy

- **Client addresses.** Set `TRUST_PROXY_HEADERS=true` only when the proxy sets
  `X-Forwarded-For` / `X-Real-IP` itself and strips client-supplied values; otherwise
  anyone can spoof the address used for login rate limiting and the audit log.
- **Cookies.** Set `SECURE_COOKIES=true` when the dashboard is reached over HTTPS so
  the session cookie is never sent over plain HTTP. The cookie is `HttpOnly`,
  `SameSite=Lax` and scoped to `/`.
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
