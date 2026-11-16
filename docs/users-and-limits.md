# Users, roles and limits

This page covers who can do what in the management API and dashboard, how logins are
protected and audited, and the per-project rate limits, token budgets, metrics and
retention that apply to the client API. Endpoint details are in the
[API reference](api.md).

## Roles

The first-run account (`ADMIN_USER`) is an `admin`. Admins create further users in the
dashboard (**Users** tab) or via `POST /admin/api/users`. Every user has exactly one role:

| Role     | Model connections, RAG stores, documents | Projects | API keys | Users, audit log |
|----------|------------------------------------------|----------|----------|------------------|
| `admin`  | full access | all projects, all metrics | own and everyone's; only role that may delete one | full access |
| `editor` | create, edit, delete, upload, test, search | create (becomes a member); read, edit, delete, rotate key, metrics and members only for projects it belongs to | own; may create `management` keys | list active users (`/users/lite`) |
| `viewer` | read and search only | read and metrics only for projects it belongs to | own `gateway` (`sk-user-…`) keys for member projects — **which means a viewer can spend**, see below; no `management` keys | — |

Writes the role does not allow answer `403 {"error":{"type":"forbidden"}}`. Everyone can
change their own password (`POST /admin/api/me/password`), manage their own API keys and
read `/admin/api/system` (the PostgreSQL and pgvector versions in it are shown to admins
only).

### A role bounds the admin surface, not spend

The table above is about `/admin/api` and the dashboard. It is not a spending limit, and
`viewer` is not a read-only *account* — only a read-only *administrator*. The key routes
are deliberately role-free, because every account manages its own credentials, so a
`viewer` can mint itself an `sk-user-…` gateway key granting the projects it is a member
of, and send traffic through `/v1`. That spend is charged to those projects' budgets and
attributed to the key in the usage records, exactly as an editor's or an admin's would
be.

What bounds that spend is never a role. It is:

- the per-project rate limits and token budgets the key runs under (below), and
- the optional per-key sub-limits set when the key is created ([Key sub-limits](#key-sub-limits)).

A user carries no limit of its own; limits live on the project and on the key.

**To cap one user**, give their key a sub-limit. Note that `0` there means *unlimited*
([Zero means unlimited](configuration.md#zero-means-unlimited)), not *nothing*, so
capping means setting a small ceiling — setting it to zero removes the ceiling instead
of applying one.

**To stop them outright**, revoke the key (`POST /admin/api/keys/{id}/revoke`), or
deactivate the account (`PUT /admin/api/users/{id}` with `is_active: false`).
Deactivation stops every key the user holds at once, at `/v1` with
`401 key_owner_inactive`.

**Two things that look like they would stop a key and do not:**

- **Dropping the user's project membership.** A gateway key carries its own grants,
  validated once when the key was created; `/v1` consults only those and never rechecks
  `project_members` ([API keys](#api-keys) below). The user loses the project in the
  dashboard while the key keeps spending against it.
- **Setting a limit to `0`.** That is the unlimited value, as above.

**Demoting the user to `viewer`** takes away the admin surface and nothing else on the
gateway side. Role is intersected with a key's scopes on `/admin/api` only, where it is
read live on every request, so a demotion narrows the `sk-mgmt-…` keys the user holds
immediately. It changes nothing about an `sk-user-…` key: `/v1` does not consult roles,
and every gateway scope (`chat`, `models`, `embeddings`, `retrieve`, `rerank`) is one every
role including `viewer` already covers.

Roles do still bind where they are the control: a `viewer` cannot put a `write`, `admin`
or `keys` scope on any key it mints, and a `management` key needs at least `editor` plus
an interactive session. Recorded as ADR-003.

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
  deactivate, delete or [anonymise](#anonymisation-kvkk) their own account (`400`).
- `POST /admin/api/users/{id}/reset-password` sets a new password and revokes the user's
  sessions; `POST /admin/api/users/{id}/sessions/revoke` only signs the user out everywhere.
  It cannot be used on your own account (`400`): you change your own password with
  `POST /admin/api/me/password`, which asks for the current one.
- Passwords must be at least 12 characters and at most 72 bytes (bcrypt's input limit);
  usernames at most 64. The same rule applies everywhere a password is set: creating a
  user, resetting or changing a password, the first-run setup, `ragmux reset-password`
  and `ADMIN_PASSWORD`. Existing passwords are not re-checked; they keep working until
  they are changed.
- Changing your own password (`POST /admin/api/me/password`) signs out every other
  session of the account; the session that made the change stays valid. A session may
  type at most five wrong passwords for it (and for the
  [two-factor](#two-factor-authentication-totp) actions that also ask for the password);
  the fifth ends the session. A right password gives the five back.
- `POST /admin/api/users` accepts `project_ids`; the memberships are written in the same
  transaction as the account, and an unknown id fails the whole request with `422`.
- `GET /admin/api/users` reports `project_count`, `active_sessions`, `totp_enabled` and
  `anonymized_at` (`null` unless the account was [anonymised](#anonymisation-kvkk)) per user;
  `GET /admin/api/me` reports `session_expires_at` and whether the session is a bearer
  token (`session_bearer`).

## Anonymisation (KVKK)

A data-subject request to erase a person's data must not wipe the usage history that
accounting and abuse reviews rely on, so Ragmux anonymises instead of deleting: the
personal fields are replaced and the rows stay. Use anonymisation, not
`DELETE /admin/api/users/{id}`, for such a request.

Both operations replace the value with **one random pseudonym** (`anon-` and 16 hex digits)
generated per operation and written to every row the operation touches. The pseudonym is
not a hash of the original: nothing derived from the original is kept, so it cannot be
reversed, and the original value is not written to the audit log either. Two operations
never share a pseudonym. Both endpoints are for a signed-in admin **session**; a
management key, even one with the `admin` scope, gets `403` with `code: "session_required"`.
A deliberate trade-off: an application cannot automate these requests with a key, and a
leaked key cannot make a change that cannot be undone.

**A user.** `POST /admin/api/users/{id}/anonymize` with `{"confirm": "<the account's current
username>"}` (exactly as stored; a mismatch is `400`). In one transaction:

- the username becomes the pseudonym, the password becomes a random one nobody knows, the
  account is deactivated, `users.anonymized_at` is set and the second factor (secret,
  recovery codes, open challenges) is removed;
- sessions and project memberships are deleted; every key of the account is revoked and
  renamed `key-<id>` (a key name is free text and can be a person's name);
- in the audit log, the `actor_username` of the account's own entries, `details.username` of
  the entries about it, `details.name` of the `apikey.*` entries for its keys (also entries
  the account made on someone else's key; the key id stays in `target_id`), and the
  `login.failure`, `login.rate_limited` and `login.locked` entries that named the account
  but had no actor, matched on the name case-insensitively and with no lower time bound;
- the account's rows in the login-attempt counters are deleted.

`request_logs` is not touched: it points at the account by id, carries no name and keeps
its usage and cost figures, so totals do not move. You cannot anonymise your own account
(`400`), as you cannot deactivate or delete it; another admin does it. The last active
admin is refused too (`409`), and a second call on an account is `409` with `code:
"already_anonymized"`. The old username can be registered again.

An anonymized account is closed for good: `PUT /admin/api/users/{id}` with `is_active: true`
and `POST /admin/api/users/{id}/reset-password` are refused with `409` and `code:
"already_anonymized"` (`ragmux reset-password` fails for it too), because anything created under
it afterwards, a key name for instance, could not be anonymized again. Create a new account
instead. The operation is audited as
`user.anonymize` with the pseudonym only.

**An end user.** `POST /admin/api/end-users/anonymize` with `{"end_user": "<id>",
"project_id": 3}` replaces the identifier in `request_logs.end_user` (in batches of 5000
rows), `end_user_usage`, `alert_events` and the subject of alert webhook deliveries.
Without `project_id` every project is covered. The response is the pseudonym and the number
of rows changed per table. If a call fails partway, call it again: rows already changed keep
their pseudonym and the rest get a second one. Audited as `end_user.anonymize` (project,
counts and pseudonym; not the identifier).

**What it does not reach.** State these to whoever files the request:

- It looks backwards. An end user who shows up again, or a login that types the old name,
  writes new rows with the original value, and there is no second pass for them.
- An end user's current rate and token windows start again from zero if the same identifier
  keeps sending: the counters in `end_user_usage` are keyed by the identifier, so the next
  request opens a new row under the original value and the ceilings count from nothing.
  Anonymise after the person has stopped using the application.
- A failed-login entry names no account, only a typed name. If the name belonged to someone
  else earlier (an account deleted, the name registered again), those entries are attached
  to this pseudonym too. They prove no identity.
- The name of an account deleted **before** this feature stays in the audit log; the
  endpoint works on an existing account only.
- A key deleted before v0.8, whose `apikey.create` entry has already been removed by
  retention, leaves a name in its `apikey.revoke` / `apikey.delete` entry until retention
  removes that too. Newer entries carry the owner's id, so the gap closes over time.
- `audit_logs.ip` is kept, as a security record with a legitimate interest, for
  `AUDIT_RETENTION_DAYS` (default 365; `0` keeps entries forever).
- Application logs (stdout) and backups are outside it: keeping and expiring them is the
  operator's job.

## API keys

Besides the one key a project carries (`sk-proj-…`, rotated from the project page and
labelled *the project's default key* in the dashboard), every user can mint their own
credentials in the **API keys** tab or through `/admin/api/keys`. They come in two kinds:

| Kind | Prefix | Calls | Carries |
|---|---|---|---|
| `gateway` | `sk-user-…` | `/v1` | a set of granted projects, its own rate limits and budgets, scopes `chat`, `models`, `embeddings`, `retrieve` and `rerank` |
| `management` | `sk-mgmt-…` | `/admin/api` | scopes `read`, `write`, `admin`, `keys`; no projects and no limits |

All three prefixes keep the `sk-` head so secret scanners that watch for OpenAI-shaped
keys keep firing on a leak. The key is shown once at creation and only its first 15
characters (`key_prefix`) are stored for identification, next to a SHA-256 hash.

The **API keys** tab lists the scopes of the chosen kind as a checklist and refuses to save
with none ticked, because an empty list would grant every scope. Editing a key keeps a
scope the dashboard does not recognise, ticked, so saving does not silently drop it.

**A key is never issued beyond its owner.** Scopes only ever narrow, grants are held at
creation to what the owner can reach, and the owner is looked up on *every* request rather
than copied onto the key. What that lookup can still take away, though, depends on the
surface — a grant, once given, is not rechecked.

What that lookup enforces differs by surface. On `/admin/api` the effective permission is
the key's scopes intersected with the owner's role, so demoting a user narrows every
`sk-mgmt-…` key they hold at once. `/v1` does not read the role at all — it checks only
that the account is active — so a demotion changes nothing about an `sk-user-…` key:
every gateway scope is one every role including `viewer` already covers. See
[A role bounds the admin surface, not spend](#a-role-bounds-the-admin-surface-not-spend).

**Deactivating** an account stops both kinds at once: the key stops resolving entirely
(`401 key_owner_inactive` at `/v1`).

| Kind | Scope | Allows |
|---|---|---|
| gateway | `chat` | `POST /v1/chat/completions` |
| gateway | `models` | `GET /v1/models`, `GET /v1/models/{id}` |
| gateway | `embeddings` | `POST /v1/embeddings` |
| gateway | `retrieve` | `POST /v1/retrieve` |
| gateway | `rerank` | `POST /v1/rerank` |
| management | `read` | every listing and metrics route (the `viewer` set) |
| management | `write` | the routes that need `editor` |
| management | `admin` | the routes that need `admin` (users, audit log, login security) |
| management | `keys` | `/admin/api/keys*`, separate from `write` so a leaked write key cannot mint more keys |

All five gateway scopes are granted by default: a gateway key created with no `scopes`, or
with an empty list, gets `chat`, `models`, `embeddings`, `retrieve` and `rerank`. A scope
name outside that list is a `400` whose message lists the valid values. The check lives in
the application, not in a database constraint, so a future scope needs no migration. A
management key needs at least one scope and the `editor` role; the `admin` scope needs the
`admin` role.

**Upgrading from v0.4.** Migration `0018` gives the three new scopes to every existing
gateway key that carries `chat`: keys that could call chat completions can call the new
endpoints too. This is a deliberate widening of what those keys may do, and the changelog
marks it as security-relevant. Revoked and expired keys are included, so reviving or
extending one later does not change its reach. The migration only adds: it is idempotent,
it leaves `updated_at` alone, and it does not touch keys that carry only `models` or any
management key. To keep a key out of the new endpoints, edit its scopes after upgrading.

**The project's default key** (`sk-proj-…`) carries no scopes at all, so it reaches the new
endpoints automatically, like every other `/v1` route. It cannot be narrowed; mint a
user-owned gateway key with the scopes you want when you need that.

These actions refuse an api-key principal outright and answer
`403 {"error":{"code":"session_required"}}`: changing your own password, resetting
someone else's, creating a `management` key, and everything about
[two-factor authentication](#two-factor-authentication-totp) (switching it on or off,
new recovery codes, an admin's reset). A leaked key must not be able to lock its owner
out of the account or mint a successor.

**Projects a gateway key may call** are the grants listed on the key, not the owner's
project memberships. Membership is a dashboard-visibility concept; requiring both would
mean an admin editing a member list silently breaks a production key. The grants are
validated once, when the key is created or updated (you can only grant projects the
*owner* can access), and from then on `/v1` consults only them. A key whose last grant
was deleted routes nowhere and fails closed. How a request picks one is described in the
[API reference](api.md#choosing-a-project).

**Expiry and revocation.** `expires_at` is optional; `POST /admin/api/keys/{id}/revoke`
sets `revoked_at`. Both are part of the single lookup that resolves a key, so there is no
cache to invalidate and a revocation takes effect on the very next request. A key that no
longer resolves answers `401`; the body carries `code` `key_revoked`, `key_expired` or
`key_owner_inactive` — an unknown key keeps the plain `invalid project api key` body, so
the extra detail only ever reaches someone who already holds the key bytes. The hourly
retention job deletes revoked or expired rows after 30 days, but never one that request
logs still attribute usage to; that is what keeps billing history intact.

Prefer **revoke** over **delete**: `DELETE /admin/api/keys/{id}` is admin-only and
answers `409 key_in_use` while any request log references the key.

### Network allowlists

A gateway key, and a project's own `sk-proj-…` key, can be restricted to the networks and the
browser origins it may be used from. A key that leaks is then useless from anywhere else, and
a key embedded in a web page cannot be used from another site. Each key carries its own two
lists (there is no project-wide ceiling and no intersection of lists):

| List | On a user key | On a project key | Entry |
|---|---|---|---|
| CIDR | `allowed_cidrs` | `key_allowed_cidrs` | an IPv4 or IPv6 network, or one address (`203.0.113.7` is stored as `203.0.113.7/32`); an IPv4-mapped IPv6 address counts as IPv4 |
| Origin | `allowed_origins` | `key_allowed_origins` | `https://app.example.com`, `http://localhost:3000`, or `https://*.example.com` |

**The CIDR list is only as trustworthy as the client address.** The gateway checks the TCP peer
address, or, when `TRUST_PROXY_HEADERS=true`, the address the proxy reported in
`X-Forwarded-For` / `X-Real-IP` (see [Behind a reverse
proxy](configuration.md#behind-a-reverse-proxy)). With `TRUST_PROXY_HEADERS=true` and an empty
`TRUSTED_PROXY_CIDRS` those headers are accepted from every connection, so a client can send
any address it likes and walk around the list. Set `TRUSTED_PROXY_CIDRS` to the network of your
reverse proxy before relying on a CIDR list. The gateway does not stop for this: it logs a `warn`
at start-up when any key or project key holds a CIDR list in that configuration, and an admin
API save of such a list answers with a `warnings` entry. Behind a reverse proxy without
`TRUST_PROXY_HEADERS` the peer is the proxy itself, so a list of client networks would admit
nothing; enable the setting or do not use CIDR lists.

Both lists hold at most 32 entries and are empty by default, which is today's behaviour: no
restriction. A wildcard origin matches a subdomain at any depth and never the apex itself
(`https://*.example.com` admits `https://a.example.com` and `https://a.b.example.com`, not
`https://example.com`); a lone `*` is refused. Origins are compared in lower case without a
trailing `/`, and the default port (`:443` for `https`, `:80` for `http`) is dropped on both
sides, as a browser leaves it out of the `Origin` header.

A request that a list does not admit is answered `403` with `type: "permission_error"` and
`code: "ip_not_allowed"` or `"origin_not_allowed"` before anything else runs: no project is
selected, no limit is spent and no request log row is written. The refusal is counted in
`ragmux_gateway_key_denied_total{reason}` and logged at `warn` with the key's id. A key that
does not resolve keeps its `401`, so the lists never tell a real key from a made-up one. The
same check guards `/v1`, `POST /v1/messages` with `x-api-key` and `/mcp`; the `CORS_ORIGINS`
check of `/mcp` still applies as well, and a browser also needs the page's origin in
`CORS_ORIGINS` for the response to be readable.

**An origin list makes the key a browser key.** When the list is not empty, a request without an
`Origin` header is refused, as is one whose `Origin` is `null` or matches no entry. A server-side
caller sends no `Origin`, so give it a separate key with a CIDR list instead. The header is sent by
browsers; any other client can set it to whatever it likes, so an origin list stops other web
pages from using a leaked key and nothing else. It is a browser signal, not a security boundary,
and is best combined with a CIDR list.

The lists are set through the admin API ([API keys](api.md#api-keys), [Projects](api.md#projects)).
They are read by the binary that serves the request: a replica running an older version, or a
rollback to one, does not know the columns and applies no allowlist at all. Mutual TLS is not
done by the gateway; terminate it at the reverse proxy, as in
[Mutual TLS at the proxy](scaling.md#mutual-tls-at-the-proxy).

### Key sub-limits

A gateway key carries the same four limits as a project (`rate_limit_rpm`,
`rate_limit_tpm`, `budget_daily_tokens`, `budget_monthly_tokens`, `0` = unlimited), kept
in `key_usage` exactly the way `project_usage` holds the project's. They are a second,
independent ceiling under the project's, not a replacement: the two tiers count against
different rows, so each is evaluated against its own counters in the order monthly →
daily → TPM (project before key) and the **first violation wins**. The `429` body gains
`"scope"`, either `"project"` or `"key"`; `code` and `type` are unchanged.

The `x-ratelimit-*` and `x-ragmux-budget-*` headers report whichever tier has the smaller
remaining allowance, so a client always sees the bound it will hit first. For the
requests-per-minute limit both minute slots are reserved atomically, project first; if
the key's slot overflows, both are released again so neither tier keeps a phantom
request.

`GET /admin/api/keys/{id}/usage` returns the key's own counters in the same shape as
`GET /admin/api/projects/{id}/usage`, with `api_key_id` in place of `project_id`.

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

The counters are keyed on the lower-cased username, as the account itself is: `ali`,
`ALI` and `Ali` share one budget. They only count password failures; wrong
[two-factor](#two-factor-authentication-totp) codes are recorded in the same table under
their own stage and never reach these limits.

A failed login (`401`) carries `attempts_remaining`: the per-user budget minus the
failures recorded for that username in the last minute, never below zero and absent
when `LOGIN_USER_LIMIT_PER_MIN` is `0`. The number only depends on the recorded
attempts, so a wrong password and a username that does not exist get identical
responses and the endpoint does not reveal which accounts exist.

Admins see the state of the limiter with `GET /admin/api/security/logins`: failed
password attempts (wrong second-factor codes are not counted) in the last hour and 24 hours, and the username/address pairs currently locked
out with the time the lockout ends. The list is computed with the limiter's own rule, so
it matches exactly what `POST /admin/api/login` refuses.

The client IP is the TCP peer address unless `TRUST_PROXY_HEADERS=true`, in which case
the last `X-Forwarded-For` entry (or `X-Real-IP`) is used, optionally restricted with
`TRUSTED_PROXY_CIDRS`; only enable it behind a reverse proxy (see
[Configuration](configuration.md#behind-a-reverse-proxy)).

## Two-factor authentication (TOTP)

Any account can switch on a time-based one-time password (RFC 6238: HMAC-SHA1, six
digits, 30 seconds, one step of clock drift either side) from an authenticator app.
Nothing is required of anybody: there is no instance-wide setting that forces it.

**Switching it on** takes two calls from a signed-in dashboard session; these and the two
that manage it afterwards each ask for the account password again:

1. `POST /admin/api/me/totp/setup {"password"}` returns a new `secret` and an
   `otpauth_uri` for the app (a client draws the QR code from it). Until step 2 the login
   stays one-step, and another `setup` replaces the pending secret. With two-factor
   authentication already on it answers `409 totp_already_enabled` and changes nothing:
   to move to a new phone, `disable`, then `setup` and `enable` again. There is a short
   window between `disable` and `enable` in which the account has no second factor.
2. `POST /admin/api/me/totp/enable {"password", "code"}` checks the first code, switches
   it on, returns **ten recovery codes once** (`xxxxx-xxxxx`, 50 bits each, stored only as
   SHA-256 hashes) and ends the account's other sessions.

`POST /admin/api/me/totp/disable {"password", "code" | "recovery_code"}` and
`POST /admin/api/me/totp/recovery-codes {"password", "code"}` then turn it off and replace
the recovery codes. `GET /admin/api/me` reports `totp_enabled`, so a client knows which of
the two sets of calls to offer.

**Signing in** is then two steps: `POST /admin/api/login` with the password answers a
`challenge_token` (five minutes, single use, closed after five wrong codes) and no
session, and `POST /admin/api/login/mfa` with the challenge and a code (or a recovery
code, once) opens it. A code is accepted once: a step already used is refused. This
holds for `"bearer": true` as well. A script that logs in with a password and takes the
token from the body therefore stops working for an account that turns two-factor
authentication on; **automation uses a management key** (see
[API keys](#api-keys)). Management keys minted before the account switched TOTP on stay
valid: review them, and revoke what is not needed.

**The lock.** Five wrong codes in 15 minutes, counted together over `login/mfa`, `enable`,
`disable` and `recovery-codes`, lock the account's second factor. While it is locked every
one of those answers `429` with `Retry-After` even for a right code or recovery code, and
a right password at `/login` gets `429` instead of a challenge; wrong passwords keep
answering `401` as before. The lock lifts by itself when the oldest of the five leaves
the 15-minute window. Parallel requests cannot go past it: a check of one account runs
alone, and a request that arrives while another is running answers `429` with
`Retry-After: 1` and is not counted.

What this costs. Someone who knows the password can keep the lock closed by typing five
wrong codes every 15 minutes, and then neither the right code nor `disable` works for the
real user. A user who still has a session can end the attack by changing the password
(`POST /admin/api/me/password` closes the open challenges, so the attacker can no longer
get to the code step). A user without a session needs another admin or the server:

- another admin's `POST /admin/api/users/{id}/reset-password` forgets the account's wrong
  codes and closes its open challenges, and leaves two-factor authentication on;
- another admin's `POST /admin/api/users/{id}/totp/reset` does the same and switches
  two-factor authentication off;
- `ragmux reset-password <user> ...` on the server does what `reset-password` does, and
  with `--disable-totp` also deletes the secret and the recovery codes. Without that flag
  it leaves two-factor authentication alone. With one admin and no phone, this is the way.

**Admins** reset another user's two-factor authentication with
`POST /admin/api/users/{id}/totp/reset` (secret, recovery codes, wrong-code history and the
user's sessions go; an admin cannot do it to themselves, `disable` is theirs). An admin who
can reset a password and a second factor can sign in as that user; that is what the admin
role is. Both are audited (`user.reset_password`, `user.totp_reset`).

**Audit.** `totp.setup`, `totp.enable`, `totp.disable`, `totp.recovery_regenerate`,
`login.mfa_failure`, `login.mfa_locked`, `login.recovery_code_used` (with the codes left),
`user.totp_reset`. The user is the target of the entry (`target_id`); `details` never carry
the username, and `details.via` is one of `login`, `enable`, `disable`, `recovery_codes`.

**Operating notes.**

- The secret is stored sealed with `SECRET_KEY` (AAD bound to the account), so
  `ragmux rotate-key` re-encrypts it with the provider keys and a lost key loses it.
- **Do not go back to a binary older than v0.8 after an account has turned two-factor
  authentication on.** That binary does not know the second factor and lets the password
  in alone.
- An abandoned setup (a secret, never enabled) does not make the login two-step.
- Expired challenges are removed by the hourly retention pass.

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
`user.revoke_sessions`, `user.anonymize`, `end_user.anonymize`, `apikey.create`, `apikey.update`, `apikey.revoke`,
`apikey.delete`, `password.change`, `totp.setup`, `totp.enable`, `totp.disable`,
`totp.recovery_regenerate`, `user.totp_reset`, `login.mfa_failure`, `login.mfa_locked`,
`login.recovery_code_used`, `logout`, `setup.complete` for the first
administrator created through the first-run form, and `audit.exported` for every
export of the log (its `details` hold the filters used). The `apikey.*` entries record
the key's name, kind, owner, scopes, grants, limits and 15-character display prefix —
never the credential or its hash.

Most entries have a signed-in actor, but two kinds do not. Failed logins record
`actor_username` without an id, and `ragmux reset-password` (see
[Configuration](configuration.md#reset-password)) records the fixed actor `cli` with a
null `actor_user_id` and `ip` `cli`; its `details` carry `via`, `host`, `os_user`,
`revoked_sessions` and `revoked_keys`. That is the one action with no account behind it,
so an emergency reset is still visible in the trail.

Admins read the log with
`GET /admin/api/audit?limit=100&action=project.&actor_user_id=1&since=…&until=…&before=…`
(`action` is a prefix match, `since`/`until` bound the time window, `before` pages
backwards; the response carries `entries`, the `total` for the filter and `has_more`)
or in the **Audit** dashboard tab, and download it as NDJSON with
`GET /admin/api/audit/export` and the same filters (at most 100 000 rows per call; use
`before` to slice a larger range). Entries are kept for `AUDIT_RETENTION_DAYS`
(default 365); export before they expire if you need a longer history.

## Rate limits and budgets

Every project has four optional limits (`0` = unlimited, see
[Zero means unlimited](configuration.md#zero-means-unlimited)), set at creation or with
`PUT /admin/api/projects/{id}` and shown in the dashboard's project form:

| Field | Meaning |
|---|---|
| `rate_limit_rpm` | Requests per UTC minute |
| `rate_limit_tpm` | Prompt + completion tokens per UTC minute |
| `budget_daily_tokens` | Prompt + completion tokens per UTC day |
| `budget_monthly_tokens` | Prompt + completion tokens per UTC calendar month |

Counters live in the `project_usage` table, so several gateway replicas share them. All
windows are aligned to UTC boundaries (`date_trunc` of minute, day and month), not sliding.
The limits cover every `/v1` call that spends tokens: chat completions, embeddings,
retrieve and rerank. The tokens of an embeddings call count toward the project's limits, and
the tokens of an LLM or API rerank are added to the request that ran it. When a limit is
hit the endpoint answers

```json
HTTP 429  Retry-After: 42
{"error":{"message":"Rate limit reached: 2 requests per minute for this project. Retry after 42 seconds.",
          "type":"rate_limit_exceeded","code":"rate_limit_rpm"}}
```

`type` is `rate_limit_exceeded` for the per-minute limits and `insufficient_quota` for
budgets; `code` is one of `rate_limit_rpm`, `rate_limit_tpm`, `budget_daily`,
`budget_monthly`; a session over its [token cap](#sessions) gets `session_token_cap`, the
one denial without a `Retry-After`. The body's `scope` says which tier denied: `project`, `key`,
`session` or, for a project's [per-end-user ceiling](#end-users-and-tags), `end_user`. `Retry-After` is the number of seconds until the violated window resets
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
characters per token (client messages plus the project system prompt) before any
retrieval. When a project's RAG store then supplies context, the per-minute limit is
checked a second time against the prompt plus the retrieved block (estimated with the
store language's ratio, see [Context and limits](rag.md#context-and-limits)); a request
over the limit by then is a `429` `rate_limit_tpm` and the context is not sent. That second
check takes no further request slot. Budgets use recorded usage only. A single request can therefore overshoot a
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
three years by the hourly retention job, in `key_usage` as well as `project_usage`.

A user-owned gateway key adds a second ceiling under the project's; see
[Key sub-limits](#key-sub-limits) for how the two interact and which one the response
headers describe.

### Budgets are counted in tokens, not money

Ragmux 0.4 estimates what each request cost and shows it on the dashboard, but **cost is
informational: no limit is enforced in money.** A request is never refused because a spend
figure was reached, and the estimate never feeds `rate_limit_tpm`, `budget_daily_tokens` or
`budget_monthly_tokens` — those stay token counters. To cap spend, cap tokens. A project can
be given a monthly dollar budget since 0.8, but only to be *warned* about it; see
[Limit alerts](#limit-alerts).

A rerank is priced with the connection that ran it: an API reranker (`cohere_rerank`,
`voyage_rerank`) at its own price row (Cohere per search unit, Voyage per token), and the
tokens of an LLM rerank at the chat connection that did the work. A rerank connection with
no price row adds 0 to `cost_micros`. The rerank tokens count toward the limits above as
before. See [Rerank cost](rag.md#reranking).

The estimate comes from the [price table](api.md#model-prices), which ships current
public list prices and is editable per model. It is not a bill: providers round,
discount and change prices without telling the gateway, and a model with no matching
price row adds a cost of zero and is logged with `cost_source: "none"` rather than a guess
(a rerank that ran on another connection with a price row still adds its own cost).

## Limit alerts

A project can ask to be told, by a signed webhook, when a limit is nearly used up, instead of
finding out from a `429`. The alerts are off until a project turns them on and registers a
webhook endpoint for them. Nothing in the request path sends or waits for them: once a minute one
replica reads the usage counters and queues the warnings, and every replica's delivery worker
sends them. A receiver that is slow or down delays or loses a warning (a delivery is tried up to five
times, the first send and four retries, over about two and a half hours) and never slows or fails a request.

- **Thresholds.** Fixed: 80 % and 100 %, each sent once per limit and window. There are no custom
  levels. If a limit jumps over both within a minute, both are sent, 80 first.
- **What is watched.**

  | Subject | Limits | Sent at |
  |---|---|---|
  | The project | `budget_daily_tokens`, `budget_monthly_tokens` | 80 %, 100 % |
  | A gateway key granted to the project | its `budget_daily_tokens`, `budget_monthly_tokens` | 80 %, 100 % |
  | An [end user](#end-users-and-tags) of the project | the project's per-end-user daily and monthly token ceilings | 100 % only |
  | The project's money budget | `budget_monthly_usd` (see below) | 80 %, 100 % |

  Per-minute limits (`rate_limit_rpm`, `rate_limit_tpm`, the end-user and key equivalents) and the
  session token cap send nothing: they trip and recover within minutes, and
  [`deploy/prometheus/alerts.yaml`](../deploy/prometheus/alerts.yaml) already covers them. End
  users warn at the ceiling only because a project can have thousands of them. A key budget is the
  key's own, counted across every project the key may use; each of the key's projects that has
  alerts on is told, so the receiver of a project hears about the keys it lets in.
- **Once per window.** The window is the limit's own: the UTC day or month. The same breach is not
  sent twice, a new day or month warns again, and **raising a limit and crossing it again is a new
  alert** (the limit value is part of what makes an alert the same alert).
- **The monthly money budget** is set per project, in USD millionths (`1000000` is one dollar). It
  is an estimate and a lower bound: it adds up the [estimated cost](#budgets-are-counted-in-tokens-not-money)
  of the month's requests, and **a request whose model has no price adds 0** and is counted
  separately. It **only warns**: no request is ever refused, delayed or answered `429` because the
  money budget was reached. Token budgets keep refusing as before.
- **Unpriced traffic.** While alerts are on, the endpoint is registered and a money budget is set, a
  project with requests of the month that had no price gets one `budget.unpriced_traffic` event a
  month (in the first minute after the first such request, or after the budget was set), so a
  receiver knows the money figures are lower than the truth. It carries no threshold.
- **Setting it up** (editor or admin of the project; `GET /alerts` is open to any member):

  | Method | Path | Purpose |
  |---|---|---|
  | `GET`, `PUT` | `/admin/api/projects/{id}/alerts` | `{"enabled": true, "budget_monthly_usd_micros": 25000000}`; `0` is no money budget |
  | `GET`, `PUT`, `DELETE` | `/admin/api/projects/{id}/webhooks/alerts` | `{"url": "https://…"}`; `PUT` registers (`201`, with the secret) or changes the URL (`200`) |
  | `POST` | `/admin/api/projects/{id}/webhooks/alerts/rotate-secret` | New secret, shown once; the old one keeps signing for 24 hours |

  The same `webhooks/{purpose}` routes register the `guardrail` endpoint. The secret
  (`whsec_…`) is in the answer that creates or rotates it and nowhere else, so a lost one is
  replaced by rotating. The URL must be `https` — `http` only for a host on
  `PRIVATE_WEBHOOK_ALLOWLIST` — without credentials, at most 2048 bytes, and its host must not
  resolve only to private or local addresses unless that same allowlist names it
  ([`PRIVATE_UPSTREAM_ALLOWLIST` does not apply](configuration.md)); a URL that breaks this is a `400`.
  Reading a webhook takes the editor role because a URL often carries a token.
- **The payload** is signed like every Ragmux webhook (`webhook-id`, `webhook-timestamp`,
  `webhook-signature`; the `webhook-id` is the payload's `id`, the same on every retry):

  ```json
  {"type":"limit.threshold","id":"evt_…","created_at":"2026-11-06T09:14:02Z",
   "project":{"id":12,"name":"support-bot"},
   "subject":{"kind":"project","id":"12"},
   "limit":"budget_monthly_tokens","threshold":80,"used":812345,"limit_value":1000000,"ratio":0.812,
   "window_start":"2026-11-01T00:00:00Z","window_end":"2026-12-01T00:00:00Z"}
  ```

  `subject.kind` is `project`, `key` (`id` is the key's id) or `end_user` (`id` is the end user as
  the client sent it). `limit` is `budget_daily_tokens`, `budget_monthly_tokens` or
  `budget_monthly_usd`. On a money event `used` and `limit_value` are USD millionths, and two more
  fields say what the figure is: `"estimate": true` and `"unpriced_requests": 3`, the month's count
  of requests that added 0. The unpriced notice is:

  ```json
  {"type":"budget.unpriced_traffic","id":"evt_…","created_at":"…","project":{"id":12,"name":"…"},
   "subject":{"kind":"project","id":"12"},"limit":"budget_monthly_usd",
   "window_start":"…","window_end":"…","estimate":true,"unpriced_requests":3}
  ```

  with no `threshold`, `used`, `limit_value` or `ratio`; `unpriced_requests` is the count when the
  event was made and is not updated, the current one arrives with the next money event.
- **A receiver that is gone.** If it answers `410 Gone`, its endpoint is closed, the deliveries
  still waiting for it fail, and the audit trail gets one `webhook.endpoint_disabled` entry (actor
  `system`, the project id and the purpose, never the URL), however many deliveries heard the 410
  at once. Saving a URL again opens it. The alerts that endpoint had not received are forgotten at
  the same time, and so are they when the endpoint is deleted: once it is open again (or a new one
  is registered) a limit that is still crossed is sent again, instead of being lost for the rest of
  its window. An alert the receiver did get is not repeated.
- **What is audited.** `project.alerts_update`, `project.webhook_register`,
  `project.webhook_update`, `project.webhook_rotate_secret`, `project.webhook_delete`; none of them
  writes a URL or a secret, only the host.
- **Delay.** A warning is queued within a minute of the counters crossing a level, plus the
  delivery time. With several replicas a round may run more than once a minute, which costs a few
  small queries and never a second alert. A round that finds the window just closed (the previous
  UTC day or month, for up to six hours after it ended) still checks it, so a limit crossed in the
  last minute, while no round ran, is reported after the restart or outage rather than lost;
  further back than that a missed breach is not reported. A closed window is only checked against
  a limit that was in force while it was open: if the project (its budgets or end-user ceilings),
  the key, or the alert settings (alerts switched on, the money budget) were edited after the
  window ended, that project's or key's closed window is skipped, so a budget set or raised in the
  small hours never warns about the night before. Any edit counts, which can drop a late warning
  but never sends a false one. A project whose traffic is mostly
  unpriced reaches its money budget later than the truth, which is what the unpriced notice is for.
- **How long the events are kept.** Each warning leaves an `alert_events` row, which is what keeps
  it once-only. The janitor deletes the rows of windows that are over, never the current one or the
  one before it: an end user's after 2 days (day windows) or 2 months, the current and the previous
  calendar month kept (month windows), the same as that end user's counters, because the row holds
  the identifier as the client sent it; every other row after `LOG_RETENTION_DAYS`
  (`0` keeps them). The delivery rows follow `LOG_RETENTION_DAYS` as well.

## Sessions

An agent that calls the gateway in a loop is hard to tell apart from the traffic around it.
A client can name the run by sending `X-Ragmux-Session: <id>` on its `/v1` requests: 1 to
128 characters of `A-Z a-z 0-9 . _ : -`, sent once. A session is the pair of project and id,
so two projects can use the same id without meeting.

- **Recorded.** The id goes into `request_logs.session_id`, and each request adds itself and
  its tokens to the session's running total (`session_usage`). The id is never a metric
  label or a span attribute: it is text the client chose. The
  [admin API](api.md#projects) lists a project's sessions, most recently active first, and
  opens one to show its request rows; any member of the project can read them, as with the
  request log.
- **Capped.** `session_token_cap` on the project is the prompt plus completion tokens one
  session may spend; `0` (the default) is unlimited. A request of a session that has
  already spent that many is answered `429`:

  ```json
  HTTP 429
  {"error":{"message":"Session token cap reached (52000 of 50000 tokens used).",
            "type":"insufficient_quota","code":"session_token_cap","scope":"session"}}
  ```

  There is no `Retry-After`: a session does not reset, so waiting does not help; the
  client starts a new session. Like the budgets, the cap compares tokens already spent and
  adds no estimate, so the request that crosses it is served in full and the next one is
  refused; requests of one session that are in flight together can each pass the check
  before any is counted, so the cap can be overshot by more than one request. The denied request is a `429` row in the request log and is counted by
  `ragmux_limits_denied_total{reason="session_token_cap"}`.
- **Invalid ids.** A value outside the allowed set, an empty one, or a header sent twice is
  `400` with `code: "invalid_session_id"`. It is never ignored, because ignoring it would
  let a request escape the cap by being malformed.
- **A guard, not a lock.** The cap protects against a runaway loop that sends its session
  header. A client that sends no header is not subject to it; use the key and project
  [limits](#rate-limits-and-budgets) for that. A binary older than this feature ignores the
  header and the cap during a rolling update.
- **Retention.** A session's total is deleted when its last request is older than the
  request log retention window (`LOG_RETENTION_DAYS`), in the same pass.

## End users and tags

An application that serves many people through one project cannot tell from the request log
which of them spent what. A request can say who it is for and how to classify it:

- **`X-Ragmux-End-User: <id>`** on any `/v1` endpoint and on the MCP search, or, when the
  header is absent, the body's own field: `safety_identifier` (else `user`) on chat and
  embeddings, `metadata.user_id` on messages. 1 to 128 bytes of `A-Z a-z 0-9 . _ : @ + = / -`,
  so an e-mail address or a user id fits. A header that is not of that shape is `400` with
  `code: "invalid_end_user"`; a body value that is not is simply not recorded, with a `Warning`,
  so a client that has always put free text in `user` keeps working.
- **`X-Ragmux-Tags: a,b`** or **`ragmux.tags`** in a chat or messages body: up to 8 tags of
  1 to 64 bytes of `A-Z a-z 0-9 . _ : / -`. When both are sent, `ragmux.tags` is used and a
  `Warning` says so. Anything malformed is `400` with `code: "invalid_tags"`.

The details of the sources, the order and the errors are in
[the API reference](api.md#end-users-and-tags).

- **Recorded.** Both go into the request log row (`request_logs.end_user`, `request_logs.tags`),
  the CSV export and the admin API's request rows, and are deleted with the row by the
  retention job. They are never a metric label, a span attribute or a log field: they are text
  the client chose, and an unbounded set.
- **Capped.** The project has four optional ceilings for **one end user**, set with
  `PUT /admin/api/projects/{id}` like the others (`0` = unlimited):

  | Field | Meaning |
  |---|---|
  | `end_user_rpm` | Requests per UTC minute, per end user |
  | `end_user_tpm` | Prompt tokens per UTC minute, per end user (the estimate, as for the project) |
  | `end_user_daily_tokens` | Prompt + completion tokens per UTC day, per end user |
  | `end_user_monthly_tokens` | Prompt + completion tokens per UTC calendar month, per end user |

  Counters live in `end_user_usage`, one row per project, end user and window, and are purged
  with the other usage counters, but under a shorter retention of their own: minute rows after
  2 hours, day rows after 2 days, month rows for the current and the previous calendar month only
  (the project and key
  counters keep theirs much longer). The limiter reads only the current window, and the row
  holds the end user's id as it was sent, an e-mail address included, so that id is not kept
  longer than the ceilings need it. They are read and written only for a
  request that names an end user, under a project that sets at least one ceiling; a project without one pays nothing.
  The tiers are checked project, then key, then end user, and the first violation answers.
  A denial is the same `429` as the other tiers, with `"scope": "end_user"`:

  ```json
  HTTP 429  Retry-After: 3120
  {"error":{"message":"Daily token budget exhausted (52000 of 50000 tokens used). Retry after 3120 seconds.",
            "type":"insufficient_quota","code":"budget_daily","scope":"end_user"}}
  ```

  When an end user's ceiling is the tightest one, the `x-ratelimit-*` and
  `x-ragmux-budget-*` response headers describe it.
- **A guard, not a lock.** A request that names no end user is not held to these ceilings, and
  one that names a different id gets a fresh allowance. They protect a project from one
  heavy user among many whose id the application controls; use the key and project
  [limits](#rate-limits-and-budgets) for anything the client must not be able to avoid.
  Like the session cap, they compare tokens already spent, so the request that crosses a
  ceiling is served in full, and a binary older than this feature ignores the headers and the
  ceilings during a rolling update.
- **What the provider sees.** Only a value the request *body* carried, and, on a translated
  Anthropic request, only as a hash. The header is never sent. See
  [Providers](providers.md#anthropic).

## Tool governance

A project can decide which tools its requests may offer the model, which matters when an
agent sends every tool it has on every request. Two lists on the project,
`tool_allow` and `tool_deny`, hold patterns matched against each tool's name
(`function.name` on chat completions):

- a pattern is literal text with `*` standing for any run of characters, and nothing else;
  matching is case sensitive. `mcp__github__*` covers every tool of one MCP server. It is
  not a regular expression, so a pattern cannot be made to run long;
- **deny always wins.** A tool that matches `tool_deny` is removed;
- an empty `tool_allow` allows everything not denied; a non-empty one allows only what it
  matches.

A removed tool is stripped from the request before it goes upstream and the response
carries a `Warning` header; the request itself is not refused, so a client that sends its
whole tool box keeps working, and the model cannot call what it was never shown. Tool
calls and tool results already in the conversation are left alone, because they happened.
A `tool_choice` that cannot be honoured after the stripping is refused with `400`
(`code: "tool_not_allowed"`): naming a removed tool, or requiring a tool when none is
left. The table is in [the chat completions reference](api.md#post-v1chatcompletions).

Each request log row records `tools_denied`, the number removed, and `tool_names`, the
tools the model *called* in its answer. Only names are stored — never arguments or results —
and the names the gateway removed are written to its log at `warn` level, not to the row.
The lists are validated when the project is saved: patterns of 1 to 128 characters of
`A-Z a-z 0-9 _ . : - *`, at most 64 per list.

This is a policy on what the model is offered, not a sandbox for what a tool does. A binary
older than this feature ignores both lists during a rolling update.

## Guardrails

A project's **guardrails** are rules that read what the project's clients send and what the
model answers, and block, redact or flag a match: Turkish citizen ids (`tckn`), IBANs, a regular
expression or a word list. They are a project setting like the [tool lists](#tool-governance):
the rules, the stream window and the webhook settings live on the project, and a project
without rules is served exactly as before. The shape, the stages and the exact behaviour are in
the [API reference](api.md#guardrails); this section is about who can do what and what to expect.

- **Who edits them.** The same people who edit the project: a member with the `editor` role
  or an admin (`PUT /admin/api/projects/{id}/guardrails`). Any member can read them. A change
  is audited as `project.guardrails_update` with the rule ids; the patterns and words are not
  written to the audit trail.
- **What a client sees.** A `block` is a `400` with `code: "guardrail_blocked"` and a fixed
  message that names the stage; the rule, and of course the match, are not told. A `redact`
  is a `Warning` header on the response. If the guardrail webhook is down and the project
  chose `fail_closed` (the default), requests get `503` with `code: "guardrail_unavailable"`.
- **A block costs a request.** A blocked request takes a slot of the project's and the key's
  requests-per-minute limit like any other, so a blocked client cannot probe the rules for free.
  No tokens are spent on a request refused before the model; an answer blocked at
  `post_response` has already been paid for and is counted.
- **Thinking cannot be redacted.** A model's thinking is signed by its provider; changing a
  character makes the provider refuse it when the client sends it back. A `redact` rule on
  `post_response` therefore only flags a match inside thinking, and a `block` rule still
  blocks. `pre_request` does not read the thinking blocks of earlier turns.
- **Streams are delayed by the window.** With a `post_response` rule a stream holds back its last
  `stream_window_chars - 1` characters (128 by default), which delays the first text by roughly 0.3
  to 0.6 s. A project without a `post_response` rule streams as before. A window smaller than a
  match cannot catch it; the API answers with a warning for a window too small for `tckn` or `iban`.
- **No logprobs on a guarded answer.** `logprobs` and `top_logprobs` are dropped from requests to a project whose `post_response` rules block or redact (with a `Warning`; a project whose rules only flag keeps them), because token probabilities would spell out a match that was redacted or blocked.
- **Content is not kept.** The request log records how many matches there were and which rule ids
  matched, not the text, a hash or a position.
- **`POST /v1/moderations`** answers with the project's `pre_request` rules, for a client that
  wants to ask before it sends. A key needs the `chat` scope for it, and the call counts against the
  request limit.

## Metrics and retention

Every `/v1` call that reaches the upstream or is turned away by a limit (chat completions,
embeddings, retrieve and rerank) is written to `request_logs`, one row per request, with the
status the client received plus two gateway-internal codes. The `endpoint` column tells the
four apart; see [the request log](observability.md#the-request-log):

| Status | Meaning |
|---|---|
| `2xx` | completed; tokens from the provider's usage block, or an estimate (`estimated: true`) |
| `429` | rejected by the project's rate limit or budget (`rate_limited` in summaries) |
| `499` | the client disconnected before the completion finished; the upstream call was cancelled. Counted as an error in summaries, never sent on the wire |
| `502` | upstream failure: any `5xx` other than `503` / `504`, a transport error, or a stream that died mid-way |
| `503` / `504` | the upstream answered `503` or `504` (an Anthropic `529` becomes `503`), relayed with its `Retry-After`; `504` is also what the gateway returns when its own upstream timeout expires |
| other `4xx` | relayed from the provider |

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

Each row also records which user-owned key made the request (`api_key_id`) and who owns
it (`user_id`); both are `null` for a project's default key. The owner is denormalised so
deleting a key never destroys the history: `api_key_id` becomes `null` and `user_id`
stays. Both are in the CSV export.

A retention job (the janitor in `internal/maintenance`) runs one minute after start and
then hourly: request logs older than `LOG_RETENTION_DAYS` (default 90) and audit entries
older than `AUDIT_RETENTION_DAYS` (default 365) are deleted, `0` keeps a table forever.
The same pass removes login attempts older than 24 hours, expired dashboard sessions,
stale usage counters on both tiers, session totals idle for longer than the request log
window, and api keys revoked or expired more than 30 days ago
that no request log still references.
