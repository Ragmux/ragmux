# Roadmap

This is a statement of direction, not a commitment: scope and order change as the work
teaches us things, and a release ships when it is ready rather than on a date.

Ragmux is pre-1.0. Until 1.0 the public API, configuration keys and metric names can still
change between minor versions, and each change is listed in the [CHANGELOG](CHANGELOG.md).
Version 1.0 is where they are frozen.

## v0.4.2 — fixes and hygiene

The next release. It fixes what hurts today, makes silent failures loud and corrects the
documentation. No schema change.

- Gemini 3 tool-calling turns keep their thought signature, and Gemini honours
  `response_format: json_schema`.
- Upstream `503` and `504` and the upstream's `Retry-After` reach the client, and Anthropic
  stop reasons map onto the values OpenAI clients expect.
- RAG stores refuse embedding models wider than 2000 dimensions instead of silently running
  without an index; the system-message handling of RAG context stops flattening content parts.
- One password rule, 12 characters at least, everywhere a password is set.
- Retention deletes in batches; OpenTelemetry GenAI span attributes, new metrics and a
  ready-made Prometheus alert rule file.
- Corrected reference docs, a contributor guide and this roadmap.

## v0.5 — shared request pipeline, routing and resilience

The foundation for everything that follows: every endpoint goes through one request
pipeline, so new endpoints and routing do not each re-implement authentication, limits,
logging and metrics.

- Routing: several connections per project, model aliases and weighted load balancing.
- Retry with backoff and ordered fallback between connections, and a circuit breaker per
  connection.
- New endpoints: `/v1/embeddings`, `/v1/retrieve` and `/v1/rerank`, with a separate
  connection for reranking.
- A request ID on every log row, accepted from `X-Request-Id` or generated.
- Provider presets for Mistral, xAI and Groq, and Azure OpenAI as a connection type.
- Integration guides for the Vercel AI SDK, n8n, Open WebUI and LibreChat.

## v0.6 — RAG quality

- Choose where retrieved context goes in the prompt, so a cached system prompt stays
  cacheable across turns.
- A retrieval policy that skips retrieval for greetings and tool turns, and a query strategy
  that condenses a conversation into one standalone question.
- Document metadata and filtering at query time.
- Content hashing, so the same upload is not embedded twice.
- `halfvec` for embedding models wider than 2000 dimensions.
- A search language per store, with better Turkish handling, and guides on running Ragmux
  on-premises for Turkish content.

## v0.7 — reasoning and agents

- A single `reasoning_effort` setting mapped onto each provider's thinking controls, and
  thinking blocks carried across turns.
- Structured output on Anthropic.
- Fields that are dropped in translation today — refusals, annotations, log probabilities —
  passed through.
- An MCP server that exposes each RAG store as a search tool.
- Per-project tool allow and deny lists, and session IDs in the request log.
- An Anthropic-compatible `/v1/messages` endpoint.

## v0.8 — security, cost visibility and feedback

- Guardrails before the request, before retrieval and on the response, including PII
  detection with Turkish national ID and IBAN validators, and webhook guardrails.
- Two-factor login with TOTP.
- Tags and end-user identifiers on requests, with per-end-user limits.
- Signed webhooks when a project nears or passes a limit.
- A feedback endpoint tied to the request ID.
- Key management through Vault or a cloud KMS, per-key IP and origin allowlists, and
  anonymisation of a user's personal data that keeps their usage history.

## v0.9 — ingest, evaluation and scale

- More input formats (CSV, XLSX, PPTX), token-based chunking, and re-embedding only the
  chunks that changed.
- Re-embedding into a second index and switching over without downtime.
- Monthly partitioning of the request log.
- A retrieval evaluation module: recall, MRR and nDCG over saved question sets, to compare
  store settings before changing them.
- Multi-query and HyDE retrieval, and retrieval the model triggers itself as a tool.
- The data-source connector framework and a first connector (URLs and sitemaps);
  S3-compatible buckets and Git repositories follow in later releases. Bedrock and Vertex AI
  arrive as providers.

## v1.0 — a stable contract

1.0 is a contract release: the public API, configuration keys and metric names are frozen
and deprecations follow a published policy.

- Several RAG stores per project, with scores merged across them.
- Document-level access control (the design is still to be settled).
- A `/v1/responses` endpoint, starting with the stateless subset.
- OpenAPI 3.1 for the gateway and admin APIs, checked in CI against the handlers.
- A Helm chart for the gateway-only image, and declarative `apply` and `export` commands
  with a management CLI.
- A public read-only demo and a reproducible benchmark script.

## Not scheduled

These have no release because they need a decision first, and some may stay out of scope.
Reasons to want one of them are welcome in an issue.

- OCR for scanned PDFs, SSO and OIDC login, budgets denominated in money, and an import
  tool for 0.1 databases.
- Object storage for uploaded files, optional prompt and response logging, response
  caching, and an organisation and team hierarchy.
- Batch, audio, image and realtime endpoints.

## Suggesting a change

Open an issue before writing code; see [CONTRIBUTING.md](CONTRIBUTING.md).
