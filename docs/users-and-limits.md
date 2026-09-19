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
change their own password (`POST /admin/api/me/password`) and read `/admin/api/system`
(the PostgreSQL and pgvector versions in it are shown to admins only).

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
- `POST /admin/api/users` accepts `project_ids`; the memberships are written in the same
  transaction as the account, and an unknown id fails the whole request with `422`.
- `GET /admin/api/users` reports `project_count` and `active_sessions` per user;
  `GET /admin/api/me` reports `session_expires_at` and whether the session is a bearer
  token (`session_bearer`).

## Case-insensitive usernames

Usernames are matched without regard to case: `Admin`, `admin` and `ADMIN` all sign in
to the same account, and the casing entered at creation is what is stored and shown.
Creating a user (or the first-run setup) whose name differs from an existing one only by
case answers `409 a user with that name already exists (usernames are case-insensitive)`.

Migration `0008` enforces this with a unique index on `lower(username)`. A database
that already holds names differing only by case cannot be migrated automatically
because there is no safe way to pick which account keeps the name: the gateway refuses
to start with

```
apply migration 0008_users_ci_audit.sql: ERROR: usernames that differ only by case exist (alice, bob); rename or delete the duplicates before upgrading
```

Fix it by hand before starting the new version, then start it again:

```sql
-- see who collides
SELECT id, username, role, is_active, last_login_at FROM users
 WHERE lower(username) IN (SELECT lower(username) FROM users GROUP BY 1 HAVING count(*) > 1)
 ORDER BY lower(username), id;
-- either rename the account you want to keep apart …
UPDATE users SET username = 'alice.ops' WHERE id = 7;
-- … or delete the one that is not needed (its sessions and memberships cascade;
-- audit rows keep the username and lose the actor id)
DELETE FROM users WHERE id = 8;
```

## Login protection

Login bodies are checked before anything else: the username (trimmed) must be 1–64
characters and the password 1–1024; anything else is a `400` that is neither recorded
nor counted.

Every remaining attempt is recorded in the database, so all replicas share the counters.
After `LOGIN_USER_LIMIT_PER_MIN` (default 5) failures for a username or
`LOGIN_RATE_LIMIT_PER_MIN` (default 10) failures from an IP within a minute, and after
`LOGIN_LOCKOUT_FAILURES` (default 20) failures for one **username from one IP** within
`LOGIN_LOCKOUT_MINUTES` (default 15), `POST /admin/api/login` answers
`429 {"error":{"message":"too many login attempts, try again later","type":"rate_limited","locked":false}}`
with a `Retry-After` header; `locked` is `true` when the lockout triggered rather than
a per-minute budget. The lockout is keyed on the username/address pair so that a
stranger hammering your username cannot lock you out from your own address; the
per-minute limits still slow them down. Setting a limit to `0` disables it. Successful
logins do not reset the counters; the windows simply expire. Attempts older than 24
hours are purged hourly.

A failed login (`401`) carries `attempts_remaining`: the per-user budget minus the
failures recorded for that username in the last minute, never below zero and absent
when `LOGIN_USER_LIMIT_PER_MIN` is `0`. The number only depends on the recorded
attempts, so a wrong password and a username that does not exist get identical
responses and the endpoint does not reveal which accounts exist.

Admins see the state of the limiter with `GET /admin/api/security/logins`: failed
attempts in the last hour and 24 hours, and the username/address pairs currently locked
out with the time the lockout ends. The list is computed with the limiter's own rule, so
it matches exactly what `POST /admin/api/login` refuses.

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
`user.revoke_sessions`, `password.change`, `logout`, `setup.complete` for the first
administrator created through the first-run form, and `audit.exported` for every
export of the log (its `details` hold the filters used).

Admins read the log with
`GET /admin/api/audit?limit=100&action=project.&actor_user_id=1&since=…&until=…&before=…`
(`action` is a prefix match, `since`/`until` bound the time window, `before` pages
backwards; the response carries `entries`, the `total` for the filter and `has_more`)
or in the **Audit** dashboard tab, and download it as NDJSON with
`GET /admin/api/audit/export` and the same filters (at most 100 000 rows per call; use
`before` to slice a larger range). Entries are kept for `AUDIT_RETENTION_DAYS`
(default 365); export before they expire if you need a longer history.

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

### Budgets are counted in tokens, not money

Ragmux 0.4 estimates what each request cost and shows it on the dashboard, but **cost is
informational: no limit is enforced in money.** A project cannot be given a dollar
budget, a request is never refused because a spend figure was reached, and the estimate
never feeds `rate_limit_tpm`, `budget_daily_tokens` or `budget_monthly_tokens` — those
stay token counters. To cap spend, cap tokens.

The estimate comes from the [price table](api.md#model-prices), which ships current
public list prices and is editable per model. It is not a bill: providers round,
discount and change prices without telling the gateway, and a model with no matching
price row is logged with `cost_source: "none"` and a cost of zero rather than a guess.

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

Since 0.4 a row also carries `cached_prompt_tokens` and `cache_write_tokens` (the parts
of `prompt_tokens` a provider cache served and wrote, see
[Prompt caching](providers.md#prompt-caching)) plus the estimated `cost_micros` and the
`cost_source` it came from. A `429` row costs nothing — nothing was sent upstream — and
a `499` row keeps the cost of whatever the provider had already reported.

`GET /admin/api/metrics/summary?window=24h` and `GET /admin/api/projects/{id}/metrics`
return the window summary (requests, errors, tokens, average and p95 latency, RAG
requests, `rate_limited`, estimated cost), the all-time summary, a 14-day daily series
and the 50 most recent requests; the dashboard's overview and project pages are built on
them.

A retention job (the janitor in `internal/maintenance`) runs one minute after start and
then hourly: request logs older than `LOG_RETENTION_DAYS` (default 90) and audit entries
older than `AUDIT_RETENTION_DAYS` (default 365) are deleted, `0` keeps a table forever.
The same pass removes login attempts older than 24 hours, expired dashboard sessions and
stale usage counters.
