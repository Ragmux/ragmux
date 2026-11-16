# Dashboard

`/admin/` serves a single-page dashboard (vanilla JavaScript, no build step, embedded in
the binary) over the same REST API. On a fresh database it shows the first-run setup
form; afterwards a sign-in screen that reports remaining attempts and lockouts. An account
with [two-factor authentication](users-and-limits.md#two-factor-authentication-totp) gets a
second step after the password: the six-digit code, or a recovery code through *Use a recovery
code*. A wrong code says how many tries the step has left; once the account's second factor
is locked the screen shows how long it stays locked and says that even a right code or a
recovery code is refused until then, and that the way out is another admin (*Reset 2FA* under
Users) or `ragmux reset-password --disable-totp` on the server. Screens:

- **Overview:** 24h / 7d / 30d window with deltas against the previous period, requests
  per day, traffic by project, budget watch with exhaustion forecasts, recent requests
  and a CSV export.
- **Models:** connection cards with provider capabilities, last test result and private
  upstream flag; the form can test a connection before saving it, start from a preset and
  takes an Azure OpenAI endpoint, deployment and API version.
- **RAG stores:** quota meters, drag-and-drop upload, ingestion progress, per-document
  reprocess and a retrieval tester (hybrid / vector, rerank, per-stage latencies); the rerank
  connection list follows the chosen backend. Also:
  - **Language.** The store form has a language selector (`simple` plus the ISO 639-1
    codes, named in your browser's language) in place of the old `fts_config` field. Changing
    it warns that the full-text column is rebuilt in the background and for how many chunks;
    the store shows a "rebuilding full-text" pill until it is done and a toast when it
    finishes. With the `pg_search` backend the form notes that the language has no effect on
    the lexical half. See [Store language](rag.md#store-language).
  - **Vector index.** The store panel and card show the state of the vector index (HNSW,
    HNSW over `halfvec`, building with its progress, missing, not indexable). For a width
    that is missing its index, admins get a **Reindex** button; its confirmation lists every
    store of that width, which are all affected. See [Embedding width](rag.md#embedding-width).
  - **Metadata.** The upload form takes document metadata as key / type / value rows (text,
    number, true / false, list, number list) with the limits checked in the browser; the
    document list shows each document's metadata, and a drawer edits it without
    reprocessing. After an upload that matches an earlier file, the row reads "dedup
    expected": that is a forecast from the upload response, not a stored fact (see
    [Duplicate uploads](rag.md#duplicate-uploads)). See
    [Metadata and filters](rag.md#metadata-and-filters).
- **Projects:** usage rings for the minute, day and month windows with reset countdowns,
  budget forecast banner, members, key rotation, a route editor (kind, up to eight targets
  with model override, weight and priority) and a project CSV export. The project form
  carries the RAG behaviour: context placement, retrieval policy, query strategy and, for
  `condense`, the number of history turns and the connection that condenses the query
  (those two are disabled under `last_user`). The project card summarises any setting that
  differs from the default. The form sends only the RAG fields you changed, so saving a
  project from an older tab or script cannot reset them. See
  [Context injection](rag.md#context-injection). Two more things live on a project:
  - **Tool policy.** The form has an *Allowed tools* and a *Denied tools* box (one pattern per
    line, `*` matches any run of characters, deny wins) and a *Session token cap* (`0` is
    unlimited). The patterns and the cap are checked in the browser against the same rules the
    server applies, and a message from the server is shown under the field it names. Like the
    RAG fields, they are sent only when you changed them. The project card shows a *tools* line
    when any of them is set. See [Tool governance](users-and-limits.md#tool-governance).
  - **Sessions.** The project panel lists the project's sessions, most recently active first,
    with their request count, prompt and completion tokens, and (when a cap is set) how much of
    it each has spent. *Load more* pages through a long list (a session that is active while you
    page moves to the top, so the list merges pages by session id and *Refresh* reloads it from
    the start). The page keeps the pages you loaded and the session you opened when it redraws
    itself, for example when a usage window rolls over. Selecting
    a session opens its totals and request rows (the newest 100), each with the tools the model
    called and, when the allow and deny lists removed any from the request, how many. A session
    over its cap says so. Everyone who can open the project can read its sessions; the data is
    the project's [admin API](api.md#projects) `sessions` endpoints. See
    [Sessions](users-and-limits.md#sessions).
- **API keys:** mint and revoke your own `sk-user-…` and `sk-mgmt-…` keys with a scope
  checklist, grants and limits. A gateway key also takes an *Allowed networks (CIDR)* and an
  *Allowed browser origins* list (one entry per line, up to 32 each, checked in the browser
  before the request). The form says that a key with an origin list cannot be used from a
  server, because server-side callers send no `Origin` header, and that a CIDR list is only
  as trustworthy as the client address behind a proxy; when a save answers with `warnings`
  (a CIDR list behind `TRUST_PROXY_HEADERS=true` with no `TRUSTED_PROXY_CIDRS`) they are shown
  in a dialog. See [Network allowlists](users-and-limits.md#network-allowlists).
- **Playground:** real `/v1/chat/completions` calls with a project key, streaming or JSON,
  optional extra system prompt, RAG sources as citations and the response headers.
- **Users** and **Audit log** (admins): role and failed-login tiles, filters, NDJSON export.
  On a user row, *Reset 2FA* (shown only while the user has two-factor authentication on, from
  the list's `totp_enabled`) removes it and ends their sessions, and *Anonymize* (shown only
  until `anonymized_at` is set) replaces the account's personal data for good (the dialog lists
  what changes and asks you to type the username). An anonymized account is marked with an
  *anonymized* badge and the date, and offers neither *Reset password*, *Reset 2FA*,
  *Anonymize* nor the *Active* switch: it stays closed and the API answers `409` to those.
  Your own row shows none of the reset buttons nor *Anonymize*, which the API refuses for the
  caller: use the sidebar's **Password** and **Two-factor** buttons. Below the table, a form
  anonymizes an end user for a data-subject request, for one project or all of them, and shows
  how many rows it changed. Both anonymizations work only in a signed-in session; they are
  irreversible. See [Anonymisation (KVKK)](users-and-limits.md#anonymisation-kvkk).

What a user sees follows their role: viewers get read-only screens, editors the forms,
admins the user and audit screens. The page ships its own fonts (Bricolage Grotesque,
IBM Plex Sans, JetBrains Mono, all SIL OFL, `web/fonts/`) and runs under a strict
`Content-Security-Policy`, so it makes no third-party requests.

**Theme.** The dashboard has a dark and a light theme and follows the operating system's
`prefers-color-scheme` by default. The theme button at the bottom of the sidebar cycles
system, light and dark; the choice is kept in this browser's `localStorage` under
`ragmux.theme` (`system`, `light` or `dark`) and also applies to the sign-in screen. While the
setting is `system`, the page follows an operating-system change live. If the browser blocks
`localStorage`, the page still works and the choice lasts until the page is reloaded.

**Two-factor authentication.** The sidebar's **Two-factor** button manages your own account's
TOTP. Every action asks for your password again (five wrong passwords in a row end the session
and send you to the sign-in screen). *Set up* shows the secret and the `otpauth://` link as
text (the dashboard draws no QR code, so it loads nothing from outside); *Turn on* takes the
password and a code and then shows ten recovery codes **once**, tells you your other sessions
were signed out, and asks you to confirm you stored them. *New recovery codes* and *Turn off*
are there while it is on. The server never replaces a secret in use, so *Change device* is
off, set up, on: the dialog warns that between the first and the last step the account signs
in with the password alone. A management key (`sk-mgmt-…`) never asks for a code.

**Project settings.** The project form has three more sections: *Extra request fields*
(allow and deny lists with a button that adds the fields that change cost or retention to the
deny list), *Where the project key may be used from* (CIDR and origin lists, with the same
notes as the key form) and *End-user limits* (requests and prompt tokens per minute, tokens
per day and month; `0` is unlimited, and a request without an end user is not held to them).
Like the tool lists they are sent only when changed. The project panel gains three cards:

- **Guardrails:** the rules (the table shows a rule's size, never its pattern or words), the
  stream window and the webhook settings, the server's warnings, and a note that retrieved RAG
  passages are not scanned. *Edit guardrails* opens a rule editor (type, action, stages, pattern
  or keywords) and warns that a streamed answer starts later by about the window and that a
  regex longer than the window can be missed. The guardrail webhook endpoint is registered,
  re-pointed, rotated and removed here.
- **Alerts & money budget:** alerts on or off, the monthly budget in dollars, and the alerts
  webhook endpoint. The money budget is labelled an estimate that only warns and never blocks a
  request. A webhook secret is shown once, when the endpoint is registered or rotated (a rotated
  secret's predecessor keeps signing for 24 hours); only editors see the endpoint, since its URL
  often carries a token.
- **Feedback:** positive share, counts, a per-day chart, the label breakdown and a table of
  the daily numbers for the last 7, 30 or 90 days. It counts scores only; the gateway never
  receives the request or answer.
