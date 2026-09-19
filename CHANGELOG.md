# Changelog

All notable changes to Ragmux are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed
- **The request id is generated, never taken from `X-Request-Id`.** Ragmux previously used
  chi's `middleware.RequestID`, which starts from the client's `X-Request-Id` header and
  only generates an id when it is absent. That id reaches the `ragmux.request_id` span
  attribute exported to your collector, the `X-Request-Id` response header and the request
  log, so a client chose all three. Filtering the header by shape was considered and does
  not work: a Ragmux gateway key, an AWS key and a bare hex token are the same shape as a
  generated id, so the header is no longer read at all.
  **If a proxy in front of Ragmux generates `X-Request-Id` and you correlate on it**, that
  correlation stops here: Ragmux now answers with its own id. Configure the proxy to emit
  `traceparent` and set `TRACING_TRUST_INCOMING=true` to join the two sides **in your
  tracing backend** — see [the trust gate](docs/observability.md#the-trust-gate) for what
  that opens up. Note this recovers trace correlation, not log correlation: Ragmux log
  lines carry `req_id`, not a trace id, so a proxy log line and a Ragmux log line still
  cannot be matched on a shared field. Proxies that mint `X-Request-Id` (nginx
  `$request_id`, HAProxy `unique-id`) do not emit `traceparent` on their own.
- The `method` label on `ragmux_http_requests_total` and
  `ragmux_http_request_duration_seconds`, and the `type` label on
  `ragmux_gateway_errors_total`, are now drawn from a closed set; unrecognised values are
  counted under `other`. Both were bounded by traffic rather than by the size of the
  installation — an invented HTTP verb or an upstream returning a fresh error type per
  response minted a series per request. Metric names, label names and buckets are
  unchanged, so existing dashboards keep working.
- `ragmux_tracing_spans_dropped_total` now also counts spans that end after the exporter
  has stopped, which previously vanished silently. The count is logged at shutdown as well.

- **`custom_openai` ships without a price row.** The built-in table used to price every
  `custom_openai` model at 0, which reported a paid endpoint's real bill as `$0.00` with
  `cost_source: "builtin"` — a priced zero rather than the missing price it is. Those
  models are now `cost_source: "none"` until you add a row (**Prices** tab, or
  `POST /admin/api/prices`), and the shipped table's `version` moved to `2`. `ollama`
  keeps its free catch-all: it runs on your own hardware. **On upgrade**, migration
  `0015` deletes that one seeded row, so a `custom_openai` connection that was showing
  `$0.00` starts showing no cost at all; add your own row to keep a figure. The delete
  is limited to that pattern and to rows still marked `builtin` — one you edited is
  `user`, keeps your price and is left alone. Nothing else about the table changes: the
  seed still only inserts and refreshes, and retiring a row stays a numbered migration
  rather than something an upgrade decides on its own. **Rolling back to 0.4.0 brings
  the row back for good:** that binary's shipped table still lists it, so its seed
  re-inserts it, and coming forward again finds `0015` already recorded and does not
  re-run. Built-in rows cannot be deleted through the API, so the way to correct it is
  to **edit** it — `PUT /admin/api/prices/{id}` flips a built-in row to `user`, which
  lets you give the catch-all the real price of the endpoint it covers.
- **The streaming usage trailer follows `include_usage` on every provider.** The final
  usage-only chunk (`"choices": []` with `usage`) is now sent only to a client that set
  `stream_options: {"include_usage": true}`, the way OpenAI behaves. Previously
  `anthropic`, `gemini` and `ollama` always sent it, and the OpenAI-compatible path
  relayed the one it requests upstream, so clients that index `chunk.choices[0]` on every
  chunk could break. **If you read token counts off a stream, set `include_usage`**; the
  request log, budgets and cost estimate are unaffected either way, because the gateway
  still asks its upstream for the counts.

### Fixed
- A document whose file type is unsupported no longer quotes the rejected extension into
  its error, which put a piece of a user-supplied filename on the `ingest.document` span.
- The Go runtime gauges shared one `runtime/metrics` sample buffer across every gauge and
  every concurrent scrape, so two overlapping scrapes could race and report one another's
  numbers.
- **`PG_SEARCH_TOKENIZER` stemming works.** `en_stem` — the only stemming value the
  documentation named — is not a tokenizer *type* `pg_search` 0.25 knows, so it rejected
  the BM25 index DDL and every rag store set to the `pg_search` backend fell back to
  `pgvector` for the life of the process: retrieval kept answering, BM25 never ran.
  Stemming now reaches `pg_search` as the `default` tokenizer carrying a Snowball
  stemmer, spelled `<iso-639-1>_stem` for twenty languages
  (`ar cs da de el en es fi fr hu it nl no pl pt ro ru sv ta tr`); a code outside that
  set is refused at startup instead of degrading silently. Pointing the setting somewhere
  new on an installation that already has the index still keeps the old analyser — the
  DDL is `CREATE INDEX IF NOT EXISTS` — but that is reported as a mismatch naming both
  the analyser the index carries and the one the configuration asks for, and `bm25 index
  ready` now logs the analyser queries actually use rather than the configured one.

### Upgrade notes
- **An installation running `PG_SEARCH_TOKENIZER=<code>_stem` never built its BM25
  index.** After this fix the first hybrid search of a `pg_search` store builds it, over
  every row in `chunks`, inside that HTTP request. The build takes a `SHARE` lock, so
  ingest writes to `chunks` block until it finishes, and it is not `CONCURRENTLY`: if the
  client gives up, the build is rolled back and the next request starts over. On a large
  corpus that is minutes. Build it ahead of the upgrade instead, without blocking writes —
  the statement must match what the gateway would create, or it will be treated as drift:

  ```sql
  CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chunks_bm25 ON chunks
  USING bm25 (id, content, rag_store_id, document_id)
  WITH (key_field = 'id', text_fields = '{"content":{"tokenizer":{"type":"default","stemmer":"English"},"record":"position"}}',
        numeric_fields = '{"rag_store_id":{"fast":true},"document_id":{"fast":true}}');
  ```

  Substitute the Snowball language for the code you run (`cs` → `Czech`, `tr` → `Turkish`,
  …), or `{"type":"default"}` with no `stemmer` for the default analyser. Installations on
  the default tokenizer already have the index and are unaffected.
- **A dump from a ParadeDB server needs a TOC filter to restore onto a plain pgvector
  one.** Dropping `idx_chunks_bm25` first is not enough; see
  [Backup and restore](docs/backup-restore.md#restoring-a-paradedb-dump-onto-a-plain-pgvector-server).

## [0.4.0] — 2026-09-19

### Added
- **User-owned API keys** (`api_keys`, migration `0010`): a key now belongs to a dashboard
  user and comes in two kinds. A **gateway** key (`sk-user-…`) calls `/v1` on the projects
  it is granted; a **management** key (`sk-mgmt-…`) calls `/admin/api` within its scopes,
  replacing the 24-hour session token scripts had to borrow. Both carry an optional
  `expires_at`, can be revoked, record `last_used_at`, and may set their own rate limits
  and budgets **under** the project's. A key never outranks its owner: its effective
  permission is its scopes intersected with the owner's live role, so demoting or
  deactivating a user immediately narrows every key they hold. A project's own
  `sk-proj-…` key is unchanged and keeps working exactly as before. Managed from the new
  **Keys** tab and `/admin/api/keys`, documented in
  [Users, roles and limits](docs/users-and-limits.md#api-keys).
- **`X-Ragmux-Project`**: a gateway key granting several projects selects one per request
  with this header, falling back to the key's default project and then to its single
  grant. There is deliberately no `"project/model"` convention — model names legitimately
  contain slashes, so the split would be ambiguous and would silently reroute existing
  clients.
- **`ragmux reset-password <username>`**: resets a dashboard password straight in the
  database when the last administrator is locked out. Needs `DATABASE_URL` and no running
  gateway, takes the new password from `--generate` or `--stdin` (never a flag, which
  would leak into `ps` and shell history), and writes an audit row attributed to `cli`, so
  a console reset appears in the dashboard next to the ones done through the UI.
- **A three-step first-run wizard**: the administrator form is now followed by an optional
  first model connection and an optional first project with its key, so a fresh install
  does not open on an empty dashboard. Steps two and three call the ordinary authenticated
  endpoints and add no unauthenticated surface.
- **Gemini tool calling.** `tools` and `tool_choice` are translated to
  `functionDeclarations` and `toolConfig`, `role:"tool"` messages become
  `functionResponse` parts (correlated by name, which is all Gemini offers), and function
  calls are returned as OpenAI `tool_calls` in both the blocking and streaming paths.
  Tool schemas are rewritten into the OpenAPI subset Gemini accepts: unknown keywords are
  dropped, `const`, `oneOf`/`allOf` and nullable unions are rewritten, and local `$ref`s
  are inlined. It is lossy by design — forwarding a schema unchanged fails on essentially
  every OpenAI strict-mode tool, and the relayed error tells an SDK author nothing. The
  exact whitelist is in [Providers](docs/providers.md#gemini).
- **Remote images for Ollama and Gemini**: an `image_url` the upstream cannot fetch is now
  downloaded and inlined by the gateway over the **same** hardened client provider calls
  use, so the SSRF dialer and redirect policy apply to image hosts too. Bounded by
  `IMAGE_FETCH_MAX_MB`, `IMAGE_FETCH_TIMEOUT` and `IMAGE_FETCH_MAX_PER_REQUEST`, cached in
  memory, and switched off entirely with `IMAGE_FETCH=false`.
- **Prompt caching passthrough**: Anthropic `cache_control` is carried through on content
  parts, tools and the system block, and every provider's cache counters are reported back
  in one shape — `usage.prompt_tokens_details` with `cached_tokens` and
  `cache_creation_tokens`. OpenAI's caching is automatic and has nothing to send; only its
  usage is surfaced.
- **Cost estimation** (`model_prices`, migration `0011`): an embedded price table, seeded
  into the database and editable from the new **Prices** tab, turns a finished request's
  token counts into `cost_micros` on its log row, pricing cached reads and cache writes at
  their own rates. Editing a shipped row makes it yours and upgrades never overwrite it
  again. Summaries, the daily series, the by-project table and the CSV export all carry
  the figure. **It is an estimate, not a bill.**
- **A ParadeDB search backend**: a RAG store can set `search_backend: "pg_search"` and get
  real BM25 ranking for the lexical half of a hybrid search instead of Postgres full-text
  ranking, fused with the vector half by the same reciprocal-rank formula. Switching
  needs **no reprocessing** — the lexical side is derived from the stored chunk text — and
  the index is built lazily on first use. A server without the extension falls back to the
  existing backend with a warning rather than failing. Ships as
  `docker-compose.paradedb.yml` and a `:<version>-paradedb` image.
- **Cohere and Voyage rerankers**: `rerank_backend` selects the existing LLM reranker,
  `cohere` or `voyage`. Their credentials live in a `model_connections` row like every
  other provider secret, so they are encrypted at rest and covered by `ragmux rotate-key`.
  Any failure still falls back to the fused order instead of failing the request.
- **Leased, cross-replica ingestion** (migration `0013`): a document is claimed with
  `FOR UPDATE SKIP LOCKED` and held under a heartbeat lease, so several replicas share one
  queue and a replica that dies hands its work back when the lease expires. Replaces the
  in-process queue that could not be shared.
- **A leader lock for the retention pass**, so one replica does the hourly deletions
  instead of all of them.
- **`docs/scaling.md`** and `docker-compose.scale.yml`: how to run several replicas, what
  is already shared, and what must be set first.
- **A Prometheus `/metrics` endpoint and OpenTelemetry traces**, both written against the
  standard library so the dependency list is unchanged. `/metrics` is **off by default**
  and, when enabled, requires a bearer token or a loopback-only listener — a configuration
  that would publish it unauthenticated on a public bind refuses to start. The metric set
  names every project and model and reports per-project token counts and spend, which is
  the same data the admin metrics endpoints already require a session for. Labels are
  bounded by construction: the HTTP route is the chi pattern rather than the path, and the
  model is the connection's, never the client's. Tracing is off unless an OTLP endpoint is
  set, samples at 5% by default, ignores an inbound `traceparent` unless told to trust it,
  and covers seven spans; point it at an OpenTelemetry Collector, which is the supported
  configuration. See [Observability](docs/observability.md).
- **`GET /readyz`**: ready when the pool answers and the schema has reached the version
  this binary embeds. `/healthz` stays liveness-only, so container healthchecks are
  unchanged. The schema comparison is one-directional on purpose: a schema *behind* the
  binary answers `503` (`migrating`), a schema *ahead* of it answers `200` with a
  `degraded` entry. Failing readiness on a newer schema would empty the fleet in the
  middle of a `maxSurge` rolling upgrade — the first new pod migrates the shared database
  and every old replica would take itself out of rotation at once — and would leave a
  rollback permanently unready. The migrations are additive, so the older code keeps
  serving. A store whose search backend is unavailable is likewise reported as degraded
  with a `200`, not a `503` — the fallback works, and taking the replica out of rotation
  would turn a degraded-but-serving instance into an outage.
- `GET /admin/api/search-backends` reports which backends this server can actually run.
  `/admin/api/provider-types` now carries a full `capabilities` object taken from the
  provider package, so it cannot drift from what the adapters do.

### Changed
- **A wrong `SECRET_KEY` is now a boot failure rather than a per-request error.** The
  database holds a canary only the right key can open, checked before any stored
  credential is read. This catches a dump restored onto a new host, a key rotated on one
  node only, a wiped data volume, and a second replica falling back to its own key file —
  each of which previously surfaced as decrypt errors nobody could trace back.
  `ragmux rotate-key` re-seals the canary in the same transaction.
- **Anthropic `prompt_tokens` now includes cached and freshly written tokens.** 0.3.x
  omitted them, under-reporting a cached request. Token counts, budgets and metrics move
  up for those requests, and historical rows cannot be corrected.
- **A `503` on document upload now means the cluster's backlog is full**, not that this
  replica's in-process queue is — genuine cross-replica backpressure, bounded by
  `MAX_PENDING_DOCUMENTS`.
- The CSV export gains six columns at the end — `cached_prompt_tokens`,
  `cache_write_tokens`, `cost_usd`, `cost_source`, `api_key_id`, `user_id`. Existing
  columns keep their positions. The two attribution columns are empty, not zero, for a
  request made with a project's default key, which has no owner.
- `/admin/api/provider-types` reports `supports_tools: true` for `gemini`, and
  `supports_embeddings: false` for `deepseek`. The second is not a new restriction — the
  dashboard has always shown it — but `SupportsEmbeddings` disagreed, so a RAG store could
  be pointed at a DeepSeek connection through the API. Editing such a store now fails
  validation; change its embedding connection.
- A Gemini `image_url` that is not a Files API or `gs://` URI is inlined by the gateway
  instead of being forwarded as `fileData`, which Gemini rejected.
- Content parts without a `type` are treated as text on every provider, matching OpenAI.
- `Ingester.Resume` is gone. Unfinished work is found by the lease-based claim on every
  poll, on every replica, so there is nothing left to resume at start.

### Fixed
- Ollama tool calls spread over several streaming lines were all given index `0` and
  merged by clients into one corrupt call; they are now numbered across the whole stream.
- Browser clients could not read `x-ratelimit-*`, `x-ragmux-*` or `Retry-After`: the
  responses never carried `Access-Control-Expose-Headers`.

### Security
- A management key is accepted **only** from the `Authorization` header. One pasted into
  the session cookie is refused before it is even looked up, which is what keeps the
  existing CSRF exemption for bearer requests sound: a browser cannot attach an
  `Authorization` header cross-origin without a preflight, while it will attach a cookie
  to any cross-site request on its own.
- Changing a password, resetting another user's password and minting a management key
  require an interactive session, so a leaked key cannot take over the account it belongs
  to.
- An expired, revoked or deactivated-owner key answers `401` with a `code` explaining
  which; an unknown key keeps the previous opaque message, so nothing can be enumerated by
  a caller who does not already hold a valid key.

## [0.3.1] — 2026-09-18

### Added
- **All-in-one container image** (`Dockerfile.aio`, published as `ghcr.io/ragmux/ragmux:<version>`
  and `:latest`): PostgreSQL 17 with pgvector and the gateway in one container, supervised by
  `docker/aio/entrypoint.sh`. One volume at `/data` holds the database (`/data/pg`) and the
  `secret.key` fallback (`/data/ragmux`); Postgres listens on a unix socket only (no TCP,
  trust authentication inside the container) and the gateway connects as the least-privilege
  role `ragmux_app`. `SIGTERM` stops the gateway first, then Postgres (`pg_ctl stop -m fast`);
  if either process dies the container exits non-zero. `DATABASE_URL` (or
  `EMBEDDED_POSTGRES=false`) skips the embedded server; `PG_SHARED_BUFFERS` tunes it;
  `ragmux-aio postgres-only` runs Postgres alone for maintenance. Documented in
  [Deployment layouts](docs/configuration.md#deployment-layouts).
- `scripts/backup.sh` and `scripts/restore.sh` detect the Compose layout (`LAYOUT=auto|split|aio`):
  in the all-in-one layout they run `pg_dump`/`pg_restore` inside the `ragmux` container as
  `postgres` over the socket, and the restore goes through a one-off `postgres-only` container
  while the service is stopped.
- The `backup` Compose profile works with the all-in-one file over a shared socket volume.
- `make docker-build-app` builds the gateway-only image; `make docker-build` now builds the
  all-in-one image.

### Changed
- **Breaking for Compose users: `docker-compose.yml` is now the single-container layout**
  (`ragmux` service built from `Dockerfile.aio`, volume `ragmux-data`, nothing required in
  `.env`; `SECRET_KEY` recommended). The previous two-service file (gateway image + separate
  `pgvector/pgvector:pg17`, requiring `SECRET_KEY`, `POSTGRES_PASSWORD` and
  `RAGMUX_DB_PASSWORD`) is unchanged in behaviour but renamed to **`docker-compose.split.yml`**
  (its local image tag becomes `ragmux/ragmux:latest-app`, matching the published `-app` tag):
  existing deployments add `-f docker-compose.split.yml` (or `COMPOSE_FILE` in `.env`) to keep
  their `pgdata` volume, or move to the default layout with a dump/restore cycle
  ([Moving between layouts](docs/backup-restore.md#moving-between-layouts)). No automatic
  migration of the data volume.
- **Image tags.** `ghcr.io/ragmux/ragmux:<version>`, `:<major>.<minor>` and `:latest` now point
  at the all-in-one image; the gateway-only distroless image is published as
  `:<version>-app`, `:<major>.<minor>-app` and `:latest-app`. Both are built for
  `linux/amd64` and `linux/arm64` and signed with cosign by digest; CI builds both Dockerfiles.
- `docker/postgres-init/01-ragmux.sql` is shared by both layouts: the `ragmux_app` password
  is only set when the `pw` psql variable is passed (the split init script still passes it).
- `.env.example` lists only optional variables for the default file; the split-layout
  passwords sit in a separate block at the end.
- **Default port is now `8765`** (was `8080`): the binary, the Docker image, the Compose file,
  the health check and the documentation all use it. Set `PORT=8080` to keep the old value;
  existing Compose deployments should update their published port mapping.

### Security
- **Signed container images.** The release workflow signs the pushed image digests of
  `ghcr.io/ragmux/ragmux` (all-in-one and `-app`, and the Docker Hub mirror when configured) with cosign, keyless
  through GitHub OIDC. Verify with `cosign verify` as documented in
  [SECURITY.md](SECURITY.md#verifying-the-container-image); images from v0.3.1 onward carry
  a signature.
- Every GitHub Action used by CI and the release workflow is pinned to a full commit SHA
  (with the release tag as a comment for Dependabot) instead of a floating major tag.
- Workflow permissions start at `contents: read`; the release job alone gains
  `contents: write`, `packages: write` and `id-token: write`. CI cancels superseded runs
  of the same ref.
- **PDF parsing is isolated and bounded.** Each PDF is parsed by a disposable child
  process (`ragmux pdf-extract`, the gateway's own binary) that is killed at the 60 s
  deadline, so a crafted file that makes the parser recurse without end (a
  self-referencing object stream, previously a fatal stack overflow that took the whole
  gateway down) or loop forever (an `/Extends` or xref `/Prev` cycle, previously a
  goroutine spinning past the deadline) now only fails that document. The page tree is
  walked by the gateway with a depth and node budget instead of the library's unguarded
  walk, so `/Kids` and `/Parent` cycles end at once and `page_count` reports the pages
  found rather than the declared `/Count`; a page yielding more than 2 MiB of text is
  rejected. A fuzz target (`FuzzExtractPDF`) and regression cases for truncated,
  cyclic, zero-page and oversized inputs cover the parser.

## [0.3.0] — 2026-09-18

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
- `GET /admin/api/audit` takes `since` and `until` (RFC 3339) next to `action`,
  `actor_user_id`, `before` and `limit`, and answers `{"entries": […], "total": N,
  "has_more": bool}` where `total` counts every entry matching the filters. The
  dashboard's Audit tab reads the new shape.
- `GET /admin/api/audit/export` (admin) streams the same entries as NDJSON, newest
  first, capped at 100 000 rows, as an attachment; every export is audited as
  `audit.exported` with the filters in `details`.
- `GET /admin/api/security/logins` (admin): failed logins in the last hour and 24 hours
  plus the username/address pairs the login limiter currently locks out, computed with
  the limiter's own rule.
- Failed logins (`401`) carry `attempts_remaining` (per-user budget minus recorded
  failures, identical for unknown usernames and wrong passwords); `429` answers gain
  `"locked": true|false` next to `Retry-After`.
- `GET /admin/api/me` reports `session_expires_at` and `session_bearer`.
- `GET /admin/api/users` entries carry `project_count` and `active_sessions`;
  `POST /admin/api/users` accepts `project_ids` and writes the memberships in the same
  transaction (`422` for an unknown project).
- `GET /admin/api/setup` reports `migrations_version`, `secret_key_source` and
  `database_role` so the setup page can confirm which database the gateway runs on.

### Changed
- **Dashboard.** `/admin/` is rebuilt as a sidebar console (dark ground, amber accent,
  self-hosted Bricolage Grotesque / IBM Plex Sans / JetBrains Mono under `web/fonts/`,
  SIL OFL) with forms in side drawers, and wires every endpoint above: window tabs with
  previous-period deltas, traffic by project, budget watch and forecasts, CSV / NDJSON
  exports, connection tests before saving, capability and private-upstream pills, quota
  meters and ingestion progress, per-stage retrieval latencies, usage rings with reset
  countdowns, playground citations and response headers, user filters and password
  strength, audit filters with totals, login attempt and lockout feedback, and the
  setup page's database and key status. Role-based visibility is unchanged; the page
  still consists of one inline script pinned by the CSP hash and loads nothing from
  third parties.
- **Usernames are case-insensitive.** Login matches `lower(username)`, and creating a
  user or the first administrator with a name that differs from an existing one only by
  case answers `409`; the stored casing is preserved. Migration `0008` adds a unique
  index on `lower(username)` and refuses to run (with the colliding names) over data
  that already collides; see
  [Case-insensitive usernames](docs/users-and-limits.md#case-insensitive-usernames)
  for the manual fix.
- `GET /admin/api/system` returns `database.postgres_version` and
  `database.pgvector_version` to admins only; editors and viewers receive the
  `database` object without those two keys.

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

[Unreleased]: https://github.com/ragmux/ragmux/compare/v0.3.1...HEAD
[0.3.1]: https://github.com/ragmux/ragmux/releases/tag/v0.3.1
[0.3.0]: https://github.com/ragmux/ragmux/releases/tag/v0.3.0
[0.2.3]: https://github.com/ragmux/ragmux/releases/tag/v0.2.3
[0.2.2]: https://github.com/ragmux/ragmux/releases/tag/v0.2.2
[0.2.1]: https://github.com/ragmux/ragmux/releases/tag/v0.2.1
[0.2.0]: https://github.com/ragmux/ragmux/releases/tag/v0.2.0
[0.1.0]: https://github.com/ragmux/ragmux/releases/tag/v0.1.0
