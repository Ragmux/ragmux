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
  [Context injection](rag.md#context-injection).
- **API keys:** mint and revoke your own `sk-user-…` and `sk-mgmt-…` keys with a scope
  checklist, grants and limits.
- **Playground:** real `/v1/chat/completions` calls with a project key, streaming or JSON,
  optional extra system prompt, RAG sources as citations and the response headers.
- **Users** and **Audit log** (admins): role and failed-login tiles, filters, NDJSON export.

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

