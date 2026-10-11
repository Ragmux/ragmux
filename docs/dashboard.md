# Dashboard

`/admin/` serves a single-page dashboard (vanilla JavaScript, no build step, embedded in
the binary) over the same REST API. On a fresh database it shows the first-run setup
form; afterwards a sign-in screen that reports remaining attempts and lockouts. Screens:

- **Overview:** 24h / 7d / 30d window with deltas against the previous period, requests
  per day, traffic by project, budget watch with exhaustion forecasts, recent requests
  and a CSV export.
- **Models:** connection cards with provider capabilities, last test result and private
  upstream flag; the form can test a connection before saving it, start from a preset and
  takes an Azure OpenAI endpoint, deployment and API version.
- **RAG stores:** quota meters, drag-and-drop upload, ingestion progress, per-document
  reprocess and a retrieval tester (hybrid / vector, rerank, per-stage latencies); the rerank
  connection list follows the chosen backend.
- **Projects:** usage rings for the minute, day and month windows with reset countdowns,
  budget forecast banner, members, key rotation, a route editor (kind, up to eight targets
  with model override, weight and priority) and a project CSV export.
- **API keys:** mint and revoke your own `sk-user-…` and `sk-mgmt-…` keys with a scope
  checklist, grants and limits.
- **Playground:** real `/v1/chat/completions` calls with a project key, streaming or JSON,
  optional extra system prompt, RAG sources as citations and the response headers.
- **Users** and **Audit log** (admins): role and failed-login tiles, filters, NDJSON export.

What a user sees follows their role: viewers get read-only screens, editors the forms,
admins the user and audit screens. The page ships its own fonts (Bricolage Grotesque,
IBM Plex Sans, JetBrains Mono, all SIL OFL, `web/fonts/`) and runs under a strict
`Content-Security-Policy`, so it makes no third-party requests.

