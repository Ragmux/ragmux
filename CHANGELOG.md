# Changelog

All notable changes to Ragmux are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- **RAG sources on chat responses.** Responses whose prompt received retrieved context
  carry `x-ragmux-rag-sources`, a JSON array of `{document_id, filename, section, page,
  score}` for the injected passages, trimmed to whole entries under 2 KB. A request may
  add `"ragmux": {"include_context": true}` to receive the same list plus the exact
  injected context block as a top-level `ragmux` object on non-streaming responses; the
  field is stripped before the upstream call and streaming responses only get the header.
- **`rag_hits` in request logs** (migration 0007): the number of passages injected per
  request, next to `rag_used`, in `/admin/api/metrics/requests` and the summary's `recent`.
- **Metrics summary options.** `GET /admin/api/metrics/summary` (and
  `/projects/{id}/metrics`) accept `compare=1` for a `previous` block covering the window
  before the current one, `days=N` (1–90) to size the `daily` series and `by_project=1`
  for a per-project `projects` breakdown under the caller's membership scope.
- **CSV export.** `GET /admin/api/metrics/requests.csv?window=&project_id=` and
  `GET /admin/api/projects/{id}/metrics.csv?window=` download up to 50 000 request logs
  of the window as `text/csv` with a dated attachment filename; cells starting with
  `=`, `+`, `-` or `@` are prefixed with a quote so spreadsheets do not evaluate them.
- **Budget forecast.** `GET /admin/api/projects/{id}/usage` gains
  `forecast.daily_exhausted_at` / `monthly_exhausted_at`: a linear projection from the
  tokens consumed in the last 60 minutes, `null` without a budget, without recent usage,
  or when the budget outlasts its window.
- `POST /admin/api/models/test` tests a connection before it is saved, with the same
  validation, private-address check and credential redaction as create; `connection_id`
  with an empty `api_key` reuses the stored key of an existing connection.
- Connection test outcomes are recorded: `POST /models/{id}/test` writes `last_test_at`,
  `last_test_ok`, `last_test_latency_ms` and `last_test_error` (redacted, at most 512
  characters), returned by the list and get endpoints (migration `0009`).
- Connections report `private_upstream`: whether `base_url` points at `localhost`, a
  loopback, private or link-local address (hostnames are classified when the connection is
  saved and cached in `upstream_private`; the provider default URL counts as public).
- `GET /admin/api/provider-types` reports `supports_tools` (false for `gemini`) and
  `supports_streaming`.
- Documents carry `page_count` (PDF pages, `null` for other formats) and
  `progress_percent` (10 after parsing, 20 after chunking, then per embedding batch up to
  100 when ready; a failed document keeps its last value).
- `POST /admin/api/rag-stores/{id}/search` returns `retrieval_latency_ms` and
  `rerank_latency_ms` (`null` when reranking did not run) next to `latency_ms`.

## [0.2.3] — 2026-09-18

### Changed
- **First-run setup replaces the generated password.** A fresh database no longer gets an
  `admin` account with a random password printed to the logs. The dashboard now shows a
  "Create the first administrator" form (username, password of at least 12 characters)
  backed by `GET`/`POST /admin/api/setup`, which only work while the `users` table is
  empty and refuse with `409` afterwards; the creation is serialised so two concurrent
  requests cannot both succeed, failed attempts count against the per-address login
  limit and the result is audited as `setup.complete`. `ADMIN_USER` / `ADMIN_PASSWORD`
  still pre-create the account for unattended installs; without `ADMIN_PASSWORD` the log
  says `no users yet: open /admin/ to create the first administrator`.

### Added
- `SECURITY.md`: supported versions, how to report a vulnerability, response targets,
  scope and hardening pointers.

### Security
- **Provider keys are bound to their connection.** `api_key_enc` is now sealed with the
  connection id as AES-GCM associated data (`key_version = 1`), so a ciphertext copied
  onto another row by someone with database write access no longer decrypts there.
  Rows written by earlier releases (`key_version = 0`) are re-sealed once on the next
  start with the same `SECRET_KEY`; `ragmux rotate-key` also writes the bound form.
- **Least-privilege database role in the bundled deployment.** `docker-compose.yml`
  now runs the gateway as `ragmux_app`, a plain `LOGIN` role (no superuser, `CREATEDB` or
  `CREATEROLE`) with `CONNECT` on the database and `CREATE, USAGE` on schema `public`,
  created on the first start by `docker/postgres-init/01-ragmux.sh` / `01-ragmux.sql`
  from the new required `RAGMUX_DB_PASSWORD`. `POSTGRES_PASSWORD` (the superuser) is
  only used by that init step, the `backup` profile and `scripts/backup.sh` /
  `restore.sh`; `restore.sh` hands the restored tables to the application role
  afterwards (`APP_ROLE`). The gateway checks `pg_extension` before
  `CREATE EXTENSION vector` and turns a permission error into
  `the vector extension is missing and the database role may not create it; run
  "CREATE EXTENSION vector" as a superuser`. Deployments upgrading from 0.2.2 keep their
  superuser `DATABASE_URL` and keep working (init scripts only run on an empty data
  volume); switching is optional but recommended: run
  `docker/postgres-init/01-ragmux.sql` once by hand, which also grants and hands over the
  existing tables, then set `RAGMUX_DB_PASSWORD`
  (see [Database privileges](docs/configuration.md#database-privileges)).
- **Per-store upload quotas.** RAG stores gain `max_documents` and `max_bytes`
  (migration `0005`, `0` = unlimited) and the instance-wide ceilings
  `MAX_DOCUMENTS_PER_STORE` / `MAX_BYTES_PER_STORE_MB`; the effective limit is the
  smaller non-zero of the two. Uploads that would exceed it are rejected before anything
  is written with `422` and `code: "store_quota"` (multi-file uploads check the running
  total; reprocessing is unaffected). Store responses carry `bytes_used`; the dashboard
  form has "Max documents" / "Max size (MB)" fields and the store table shows usage
  against the quotas. Bounds the storage and embedding cost an editor can cause.

## [0.2.2] — 2026-09-18

### Security
- **SSRF guard for provider URLs.** Outbound provider connections are dialled through a
  resolver that refuses loopback, link-local, multicast and private ranges (DNS rebinding
  included) unless `ALLOW_PRIVATE_UPSTREAMS=true` or the host is in
  `PRIVATE_UPSTREAM_ALLOWLIST` (e.g. `host.docker.internal,ollama`); `base_url` is validated
  at save time (http/https only, no credentials, query or fragment); redirects are limited to
  three hops on the same host; `HTTP_PROXY` is ignored unless private upstreams are allowed.
- Upstream transport failures are reported as classified messages (`upstream unreachable`,
  `upstream request timed out`, …) instead of raw error text, and relayed provider error
  bodies are capped at 512 characters. Redaction covers Basic auth, JWTs, Google OAuth,
  Groq/HF/xAI keys, URL credentials and the connection's own key literally, including
  Anthropic and Ollama stream errors.
- Gemini `model_name` is URL-escaped as a single path segment and validated
  (`[A-Za-z0-9._:/@-]`, no `..`), closing a path-injection issue.
- Document parsing is bounded: PDFs stop at 2000 pages and 60 s, DOCX parts over 32 MiB or
  with a compression ratio above 100:1 are rejected, extracted text is capped at 20 MiB,
  documents that split into more than `MAX_CHUNKS_PER_DOCUMENT` (20000) chunks fail before
  embedding, ingestion of one document is limited to 15 minutes, `chunk_size` must be ≥ 200
  and a full ingest queue answers `503` instead of silently dropping the document.
- Streaming provider responses are bounded by `STREAM_MAX_DURATION` (30m) and
  `STREAM_MAX_BYTES_MB` (256).
- Retrieved passages are labelled as untrusted data in the RAG and rerank prompts and
  `<context>` tags inside documents are neutralised, limiting prompt injection from uploads.
- Behind a proxy the client address is now the **last** `X-Forwarded-For` entry (the one
  the nearest proxy appended) instead of the first; `X-Real-IP` only counts when
  `X-Forwarded-For` is absent. New `TRUSTED_PROXY_CIDRS` limits proxy headers to
  connections from those networks and skips them when walking the header.
- Login: username (≤ 64) and password (≤ 1024) are validated before any database or
  bcrypt work; the 15-minute lockout is keyed on username **and** IP so a stranger cannot
  lock a user out; passwords over bcrypt's 72-byte limit are refused on create, reset and
  change instead of being truncated.
- The login response returns `token` only when the request sets `"bearer": true`; the
  dashboard uses the cookie alone. The cookie is `Secure` on TLS connections and behind a
  trusted proxy reporting `X-Forwarded-Proto: https`, not only with `SECURE_COOKIES`.
- Changing your own password revokes every other session of the account.
- Cookie-authenticated `POST`/`PUT`/`DELETE` requests must be same-origin
  (`Sec-Fetch-Site` / `Origin` vs `Host`) and JSON bodies must be
  `Content-Type: application/json` (`403` / `415`); bearer requests are unaffected.
- `/admin/api` responses are `Cache-Control: no-store` and carry `X-Request-Id`; every
  response carries `X-Content-Type-Options`, `Referrer-Policy` and `X-Frame-Options`; the
  dashboard is served with a `Content-Security-Policy` pinning its script by hash.
- Request bodies get per-route read deadlines (login 10 s, admin JSON 30 s, uploads 5 min,
  chat 60 s) so idle connections cannot hold handlers open.
- Unexpected admin errors answer `internal error (request id …)`; the gateway hides
  provider factory errors behind `model connection unavailable`.
- `POST /admin/api/models/{id}/test` now requires the editor role. Demoting, deactivating
  or deleting the last active admin is refused atomically, and a project and its members
  are created in one transaction. A panic inside a provider stream becomes a `502`
  instead of crashing the process.
- `SECRET_KEY_FILE` and `DATABASE_URL_FILE` read secrets from files; `ragmux rotate-key
  --new <hex>` re-encrypts stored provider keys with a new `SECRET_KEY`.
- Compose publishes the gateway on `127.0.0.1:8080` and requires `POSTGRES_PASSWORD`
  (no more `ragmux` default); `.env.example` ships without an admin password.
  `backup.sh` runs with `umask 077`, creates `BACKUP_DIR` with mode `700` and can put
  the key file elsewhere (`SECRET_KEY_DIR`); `restore.sh` no longer passes credentials
  on the command line. Dependabot, image provenance/SBOM and a pinned golangci-lint
  version were added to CI.

## [0.2.1] — 2026-09-18

### Fixed
- OpenAI-compatible requests carried a stray `"Extra": null` field, which OpenAI rejects with
  `Unrecognized request argument supplied: Extra`. Unknown client fields are still merged into
  the request; the container field itself is no longer serialised.
- Connection tests use a 256-token budget so reasoning models (Claude, Gemini, gemma) return
  a visible reply instead of an empty string.
- Gemini `completion_tokens` now includes reasoning ("thoughts") tokens, so
  `prompt_tokens + completion_tokens == total_tokens` as with the other providers.

Verified live against OpenAI (`gpt-4o-mini`, `text-embedding-3-small`), Anthropic
(`claude-opus-5`), Gemini (`gemini-3.6-flash`) and a local Ollama `gemma4:12b`: JSON and
streaming chat, tool calls (OpenAI, Anthropic), hybrid RAG with reranking over MD/DOCX/HTML.

## [0.2.0] — 2026-09-18

### Breaking
- **Storage moved from embedded SQLite/sqlite-vec to PostgreSQL + pgvector.** `DATABASE_URL`
  is now required and the gateway is deployed together with a Postgres instance
  (`docker-compose.yml` bundles `pgvector/pgvector:pg17`). There is no migration tool for
  a 0.1 SQLite database.
- Uploaded documents are stored in the database as `bytea`; the `uploads/` directory is gone.
- The provider credential encryption key should be supplied via `SECRET_KEY` (64 hex
  characters). The on-disk `DATA_DIR/secret.key` fallback remains for development only.
- `GET /admin/api/system` returns `database` (Postgres, pgvector and migration versions,
  size), `backup` and `secret_key_source` instead of `data_dir`, `db_size_bytes` and
  `vector_engine`.
- Proxy headers are no longer trusted by default: the client IP for login limits and the
  audit log is the TCP peer address unless `TRUST_PROXY_HEADERS=true`.
- License changed to AGPL-3.0-or-later.

### Added
- Roles (`admin`, `editor`, `viewer`), a user management API (`/admin/api/users…`) and a
  **Users** dashboard tab; project membership (`member_ids`, `/projects/{id}/members`).
  Non-admins only see their projects and foreign project ids answer `404`. Existing
  accounts become `admin` on upgrade.
- Login rate limiting per username and per IP plus a lockout window, stored in Postgres
  (`LOGIN_RATE_LIMIT_PER_MIN`, `LOGIN_USER_LIMIT_PER_MIN`, `LOGIN_LOCKOUT_FAILURES`,
  `LOGIN_LOCKOUT_MINUTES`); `429` with `Retry-After`.
- Audit log of logins and every management action, `GET /admin/api/audit` and an **Audit**
  dashboard tab.
- `GET /admin/api/me` and the login response include `role`, `is_active` and `last_login_at`.
- Per-project requests-per-minute / tokens-per-minute limits and daily / monthly token
  budgets (`rate_limit_rpm`, `rate_limit_tpm`, `budget_daily_tokens`, `budget_monthly_tokens`;
  UTC windows shared across replicas). Over the limit the gateway answers `429` with
  `Retry-After`, OpenAI-style `x-ratelimit-*` and `x-ragmux-budget-*-remaining` headers;
  `GET /admin/api/projects/{id}/usage` shows live counters, metrics summaries gain
  `rate_limited`, and the dashboard shows limits, usage meters and a 429 tile.
- Retrieval: hybrid vector + PostgreSQL full-text search fused with reciprocal rank fusion
  (`search_mode`, `fts_config`), section- and page-aware chunks with contextual embeddings
  (`contextual_chunks`), optional LLM reranking through the project's chat model (`rerank`,
  `rerank_candidates`), a cosine distance threshold (`max_distance`), DOCX and HTML
  ingestion, the `x-ragmux-rag-hits` response header, `POST /admin/api/rag-stores/{id}/reprocess`
  and per-call search overrides (`mode`, `rerank`, `max_distance`) with rank and score
  details in the dashboard.
- Backup and restore: `scripts/backup.sh` (`pg_dump -Fc`, verification, rotation, optional
  `SECRET_KEY` export), `scripts/restore.sh` (`--yes` guard, stops and starts the gateway,
  waits for `/healthz`), `make backup` / `make restore`, a scheduled `backup` Compose
  profile (`prodrigestivill/postgres-backup-local`), a `backup` block in
  `GET /admin/api/system` and the `ragmux -version` flag.
- Retention job (`internal/maintenance`): request logs older than `LOG_RETENTION_DAYS`
  (default 90) and audit entries older than `AUDIT_RETENTION_DAYS` (default 365) are
  deleted hourly (`0` keeps them forever), together with day-old login attempts, expired
  sessions and stale usage counters.
- Native Ollama adapter: the `ollama` provider type uses `/api/chat` (NDJSON streaming,
  tools and tool calls, `keep_alive` / `num_ctx` / `options` passthrough, optional bearer
  key for proxies) and `/api/embed`; `custom_openai` still covers Ollama's `/v1` shim.
- Request logs record `499` when the client disconnects mid-completion; the upstream call
  is cancelled in both the JSON and the streaming path.
- Upstream error messages are scrubbed of API-key-looking strings before they reach logs,
  the request log or clients.
- `TRUST_PROXY_HEADERS` gates the use of `X-Forwarded-For` / `X-Real-IP` as the client address.
- Graceful shutdown: HTTP drains for 15 s, in-flight ingestion jobs get 30 s, then the
  retention job and the pool stop; interrupted ingestion resumes on the next start.
- Gateway test suite (auth, validation, error relay, streaming failures, disconnects,
  degraded RAG, rate limit headers), Ollama adapter and retention tests, golangci-lint v2
  in CI, and a release workflow publishing `ghcr.io/ragmux/ragmux` for `linux/amd64` and
  `linux/arm64`.
- Documentation split into `docs/` (configuration, API reference, retrieval, users and
  limits, providers, backup and restore).

### Changed
- `docker-compose.dev.yml` and `make dev-db` start a local pgvector Postgres for tests;
  tests read `TEST_DATABASE_URL` and use a throwaway schema each.
- Migrations run at startup under an advisory lock so several replicas can start against
  the same database.

### Known limitations
- No migration tool from the 0.1 SQLite database to PostgreSQL; set up 0.2 fresh and
  re-create connections, projects and documents.
- Gemini connections do not support tool calling (`400` when `tools` is sent).
- No OCR: scanned PDFs without a text layer are rejected.
- No Prometheus metrics endpoint; metrics are available through the admin API and dashboard.
- No SSO / OIDC; authentication is username and password only.

## [0.1.0] — 2026-09-18

Initial release: single-container gateway with SQLite + sqlite-vec, OpenAI-compatible proxy
for OpenAI, Anthropic, Gemini, DeepSeek, Ollama and custom endpoints, RAG over PDF/TXT/MD,
projects with `sk-proj-` keys, metrics and an embedded dashboard.

[Unreleased]: https://github.com/ragmux/ragmux/compare/v0.2.0...HEAD
[0.2.3]: https://github.com/ragmux/ragmux/releases/tag/v0.2.3
[0.2.2]: https://github.com/ragmux/ragmux/releases/tag/v0.2.2
[0.2.1]: https://github.com/ragmux/ragmux/releases/tag/v0.2.1
[0.2.0]: https://github.com/ragmux/ragmux/releases/tag/v0.2.0
[0.1.0]: https://github.com/ragmux/ragmux/releases/tag/v0.1.0
