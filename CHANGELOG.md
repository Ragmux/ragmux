# Changelog

All notable changes to Ragmux are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

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
[0.2.1]: https://github.com/ragmux/ragmux/releases/tag/v0.2.1
[0.2.0]: https://github.com/ragmux/ragmux/releases/tag/v0.2.0
[0.1.0]: https://github.com/ragmux/ragmux/releases/tag/v0.1.0
