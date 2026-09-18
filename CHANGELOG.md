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
- Roles (`admin`, `editor`, `viewer`), user management API and dashboard, project membership.
- Login rate limiting and account lockout; audit log.
- Per-project requests-per-minute / tokens-per-minute limits and daily / monthly token budgets
  with OpenAI-style `x-ratelimit-*` headers.
- RAG: hybrid BM25 + vector search (RRF), section/page aware chunks, optional LLM reranking,
  DOCX and HTML ingestion, similarity threshold.
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
