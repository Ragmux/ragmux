# REST API reference

Ragmux exposes three HTTP surfaces:

| Prefix | Audience | Authentication |
|---|---|---|
| `/v1/…` | your applications (OpenAI-compatible) | `Authorization: Bearer sk-proj-…` project key |
| `/admin/api/…` | dashboard and management scripts | session cookie or bearer session token |
| `/healthz` | load balancers, container healthchecks | none |

All request and response bodies are JSON unless noted. `/admin/` (without `api`) serves
the embedded dashboard.

## Authentication

### Management API

`POST /admin/api/login` with `{"username": "...", "password": "...", "bearer": true}`
returns

```json
{"token": "…", "user": {"id": 1, "username": "admin", "role": "admin", "is_active": true,
                          "last_login_at": "2026-09-18T10:00:00Z", "created_at": "…"}}
```

and sets the `ragmux_session` cookie (`HttpOnly`, `SameSite=Lax`, `Secure` on HTTPS or
with `SECURE_COOKIES=true`). The `token` field is only included when the request sets
`"bearer": true`; the dashboard omits it and relies on the cookie alone. Every other
`/admin/api` route accepts either that cookie or `Authorization: Bearer <token>`; the
bearer header wins when both are present. Sessions expire after `SESSION_TTL` (default
24 h) and are revoked by `POST /admin/api/logout`, a password change (all other
sessions of the account), a password reset, a session revoke or deactivation of the
account.

```bash
TOKEN=$(curl -s localhost:8765/admin/api/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"YOUR_PASSWORD","bearer":true}' | jq -r .token)
AUTH="Authorization: Bearer $TOKEN"
```

Requests authenticated by the **cookie** are subject to two browser-oriented checks
that bearer requests skip: state-changing methods (`POST`, `PUT`, `DELETE`) must come
from the same origin (`Sec-Fetch-Site: same-origin`/`none`, or an `Origin` whose host
equals the request `Host`; otherwise `403 {"error":{"message":"cross-site request
rejected","type":"forbidden"}}`), and JSON bodies must be sent as
`Content-Type: application/json` (otherwise `415`). The login request itself always
needs the JSON content type. Scripts should use the bearer token.

Every `/admin/api` response carries `Cache-Control: no-store` and an `X-Request-Id`;
unexpected failures answer `500 {"error":{"message":"internal error (request id …)"}}`
and log the detail under that id.

Login failures answer
`401 {"error":{"message":"invalid username or password","type":"Unauthorized","attempts_remaining":4}}`,
where `attempts_remaining` is how many more failures the username may record this
minute before the per-user limit triggers (absent when that limit is disabled). It is
derived from the recorded attempts alone, so an unknown username and a wrong password
get identical answers. Too many failures answer
`429 {"error":{"message":"too many login attempts, try again later","type":"rate_limited","locked":false}}`
with a `Retry-After` header; `locked` is `true` when the username/address lockout
triggered rather than a per-minute budget (see
[Users, roles and limits](users-and-limits.md#login-protection)). Usernames are matched
case-insensitively.

### Client API

`/v1` routes require `Authorization: Bearer sk-proj-…`, the key returned once when a
project is created or its key is rotated. The key selects the project, and with it the
model connection, system prompt, RAG store and limits. Missing or unknown keys answer
`401` with type `invalid_request_error` (no bearer header) or `invalid_api_key`.

## Roles

The **Role** column in the tables below is the minimum role required (`viewer` <
`editor` < `admin`). `member` means the caller must be an `admin` or a member of the
project; non-members get `404` so project ids are not enumerable. Requests below the
required role answer `403 {"error":{"message":"insufficient role","type":"forbidden"}}`;
requests without a valid session answer
`401 {"error":{"message":"authentication required","type":"unauthorized"}}`.

## Error envelope

Management endpoints answer errors as

```json
{"error": {"message": "chunk_size must be between 200 and 20000", "type": "Bad Request"}}
```

where `type` is the HTTP status text: `Bad Request` (validation), `Not Found`,
`Conflict` (duplicate name, or an item still referenced by a project or RAG store),
`Forbidden`, `Internal Server Error`. Login limiting and the auth middleware use the
lower-case types shown above.

The client API uses the OpenAI shape with a `code` field:

```json
{"error": {"message": "invalid project api key", "type": "invalid_api_key", "code": null}}
```

## Management endpoints

All paths are relative to `/admin/api`.

### First-run setup

Unauthenticated by design; both refuse as soon as any user exists.

| Method | Path | Role | Purpose |
|---|---|---|---|
| GET | `/setup` | — | `{"needs_setup": true, "migrations_version": 8, "secret_key_source": "env", "database_role": "ragmux"}`: `needs_setup` is `true` while the `users` table is empty; the other three let the setup page confirm which database and key the gateway runs on (`secret_key_source` is `env` or `file`, `database_role` the connected PostgreSQL role) |
| POST | `/setup` | — | `{username, password, bearer?}` creates the first user with the `admin` role and logs it in (session cookie; `token` in the body when `bearer` is true) → `201 {user}`. Username: 3–64 characters of `a-z 0-9 . _ -`; password: 12–72 bytes. `409 setup already completed` once a user exists, also for a concurrent request that lost the race. Failed attempts count against the per-address login limit (`429` with `Retry-After`). Audited as `setup.complete`. |

`ADMIN_PASSWORD` pre-creates the account on start for unattended installs, in which
case setup is already complete (see [Configuration](configuration.md#environment-variables)).

### Session and account

| Method | Path | Role | Purpose |
|---|---|---|---|
| POST | `/login` | — | `{username, password, bearer?}` → `{user}` plus `token` when `bearer` is true; `400` when the username (1–64 characters) or password (1–1024) is missing or too long; the username is matched case-insensitively |
| POST | `/logout` | viewer | Ends the session → `{"ok": true}` |
| GET | `/me` | viewer | Current user `{id, username, role, is_active, last_login_at, created_at}` plus `session_expires_at` (RFC 3339) and `session_bearer` (`true` when the request carried an `Authorization` header rather than the cookie) |
| POST | `/me/password` | viewer | `{current_password, new_password}` (8+ characters, at most 72 bytes) → `{"ok": true}`; `403` when the current password is wrong; every other session of the account is revoked |
| GET | `/provider-types` | viewer | Supported provider types with `type`, `label`, `default_base_url`, `supports_embeddings`, `requires_api_key`, `supports_tools` (false for `gemini`), `supports_streaming` |

### Model connections

| Method | Path | Role | Purpose |
|---|---|---|---|
| GET | `/models` | viewer | List connections |
| POST | `/models` | editor | Create → `201` connection |
| GET | `/models/{id}` | viewer | Read one |
| PUT | `/models/{id}` | editor | Update; an empty `api_key` keeps the stored key |
| DELETE | `/models/{id}` | editor | Delete → `{"ok": true}`; `409` while a project or RAG store uses it |
| POST | `/models/{id}/test` | editor | `{"mode": "chat"}` (default) or `{"mode": "embedding"}`; spends provider quota, so it is a write; the outcome is recorded on the connection |
| POST | `/models/test` | editor | Test an unsaved connection: the create body plus `mode` and optionally `connection_id` (reuse that connection's stored key when `api_key` is empty); nothing is recorded |

Request body for create and update:

```json
{"name": "claude", "provider_type": "anthropic", "api_key": "sk-ant-...",
 "model_name": "claude-sonnet-4-5", "base_url": ""}
```

`provider_type` is one of `openai`, `anthropic`, `gemini`, `deepseek`, `ollama`,
`custom_openai`; `base_url` must be an `http://` or `https://` URL with a host and
without credentials, query string or fragment, and is required for `custom_openai`; a
host that resolves only to private or local addresses is rejected with `400` unless
allowed by `PRIVATE_UPSTREAM_ALLOWLIST` / `ALLOW_PRIVATE_UPSTREAMS` (see
[Configuration](configuration.md#private-upstreams)). `model_name` may contain letters,
digits and `. _ : / @ -` (up to 128 characters, no leading `/`, no `..`). The response
never contains the key:

```json
{"id": 1, "name": "claude", "provider_type": "anthropic", "base_url": "",
 "api_key_masked": "sk-ant-…abcd", "model_name": "claude-sonnet-4-5", "private_upstream": false,
 "last_test_at": "2026-09-18T10:05:00Z", "last_test_ok": true, "last_test_latency_ms": 812, "last_test_error": "",
 "created_at": "2026-09-18T10:00:00.000Z", "updated_at": "2026-09-18T10:00:00.000Z"}
```

`private_upstream` is true when `base_url` points at `localhost`, a loopback, private or
link-local address, or a hostname that resolved only to such addresses when the
connection was saved (it is computed on create and update, never on read; the provider
default URL counts as public). `last_test_*` describe the most recent
`POST /models/{id}/test`: all `null` / empty until the connection has been tested.

`POST /models/{id}/test` always answers `200` and reports the outcome in the body:
`{"ok": true, "reply": "pong", "usage": {…}, "latency_ms": 812}` for chat (the model is asked to reply
with the word `pong`, `max_tokens` 256), `{"ok": true, "dimensions": 1536, "latency_ms": 240}`
for embeddings, or `{"ok": false, "error": "...", "latency_ms": …}`. The result is stored on
the connection as `last_test_at`, `last_test_ok`, `last_test_latency_ms` and
`last_test_error` (credentials redacted, at most 512 characters).

`POST /models/test` runs the same test on a connection that has not been saved:

```json
{"provider_type": "custom_openai", "base_url": "http://vllm:8000/v1", "api_key": "",
 "model_name": "llama-3", "mode": "chat", "connection_id": 4}
```

`base_url`, `provider_type` and `model_name` are validated exactly like create (including
the private-address check → `400`); `name` is not required. With `connection_id` set and
`api_key` empty the stored key of that connection is used, so an edit form can try a
changed URL or model without re-entering the key. The response has the same shape as
`/models/{id}/test`; nothing is recorded.

### RAG stores and documents

| Method | Path | Role | Purpose |
|---|---|---|---|
| GET | `/rag-stores` | viewer | List stores (with `document_count`, `bytes_used`, `chunk_count`, `dimensions`) |
| POST | `/rag-stores` | editor | Create → `201` store |
| GET | `/rag-stores/{id}` | viewer | Read one |
| PUT | `/rag-stores/{id}` | editor | Update → store plus `reprocess_recommended` |
| DELETE | `/rag-stores/{id}` | editor | Delete the store, its documents and vectors; projects linked to it lose the link |
| GET | `/rag-stores/{id}/documents` | viewer | List documents, newest first |
| POST | `/rag-stores/{id}/documents` | editor | Upload (`multipart/form-data`, one or more `file` fields) → `202` document or array of documents; `422` with `code: "store_quota"` when the store's `max_documents` / `max_bytes` or the instance ceiling would be exceeded |
| POST | `/rag-stores/{id}/search` | viewer | Retrieval test |
| POST | `/rag-stores/{id}/reprocess` | editor | Re-parse, re-chunk and re-embed every document → `202 {"ok": true, "documents": n}` |
| GET | `/documents/{id}` | viewer | Document status |
| DELETE | `/documents/{id}` | editor | Delete document and its chunks → `{"ok": true}` |
| POST | `/documents/{id}/reprocess` | editor | Re-chunk and re-embed one document → `202 {"ok": true}` |

Store body (all fields except `name` and `embedding_connection_id` optional; the
retrieval semantics are explained in [Retrieval (RAG)](rag.md)):

```json
{"name": "handbook", "embedding_connection_id": 2,
 "chunk_size": 1000, "chunk_overlap": 200, "top_k": 5,
 "search_mode": "hybrid", "fts_config": "simple",
 "rerank": false, "rerank_candidates": 15, "max_distance": 0, "contextual_chunks": true,
 "max_documents": 0, "max_bytes": 0}
```

Validation: `chunk_size` 200-20000, `chunk_overlap` ≥ 0 and smaller than `chunk_size`,
`top_k` ≤ 50, `search_mode` `vector` or `hybrid`, `fts_config` must exist in
`pg_ts_config`, `rerank_candidates` 1-100, `max_distance` 0-2, `max_documents` and
`max_bytes` ≥ 0 (`0` = unlimited, see [Quotas](rag.md#quotas)). The embedding
connection must be of a type that supports embeddings and cannot be changed while the
store has chunks (`400`). Responses carry the usage the quotas are checked against:
`document_count` and `bytes_used`.

Documents look like

```json
{"id": 7, "rag_store_id": 1, "filename": "handbook.pdf", "mime": "application/pdf",
 "size_bytes": 1048576, "status": "ready", "error": "", "chunk_count": 42,
 "page_count": 12, "progress_percent": 100, "created_at": "…", "updated_at": "…"}
```

with `status` moving `pending` → `processing` → `ready` (or `failed` with `error` set).
`progress_percent` follows ingestion: `10` once parsed, `20` once chunked, then the share
of embedding batches completed, `100` when ready; a failed document keeps the value it
reached. `page_count` is the number of pages of a PDF and `null` for other formats (and
until the document has been parsed).
Accepted extensions: `.pdf`, `.docx`, `.html`, `.htm`, `.md`, `.markdown`, `.txt`; the
request body is limited to `MAX_UPLOAD_MB`. Upload and reprocess answer
`503 the ingestion backlog is full (MAX_PENDING_DOCUMENTS), retry later` when the queue
of `pending` documents is at `MAX_PENDING_DOCUMENTS` (1024); the documents concerned are
marked `failed` with that message. The backlog is counted across every replica, not per
process.

Search request and response:

```json
POST /admin/api/rag-stores/1/search
{"query": "vacation policy", "top_k": 3, "mode": "hybrid", "rerank": true, "max_distance": 0.6}

{"mode": "hybrid", "reranked": true, "latency_ms": 412,
 "retrieval_latency_ms": 180, "rerank_latency_ms": 230,
 "hits": [{"chunk_id": 8, "document_id": 2, "filename": "handbook.pdf", "index": 3,
           "section": "Leave > Vacation", "page": 12, "content": "…",
           "distance": 0.18, "score": 0.0325, "vector_rank": 1, "fts_rank": 2}]}
```

Everything but `query` is optional and overrides the store setting for this call only;
`top_k` is clamped to 1-50 (`0` or absent uses the store's `top_k`). `latency_ms` is the
whole call, `retrieval_latency_ms` covers embedding the query and the database search,
and `rerank_latency_ms` the reranker (`null` when reranking is off or no chat connection
is available for it).
A `502` is returned when the embedding call fails.

### Projects

| Method | Path | Role | Purpose |
|---|---|---|---|
| GET | `/projects` | viewer | Projects the caller is a member of (admins: all) |
| POST | `/projects` | editor | Create → `201 {"project": …, "api_key": "sk-proj-…"}` (key shown once; the creator becomes a member) |
| GET | `/projects/{id}` | member | Read one |
| PUT | `/projects/{id}` | member + editor | Update (full replace of the fields below) |
| DELETE | `/projects/{id}` | member + editor | Delete with its request logs → `{"ok": true}` |
| POST | `/projects/{id}/rotate-key` | member + editor | New key → `{"project": …, "api_key": "sk-proj-…"}`; the old key stops working immediately |
| GET | `/projects/{id}/members` | member | `[{id, username, role, added_at}]` |
| PUT | `/projects/{id}/members` | member + editor | `{"user_ids": [1, 4]}` replaces the member set; an editor cannot remove themselves |
| GET | `/projects/{id}/metrics?window=24h` | member | Metrics for one project (see below; takes the same `compare`, `days` and `by_project` options as the summary) |
| GET | `/projects/{id}/metrics.csv?window=24h` | member | The project's request logs in the window as a CSV download (see below) |
| GET | `/projects/{id}/usage` | member | Live rate-limit and budget counters with a budget forecast |

Project body:

```json
{"name": "support-bot", "model_connection_id": 1, "rag_store_id": 1,
 "system_prompt": "You are the company support assistant.",
 "member_user_ids": [4],
 "rate_limit_rpm": 0, "rate_limit_tpm": 0, "budget_daily_tokens": 0, "budget_monthly_tokens": 0}
```

`rag_store_id` may be `null` or `0` for no store; limits are `0` for unlimited.
`member_user_ids` is only read on create (use the members endpoint afterwards). The
project object contains `api_key_prefix` (the first characters of the key for
identification), `member_ids` and the limit fields; the full key is never returned
again.

`GET /projects/{id}/usage`:

```json
{"project_id": 3, "generated_at": "2026-09-18T12:00:30Z",
 "minute": {"period_start": "2026-09-18T12:00:00Z", "resets_at": "2026-09-18T12:01:00Z",
            "requests": 2, "prompt_tokens": 20, "completion_tokens": 4, "tokens": 24,
            "request_limit": 2, "request_percent": 100, "token_limit": 0, "token_percent": 0},
 "day":   {"…": "…", "tokens": 12, "token_limit": 5, "token_percent": 100, "resets_at": "2026-09-19T00:00:00Z"},
 "month": {"…": "…"},
 "forecast": {"daily_exhausted_at": "2026-09-18T15:40:00Z", "monthly_exhausted_at": null}}
```

`forecast` extends the tokens consumed in the last 60 minutes linearly and reports when
each budget would run out at that pace. A value is `null` when the budget is not set,
nothing was consumed in the last hour, or the projection lands after the window resets
(`resets_at`); an already exhausted budget projects to `generated_at`.

### Metrics

| Method | Path | Role | Purpose |
|---|---|---|---|
| GET | `/metrics/summary?window=24h&compare=1&days=14&by_project=1` | viewer | Summary over the projects the caller can see |
| GET | `/metrics/requests?limit=50&project_id=3` | viewer | Recent request logs, newest first (`limit` defaults to 100, max 1000; `project_id` must be visible to the caller) |
| GET | `/metrics/requests.csv?window=24h&project_id=3` | viewer | Request logs in the window as a CSV download (`project_id` optional, same visibility rule) |

`window` is a Go duration (default `24h`). Optional summary parameters:

| Parameter | Effect |
|---|---|
| `compare=1` | adds `previous`: the same summary fields for the window of equal length that ends where the current one starts |
| `days=N` | sizes the `daily` series (1–90, default 14; out-of-range values are clamped) |
| `by_project=1` | adds `projects`: one row per project with requests in the window, busiest first, limited to the projects the caller can see |

The summary response (also used by `/projects/{id}/metrics`):

```json
{"window": {"requests": 120, "errors": 3, "prompt_tokens": 51000, "completion_tokens": 9800,
            "avg_latency_ms": 840.5, "p95_latency_ms": 2100, "rag_requests": 95, "rate_limited": 2},
 "total":  {"…": "same fields over all time"},
 "daily":  [{"day": "2026-09-05", "requests": 10, "errors": 0, "prompt_tokens": 4000, "completion_tokens": 900}],
 "recent": [{"id": 991, "project_id": 3, "model_name": "claude-sonnet-4-5", "status_code": 200,
             "prompt_tokens": 420, "completion_tokens": 80, "estimated": false, "latency_ms": 910,
             "streamed": true, "rag_used": true, "rag_hits": 3, "error": "", "created_at": "…"}],
 "previous": {"…": "only with compare=1"},
 "projects": [{"project_id": 3, "name": "support-bot", "requests": 80, "errors": 2, "prompt_tokens": 30000,
               "completion_tokens": 6000, "rate_limited": 1, "rag_requests": 70}]}
```

`daily` covers the last `days` days (14 by default), `recent` the last 50 requests.
`rag_hits` is the number of retrieved passages injected into that request (`0` when
`rag_used` is false). Status semantics (`429`, `499`, `502`/`504`) are described in
[Users, roles and limits](users-and-limits.md#metrics-and-retention).

#### CSV export

`GET /metrics/requests.csv` and `GET /projects/{id}/metrics.csv` return the window's
request logs, oldest first and at most 50 000 rows, as `text/csv` with a
`Content-Disposition: attachment; filename="ragmux-requests-<date>.csv"` (or
`ragmux-project-<id>-requests-<date>.csv`) header. Columns:

```
created_at, project_id, project_name, model_name, status_code, prompt_tokens, completion_tokens,
estimated, latency_ms, streamed, rag_used, rag_hits, error
```

Cells that begin with `=`, `+`, `-` or `@` (also after a leading tab or carriage return)
are prefixed with a single quote so a spreadsheet does not evaluate them as formulas;
model names and error messages can be shaped by an upstream.

### Users and audit log

| Method | Path | Role | Purpose |
|---|---|---|---|
| GET | `/users/lite` | editor | Active users as `[{id, username, role}]` (for member pickers) |
| GET | `/users` | admin | All users, each with `project_count` (memberships) and `active_sessions` (unexpired sessions) |
| POST | `/users` | admin | `{username, password, role, project_ids?}` → `201` user (`role` defaults to `viewer`, password 8+ characters and ≤ 72 bytes, username ≤ 64); `project_ids` adds the memberships in the same transaction, `422` when one of them is unknown; `409` when the username already exists in any casing |
| GET | `/users/{id}` | admin | Read one |
| PUT | `/users/{id}` | admin | `{role, is_active}` (both optional); deactivating drops the user's sessions |
| DELETE | `/users/{id}` | admin | Delete → `{"ok": true}` |
| POST | `/users/{id}/reset-password` | admin | `{new_password}`; revokes the user's sessions |
| POST | `/users/{id}/sessions/revoke` | admin | Sign the user out everywhere |
| GET | `/audit` | admin | Audit entries, newest first, with the total for the filter |
| GET | `/audit/export` | admin | The same filters as NDJSON (one entry per line), at most 100 000 rows, newest first, as an attachment; audited as `audit.exported` |
| GET | `/security/logins` | admin | Failed-login counters and active lockouts |

`PUT`/`DELETE` on users refuse to demote, deactivate or delete the last active admin
(`409`) and to deactivate or delete the caller's own account (`400`).

`GET /audit?limit=100&action=project.&actor_user_id=1&since=2026-09-01T00:00:00Z&until=2026-09-18T00:00:00Z&before=2026-09-17T10:00:00Z`:
`action` is a prefix match, `since` (inclusive) and `until` (exclusive) bound
`created_at`, `before` pages backwards (all three RFC 3339; a malformed value is a
`400`), `limit` defaults to 100 (max 1000). The response is

```json
{"entries": [{"id": 512, "actor_user_id": 1, "actor_username": "admin", "action": "project.rotate_key",
              "target_type": "project", "target_id": 3, "details": {"name": "support-bot"},
              "ip": "203.0.113.7", "created_at": "…"}],
 "total": 1834, "has_more": true}
```

where `total` counts every entry matching `action`, `actor_user_id`, `since` and
`until` (ignoring `before` and `limit`) and `has_more` tells whether another page
exists beyond this one.

`GET /audit/export?action=login.&since=…` streams the same entries as
`application/x-ndjson` (`Content-Disposition: attachment; filename="ragmux-audit-<timestamp>.ndjson"`),
one JSON object per line in the shape above, newest first, capped at 100 000 rows;
`before` is honoured so a larger range can be exported in slices. Every export writes
an `audit.exported` entry whose `details` carry the filters that were used.

`GET /security/logins`:

```json
{"failed_last_hour": 12, "failed_last_24h": 40,
 "active_lockouts": [{"username": "admin", "ip": "203.0.113.7", "until": "2026-09-18T10:14:00Z"}]}
```

The counters are failed attempts in the `login_attempts` table; `active_lockouts` lists
the username/address pairs the login limiter currently refuses, computed with the same
`LOGIN_LOCKOUT_FAILURES` / `LOGIN_LOCKOUT_MINUTES` rule, with `until` the time the
lockout ends (empty when the lockout is disabled).

### System

`GET /system` (viewer):

```json
{"database": {"postgres_version": "17.11", "pgvector_version": "0.8.6", "migrations_version": 8, "size_bytes": 8787635},
 "backup": {"tables": 1, "documents_bytes": 1048576, "last_migration_at": "2026-09-18T12:34:41Z"},
 "secret_key_source": "env",
 "version": "0.3.0"}
```

`backup.tables` is the number of `chunk_embeddings_<dims>` tables, `documents_bytes` the
bytes of uploaded files stored in the database, `secret_key_source` is `env` or `file`.
`database.postgres_version` and `database.pgvector_version` are only returned to admins;
editors and viewers receive the `database` object without those two keys.

## Health

`GET /healthz` pings the database and answers `200 {"status":"ok","version":"0.2.0"}`
or `503 db unavailable` (plain text). No authentication.

## Client API (`/v1`)

Point any OpenAI SDK at `http://<host>:8765/v1` with the project key as `api_key`.

### `POST /v1/chat/completions`

Accepts the OpenAI Chat Completions payload: `model`, `messages` (text or
`text` / `image_url` content parts), `stream`, `temperature`, `top_p`, `max_tokens` /
`max_completion_tokens`, `stop`, `n`, `tools`, `tool_choice`, `response_format`,
`stream_options`, `user`. Unknown fields are forwarded to OpenAI-compatible upstreams
unchanged (see [Providers](providers.md)), with one exception: an optional `ragmux`
object is consumed by the gateway and never sent upstream. `messages` is required; the
body may not exceed 4 MiB.

```json
{"model": "…", "messages": […], "ragmux": {"include_context": true}}
```

`include_context` asks for the retrieved sources and the exact context block that was
injected, added as a top-level `ragmux` object to a non-streaming response (see
[RAG](rag.md#sources-on-the-response)); streaming responses only carry the headers.

What happens to a request, in order:

1. The project's rate limits and budgets are checked (before any embedding call).
2. The project `system_prompt`, if set, is prepended to the first `system`/`developer`
   message (or inserted as one).
3. If the project has a RAG store, the last `user` message is used as the query and the
   retrieved passages are injected the same way; the response carries
   `x-ragmux-rag-hits: <n>` and, when passages were injected, `x-ragmux-rag-sources`
   listing them. Retrieval failures are logged and the request continues without
   context.
4. The request is translated for the provider and sent. The `model` field is **not**
   used for routing; the project's connection decides. The value you send is echoed
   back in the response (`model` of the completion and every stream chunk); an empty
   value is replaced with the connection's model name.

Non-streaming responses are the provider's answer normalised to the OpenAI schema
(`choices[].message`, `finish_reason`, `usage`). Streaming responses are
`text/event-stream` with `data: {chunk}` lines and a final `data: [DONE]`; a failure
after the stream has started is emitted as a `data: {"error": …}` event before `[DONE]`.

Response headers, only for limits the project has set:

| Header | Meaning |
|---|---|
| `x-ratelimit-limit-requests` | configured requests per minute |
| `x-ratelimit-remaining-requests` | requests left in the current UTC minute |
| `x-ratelimit-reset-requests` | whole seconds until the minute window resets |
| `x-ragmux-budget-daily-remaining` | tokens left in today's budget |
| `x-ragmux-budget-monthly-remaining` | tokens left in this month's budget |
| `x-ragmux-rag-hits` | passages injected (present whenever the project has a store, `0` when none matched) |
| `x-ragmux-rag-sources` | JSON array of `{document_id, filename, section, page, score}` for the injected passages; present only when `x-ragmux-rag-hits` is above `0`, trimmed to whole entries to stay under 2 KB |

Status codes:

| Status | When |
|---|---|
| `400` | malformed JSON, missing `messages`, or a translation error such as tools on a Gemini connection |
| `401` | missing or invalid project key |
| `413` | body larger than 4 MiB |
| `429` | project rate limit or budget exceeded; `Retry-After` set, body `{"error":{"message","type":"rate_limit_exceeded"|"insufficient_quota","code":"rate_limit_rpm"|"rate_limit_tpm"|"budget_daily"|"budget_monthly"}}` |
| `4xx`/`5xx` from the provider | relayed with the provider's status and message (API-key-looking strings redacted) |
| `500` | the project's model connection cannot be set up (`model connection unavailable`; the reason is in the gateway log) |
| `502` | transport failure, a provider error without a status, or a crash inside the provider adapter during a stream; mid-stream failures are logged as `502` |
| `499` | never sent on the wire: recorded in the request log when the client disconnected before the completion finished. The upstream call is cancelled in both the JSON and the streaming path |

### `GET /v1/models` and `GET /v1/models/{id}`

Return the project's single model in OpenAI's shape so SDK model listings work:

```json
{"object": "list", "data": [{"id": "claude-sonnet-4-5", "object": "model", "created": 1758190800, "owned_by": "anthropic"}]}
```

`{id}` is ignored; both routes describe the project's connection.
