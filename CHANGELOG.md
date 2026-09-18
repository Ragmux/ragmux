# Changelog

All notable changes to Ragmux are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

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
[0.2.2]: https://github.com/ragmux/ragmux/releases/tag/v0.2.2
[0.2.1]: https://github.com/ragmux/ragmux/releases/tag/v0.2.1
[0.2.0]: https://github.com/ragmux/ragmux/releases/tag/v0.2.0
[0.1.0]: https://github.com/ragmux/ragmux/releases/tag/v0.1.0
