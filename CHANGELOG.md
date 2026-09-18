# Changelog

All notable changes to Ragmux are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased] — 0.2.0

### Breaking
- **Storage moved from embedded SQLite/sqlite-vec to PostgreSQL + pgvector.** A `DATABASE_URL`
  is now required and the gateway is deployed together with a Postgres instance (see
  `docker-compose.yml`). There is no migration tool from the 0.1 SQLite database.
- Uploaded documents are stored in the database; the `uploads/` directory is gone.
- Provider credential encryption key should be supplied via `SECRET_KEY`; the on-disk
  `secret.key` fallback remains for development.

### Added
- Roles (`admin`, `editor`, `viewer`), user management API (`/admin/api/users…`) and a
  **Users** dashboard tab, project membership (`member_ids`, `/projects/{id}/members`).
  Non-admins only see their projects; foreign project ids answer 404. Existing accounts
  become `admin` on upgrade.
- Login rate limiting per username and per IP plus a lockout window, stored in Postgres
  (`LOGIN_RATE_LIMIT_PER_MIN`, `LOGIN_USER_LIMIT_PER_MIN`, `LOGIN_LOCKOUT_FAILURES`,
  `LOGIN_LOCKOUT_MINUTES`); `429` with `Retry-After`.
- Audit log of logins and every management action, `GET /admin/api/audit` and an **Audit**
  dashboard tab.
- `GET /admin/api/me` and the login response include `role`, `is_active` and `last_login_at`.
- Per-project requests-per-minute / tokens-per-minute limits and daily / monthly token budgets
  (`rate_limit_rpm`, `rate_limit_tpm`, `budget_daily_tokens`, `budget_monthly_tokens`; UTC
  windows shared across replicas). Over the limit the gateway answers `429` with
  `Retry-After`, OpenAI-style `x-ratelimit-*` and `x-ragmux-budget-*-remaining` headers;
  `GET /admin/api/projects/{id}/usage` shows live counters, metrics summaries gain
  `rate_limited`, and the dashboard shows limits, usage meters and a 429 tile.
- RAG: hybrid vector + PostgreSQL full-text search fused with reciprocal rank fusion
  (`search_mode`, `fts_config`), section/page aware chunks with contextual embeddings
  (`contextual_chunks`), optional LLM reranking through the project's chat model (`rerank`,
  `rerank_candidates`), a cosine distance threshold (`max_distance`), DOCX and HTML ingestion,
  `x-ragmux-rag-hits` response header, `POST /admin/api/rag-stores/{id}/reprocess` and search
  overrides (`mode`, `rerank`, `max_distance`) with rank/score details in the dashboard.
- Backup and restore scripts and documentation.
- Data retention jobs, gateway test suite, native Ollama adapter, CI (lint, vet, tests, vuln scan)
  and release workflow publishing `ghcr.io/ragmux/ragmux`.

### Changed
- License: AGPL-3.0-or-later.
- `GET /admin/api/system` now returns `database` (Postgres, pgvector and migration versions,
  size) and `secret_key_source` instead of `data_dir`, `db_size_bytes` and `vector_engine`.
- `docker-compose.dev.yml` and `make dev-db` start a local pgvector Postgres for tests.

## [0.1.0] — 2026-09-18

Initial release: single-container gateway with SQLite + sqlite-vec, OpenAI-compatible proxy
for OpenAI, Anthropic, Gemini, DeepSeek, Ollama and custom endpoints, RAG over PDF/TXT/MD,
projects with `sk-proj-` keys, metrics and an embedded dashboard.
