# Users, roles and limits

This page covers who can do what in the management API and dashboard, how logins are
protected and audited, and the per-project rate limits, token budgets, metrics and
retention that apply to the client API. Endpoint details are in the
[API reference](api.md).

## Roles

The first-run account (`ADMIN_USER`) is an `admin`. Admins create further users in the
dashboard (**Users** tab) or via `POST /admin/api/users`. Every user has exactly one role:

| Role     | Model connections, RAG stores, documents | Projects | Users, audit log |
|----------|------------------------------------------|----------|------------------|
| `admin`  | full access | all projects, all metrics | full access |
| `editor` | create, edit, delete, upload, test, search | create (becomes a member); read, edit, delete, rotate key, metrics and members only for projects it belongs to | list active users (`/users/lite`) |
| `viewer` | read and search only | read and metrics only for projects it belongs to | — |

Writes the role does not allow answer `403 {"error":{"type":"forbidden"}}`. Everyone can
change their own password (`POST /admin/api/me/password`) and read `/admin/api/system`.

## Project membership

Projects have a member list (`member_ids`). Non-admins only see projects they are a member
of; any other project id answers `404`, and the global metrics endpoints
(`/metrics/summary`, `/metrics/requests`) are limited to their projects. The creator of a
project is always added as a member, and an editor cannot remove themselves through
`PUT /projects/{id}/members`, so editors keep access to what they manage. Admins can
replace the member set freely.

## User management

- `PUT /admin/api/users/{id}` changes `role` and `is_active`. Deactivated users cannot sign
  in and their existing sessions stop working immediately.
- The last active admin cannot be demoted, deactivated or deleted (`409`), and nobody can
  deactivate or delete their own account (`400`).
- `POST /admin/api/users/{id}/reset-password` sets a new password and revokes the user's
  sessions; `POST /admin/api/users/{id}/sessions/revoke` only signs the user out everywhere.
- Passwords must be at least 8 characters and at most 72 bytes (bcrypt's input limit);
  usernames at most 64.
- Changing your own password (`POST /admin/api/me/password`) signs out every other
  session of the account; the session that made the change stays valid.

## Login protection

Login bodies are checked before anything else: the username (trimmed) must be 1–64
characters and the password 1–1024; anything else is a `400` that is neither recorded
nor counted.

Every remaining attempt is recorded in the database, so all replicas share the counters.
After `LOGIN_USER_LIMIT_PER_MIN` (default 5) failures for a username or
`LOGIN_RATE_LIMIT_PER_MIN` (default 10) failures from an IP within a minute, and after
`LOGIN_LOCKOUT_FAILURES` (default 20) failures for one **username from one IP** within
`LOGIN_LOCKOUT_MINUTES` (default 15), `POST /admin/api/login` answers
`429 {"error":{"message":"too many login attempts, try again later","type":"rate_limited"}}`
with a `Retry-After` header. The lockout is keyed on the username/address pair so that
a stranger hammering your username cannot lock you out from your own address; the
per-minute limits still slow them down. Setting a limit to `0` disables it. Successful
logins do not reset the counters; the windows simply expire. Attempts older than 24
hours are purged hourly.

The client IP is the TCP peer address unless `TRUST_PROXY_HEADERS=true`, in which case
the last `X-Forwarded-For` entry (or `X-Real-IP`) is used, optionally restricted with
`TRUSTED_PROXY_CIDRS`; only enable it behind a reverse proxy (see
[Configuration](configuration.md#behind-a-reverse-proxy)).

## Audit log

Logins (`login.success`, `login.failure`, `login.rate_limited`, `login.locked`), logouts,
password changes and every create, update, delete, key rotation, upload, reprocess, member
change and connection test are written to `audit_logs` with the actor, target, IP and a
small JSON `details` object that never contains credentials. Action names are
`<target>.<verb>`: `model.create`, `model.update`, `model.delete`, `model.test`,
`rag_store.create`, `rag_store.update`, `rag_store.delete`, `rag_store.reprocess_all`,
`document.upload`, `document.delete`, `document.reprocess`, `project.create`,
`project.update`, `project.delete`, `project.rotate_key`, `project.members_update`,
`user.create`, `user.update`, `user.delete`, `user.reset_password`,
`user.revoke_sessions`, `password.change`, `logout`.

Admins read the log with
`GET /admin/api/audit?limit=100&action=project.&actor_user_id=1&before=2026-09-18T10:00:00Z`
(`action` is a prefix match, `before` pages backwards) or in the **Audit** dashboard tab.
Entries are kept for `AUDIT_RETENTION_DAYS` (default 365).

## Rate limits and budgets

Every project has four optional limits (`0` = unlimited), set at creation or with
`PUT /admin/api/projects/{id}` and shown in the dashboard's project form:

| Field | Meaning |
|---|---|
| `rate_limit_rpm` | Requests per UTC minute |
| `rate_limit_tpm` | Prompt + completion tokens per UTC minute |
| `budget_daily_tokens` | Prompt + completion tokens per UTC day |
| `budget_monthly_tokens` | Prompt + completion tokens per UTC calendar month |

Counters live in the `project_usage` table, so several gateway replicas share them. All
windows are aligned to UTC boundaries (`date_trunc` of minute, day and month), not sliding.
When a limit is hit `/v1/chat/completions` answers

```json
HTTP 429  Retry-After: 42
{"error":{"message":"Rate limit reached: 2 requests per minute for this project. Retry after 42 seconds.",
          "type":"rate_limit_exceeded","code":"rate_limit_rpm"}}
```

`type` is `rate_limit_exceeded` for the per-minute limits and `insufficient_quota` for
budgets; `code` is one of `rate_limit_rpm`, `rate_limit_tpm`, `budget_daily`,
`budget_monthly`. `Retry-After` is the number of seconds until the violated window resets
(next minute, next UTC day or next month). Rejected requests are recorded in the request
log with status 429 (visible as `rate_limited` in the metrics summaries and the
dashboard) and do not consume tokens.

Response headers on every chat completion (only for limits that are set):

| Header | Value |
|---|---|
| `x-ratelimit-limit-requests` | configured requests per minute |
| `x-ratelimit-remaining-requests` | requests left in the current minute |
| `x-ratelimit-reset-requests` | integer seconds until the minute window resets |
| `x-ragmux-budget-daily-remaining` | tokens left in today's budget |
| `x-ragmux-budget-monthly-remaining` | tokens left in this month's budget |

How the checks work: limits are checked before RAG retrieval, so throttled clients do
not cost an embedding call. The requests-per-minute slot is reserved atomically **before**
the upstream call, so concurrent requests cannot exceed the limit; a rejected reservation
is released again so `remaining` stays accurate. Tokens are only known after the provider
answers, so the token limit and the budgets are checked against what has been recorded so
far. For the per-minute token limit the request's own prompt is estimated at four
characters per token (client messages plus the project system prompt, before RAG context
is added); budgets use recorded usage only. A single request can therefore overshoot a
budget by its own size — the *next* request is refused. Providers that do not report
usage fall back to the same character estimate.

`GET /admin/api/projects/{id}/usage` returns the live counters:

```json
{"project_id":3,"generated_at":"2026-09-18T12:00:30Z",
 "minute":{"period_start":"…","resets_at":"…","requests":2,"tokens":24,"request_limit":2,"request_percent":100,"token_limit":0,"token_percent":0, …},
 "day":{"…":"…","tokens":12,"token_limit":5,"token_percent":100,"resets_at":"2026-09-19T00:00:00Z"},
 "month":{"…":"…"}}
```

Minute rows are purged after two hours, day rows after 400 days and month rows after
three years by the hourly retention job.

## Metrics and retention

Every chat completion is written to `request_logs` with the status the client received
plus two gateway-internal codes:

| Status | Meaning |
|---|---|
| `2xx` | completed; tokens from the provider's usage block, or an estimate (`estimated: true`) |
| `429` | rejected by the project's rate limit or budget (`rate_limited` in summaries) |
| `499` | the client disconnected before the completion finished; the upstream call was cancelled. Counted as an error in summaries, never sent on the wire |
| `502` / `504` | upstream failure or timeout, including streams that died mid-way |
| other `4xx` / `5xx` | relayed from the provider |

Each row also records the model name, prompt and completion tokens, latency, whether the
response was streamed and whether RAG context was injected. Prompts and completions are
not stored. Upstream error messages are stored and logged with anything that looks like
an API key (`sk-…`, `sk-ant-…`, `AIza…`, `Bearer …`) replaced by `[redacted]`.

`GET /admin/api/metrics/summary?window=24h` and `GET /admin/api/projects/{id}/metrics`
return the window summary (requests, errors, tokens, average and p95 latency, RAG
requests, `rate_limited`), the all-time summary, a 14-day daily series and the 50 most
recent requests; the dashboard's overview and project pages are built on them.

A retention job (the janitor in `internal/maintenance`) runs one minute after start and
then hourly: request logs older than `LOG_RETENTION_DAYS` (default 90) and audit entries
older than `AUDIT_RETENTION_DAYS` (default 365) are deleted, `0` keeps a table forever.
The same pass removes login attempts older than 24 hours, expired dashboard sessions and
stale usage counters.
