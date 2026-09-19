# Observability

Ragmux exports Prometheus metrics at `/metrics` and OpenTelemetry spans over OTLP/HTTP.
Both are off by default, both are implemented in the standard library
(`internal/metrics/`, `internal/tracing/`), and both are deliberately small: the gateway
takes no Go module dependency for either.

- [Metrics](#metrics)
  - [Turning it on](#turning-it-on)
  - [Why the endpoint is authenticated](#why-the-endpoint-is-authenticated)
  - [Scrape configuration](#scrape-configuration)
  - [The metric set](#the-metric-set)
  - [Cardinality checklist](#cardinality-checklist)
- [`/readyz` and `/healthz`](#readyz-and-healthz)
- [Tracing](#tracing)
  - [Turning tracing on](#turning-tracing-on)
  - [The seven spans](#the-seven-spans)
  - [Sampling](#sampling)
  - [The trust gate](#the-trust-gate)
  - [No span carries content](#no-span-carries-content)
  - [What a hand-rolled exporter does not get](#what-a-hand-rolled-exporter-does-not-get)
  - [Running a Collector](#running-a-collector)

## Metrics

### Turning it on

| Variable | Default | Meaning |
|---|---|---|
| `METRICS_ENABLED` | `false` | Serve `/metrics` at all. Off, the route does not exist and returns `404`. |
| `METRICS_TOKEN` / `METRICS_TOKEN_FILE` | *(none)* | Bearer token every scrape must present. Required unless `METRICS_LISTEN` binds a loopback address. |
| `METRICS_LISTEN` | *(none)* | `host:port`. When set, `/metrics` moves to a second HTTP server and leaves the main router entirely. |
| `METRICS_MAX_SERIES` | `5000` | Ceiling on label combinations; beyond it new ones are dropped and counted. |

```bash
METRICS_ENABLED=true
METRICS_TOKEN=$(openssl rand -hex 24)
```

or, on a host where Prometheus runs alongside the container:

```bash
METRICS_ENABLED=true
METRICS_LISTEN=127.0.0.1:9090
```

`METRICS_ENABLED=true` with neither a token nor a loopback `METRICS_LISTEN` stops the
start with

```
config: METRICS_ENABLED without METRICS_TOKEN would publish per-project usage and spend
unauthenticated; set METRICS_TOKEN or bind METRICS_LISTEN to a loopback address
```

### Why the endpoint is authenticated

The metric set names **every project and every model** in the installation and reports
**per-project token counts and spend**. That is the same data
`/admin/api/projects/{id}/metrics` serves, and that endpoint requires a session. An
unauthenticated `/metrics` would be the one place the product gives it away for free:
anyone who can reach the port learns what your customers are called, which models they
use, how much they send and what it costs you.

So the endpoint is authenticated, and there are exactly two ways to satisfy that:

- **A bearer token.** `Authorization: Bearer <METRICS_TOKEN>`, compared in constant time
  so a wrong token cannot be recovered a byte at a time. Responses carry
  `Cache-Control: no-store`.
- **A loopback bind.** With `METRICS_LISTEN=127.0.0.1:9090` the endpoint is only
  reachable from the machine itself, and a token adds nothing.

`METRICS_LISTEN` does more than move the port. While it is set, `/metrics` is **never
registered on the main router**, so a reverse proxy configured to forward everything, a
path-prefix mistake, or a future route that shadows a `deny` rule cannot expose it.
There is no route to reach.

The endpoint is also excluded from the JSON request log, the way the dashboard's static
assets are: a fifteen-second scrape interval would otherwise be four log lines a minute
about nothing, and the scrape is already counted in the metrics it fetches.

### Scrape configuration

```yaml
# prometheus.yml
scrape_configs:
  - job_name: ragmux
    scrape_interval: 15s
    metrics_path: /metrics
    static_configs:
      - targets: ["ragmux:8765"]
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/ragmux-metrics-token
```

With `METRICS_LISTEN=127.0.0.1:9090`, point the target at `127.0.0.1:9090` and drop the
`authorization` block.

### The metric set

Every name is prefixed `ragmux_`. Durations are seconds, as Prometheus expects.

**HTTP.** Recorded by a middleware installed straight after the request-id middleware, so
it measures everything below it — including a request that panics.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ragmux_http_requests_total` | counter | `route`, `method`, `status` | Requests by chi route pattern. |
| `ragmux_http_request_duration_seconds` | histogram | `route`, `method` | Buckets `.005 .01 .025 .05 .1 .25 .5 1 2.5 5 10 30 60` — a chat completion legitimately takes 30 s, and a histogram that stopped at 10 would put every real completion in `+Inf`. |
| `ragmux_http_requests_in_flight` | gauge | — | Requests currently being served. |
| `ragmux_http_response_bytes_total` | counter | `route` | Response body bytes written. |

**Gateway.** Recorded at the single deferred chokepoint of a finished chat completion,
which the success, error, mid-stream-failure and client-disconnect paths all reach.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ragmux_gateway_requests_total` | counter | `project`, `model`, `status`, `streamed` | Chat completions. |
| `ragmux_gateway_request_duration_seconds` | histogram | `project`, `model`, `streamed` | End to end through the gateway handler. |
| `ragmux_gateway_upstream_duration_seconds` | histogram | `provider`, `model` | Time inside the provider call. For a stream it covers the whole stream. |
| `ragmux_gateway_time_to_first_token_seconds` | histogram | `provider`, `model` | Buckets `.05 .1 .25 .5 1 2 5 10 30`. Streams only. |
| `ragmux_gateway_stream_chunks_total` | counter | `provider` | Chunks relayed to clients. |
| `ragmux_gateway_streams_active` | gauge | — | Streams in flight. |
| `ragmux_gateway_tokens_total` | counter | `project`, `model`, `kind` | `kind` is `prompt` or `completion`. |
| `ragmux_gateway_tokens_estimated_total` | counter | `project`, `model` | Requests whose token counts are Ragmux's estimate because the provider reported none. |
| `ragmux_gateway_cost_usd_total` | counter (float) | `project`, `model` | Estimated spend, from the same price table the request log uses. |
| `ragmux_gateway_errors_total` | counter | `project`, `provider`, `type` | `type` is the provider error type, a bounded set. |
| `ragmux_gateway_client_disconnects_total` | counter | — | Completions the client abandoned (status 499). |

`tokens_estimated_total` is a separate metric rather than a `estimated="true"` label on
`tokens_total` on purpose: a token series that silently mixes estimated and reported
counts is a trap for anyone doing cost maths. Divide the estimated request count by
`ragmux_gateway_requests_total` to see how much of a model's spend figure is guesswork.

**Limits.**

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ragmux_limits_denied_total` | counter | `project`, `reason` | `reason` is `rate_limit_rpm`, `rate_limit_tpm`, `budget_daily` or `budget_monthly`. |

The denied request itself is still counted by `ragmux_http_requests_total` with
`status="429"`.

**Retrieval.**

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ragmux_rag_retrieval_duration_seconds` | histogram | `store`, `backend`, `mode` | Query embedding plus the database search. Buckets `.005 .01 .025 .05 .1 .25 .5 1 2.5 5`. |
| `ragmux_rag_embed_duration_seconds` | histogram | `provider` | The query embedding call alone. |
| `ragmux_rag_rerank_duration_seconds` | histogram | `backend` | Reranking, timed even when the reranker was skipped. |
| `ragmux_rag_rerank_failures_total` | counter | `backend`, `reason` | `reason` is one of `timeout`, `upstream`, `parse`, `unavailable` and nothing else. |
| `ragmux_rag_searches_total` | counter | `store`, `used` | `used` is the backend that **actually answered**. |
| `ragmux_rag_hits_total` | counter | `store` | Passages returned. |
| `ragmux_search_backend_available` | gauge | `backend` | `1` when this server can answer with that backend. |

`used` differs from the store's configured `search_backend` when the configured one is
not available on this server and the search fell back to pgvector. Comparing the two is
how a silent fallback becomes visible:

```promql
sum by (store) (rate(ragmux_rag_searches_total{used="pgvector"}[5m]))
  and on() ragmux_search_backend_available{backend="pg_search"} == 0
```

**Ingestion.**

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `ragmux_ingest_jobs_total` | counter | `outcome` | `ready`, `failed`, `cancelled` or `lease_lost`. |
| `ragmux_ingest_duration_seconds` | histogram | — | One document's parse, chunk, embed and store cycle. Buckets `1 5 15 30 60 120 300 600 900`. |
| `ragmux_ingest_chunks_total` | counter | — | Chunks written. |
| `ragmux_ingest_claims_total` | counter | `outcome` | `claimed`, `empty` or `error`. |
| `ragmux_ingest_lease_lost_total` | counter | — | Jobs another replica took over. |
| `ragmux_documents` | gauge | `status` | Documents by status; `pending` is the cluster's ingestion backlog. |

`ragmux_documents` is a `SELECT status, count(*) FROM documents GROUP BY status` with its
own three-second timeout, **cached for 30 seconds**. Backlog depth is a database property
now that the queue lives there, and without the cache a scrape storm — several Prometheus
replicas, or a dashboard refreshing — would become a query storm against the same table
ingestion is writing to. A scrape that cannot reach the database serves the previous
values rather than zero: an alert would read a zero as "the backlog drained".

**Database pool.** All read from `pgxpool.Stat()`, which costs no query.

| Metric | Type | Meaning |
|---|---|---|
| `ragmux_db_pool_total_conns` | gauge | Connections the pool holds. |
| `ragmux_db_pool_idle_conns` | gauge | Idle connections. |
| `ragmux_db_pool_acquired_conns` | gauge | Connections checked out. |
| `ragmux_db_pool_max_conns` | gauge | `DB_MAX_CONNS`. |
| `ragmux_db_pool_acquires_total` | gauge | Successful acquires since start. |
| `ragmux_db_pool_empty_acquires_total` | gauge | Acquires that waited for a free connection; a rising rate means `DB_MAX_CONNS` is the bottleneck. |

**Self.**

| Metric | Type | Meaning |
|---|---|---|
| `ragmux_build_info` | gauge | Always `1`; `version` and `go_version` in the labels. |
| `ragmux_process_start_time_seconds` | gauge | Process start, Unix seconds. |
| `ragmux_go_goroutines`, `ragmux_go_memstats_heap_inuse_bytes`, `ragmux_go_memstats_alloc_bytes_total`, `ragmux_go_gc_cycles_total` | gauge | Read through `runtime/metrics`, which does not stop the world. |
| `ragmux_metrics_series_dropped_total` | counter | Label combinations refused at `METRICS_MAX_SERIES`. Drops are also logged at `error`, at most once a minute. Alert on `increase(...[1h]) > 0`: any value above zero means metrics are being lost. |
| `ragmux_tracing_spans_dropped_total` | gauge | Spans discarded because the export queue was full. |
| `ragmux_tracing_export_failures_total` | gauge | Export attempts abandoned after their one retry. |

The two tracing counters are monotonic but are exported with `# TYPE gauge`: the registry
has no counter-valued function collector. `rate()` over them behaves the same way.

### Cardinality checklist

The metric set is designed so that the number of series grows with the **size of the
installation** — projects, models, RAG stores, routes — and never with traffic. Every
label added to this repository has to pass this list:

- [ ] **Route labels come from the chi pattern**, read *after* `next.ServeHTTP` returned
      (chi fills it during routing), never from `r.URL.Path`. That single decision is the
      primary guard: `/admin/api/rag-stores/{id}/documents` is one series, the path would
      be one per document id. A request no route matched is labelled `unmatched`, so a
      404 sweep for `/wp-admin/...` collapses into one series. Dashboard static assets
      collapse into `/admin/*`.
- [ ] **Model labels come from the model connection** (`conn.ModelName`), resolved
      server-side. Never the model string the client sent: it is attacker-controlled and
      an unbounded label value is a memory bug, not a cosmetic one.
- [ ] **Project and store labels are numeric ids.** Not names — a name can be renamed,
      which orphans a series, and a name is user-supplied text.
- [ ] **Failure reasons are constants from a closed set.** `rerank_failures_total` takes
      `timeout|upstream|parse|unavailable`, never `err.Error()`, which quotes upstream
      bodies and model replies. `errors_total` takes `provider.Error.Type`.
      `limits_denied_total` takes the `limits.Reason*` constants.
- [ ] **Never** a label taken from a header, an error message, a filename, a query
      string, a user agent or an IP address.

The registry enforces a backstop regardless: at `METRICS_MAX_SERIES` new combinations are
refused, counted in `ragmux_metrics_series_dropped_total` and logged at **`error`** (at
most once a minute, so a hot loop of refusals cannot become the log itself).

**Treat a non-zero drop counter as an incident, not a note.** The cap has no eviction: once
it is full, every *new* legitimate series is refused from then on — a project created
today, a model added today, that project's `ragmux_gateway_cost_usd_total` series. Existing
series keep updating, so nothing looks broken; the new ones simply never appear. Hitting
the cap almost always means a label went unbounded, so look for the cause first and raise
`METRICS_MAX_SERIES` only once you know the cardinality is legitimate.

Eviction was considered and rejected. Dropping the least recently used series would keep
legitimate metrics from freezing, but it costs a write on every observation — a lock or an
atomic store on the scrape path — and an evicted counter that comes back starts at zero,
which Prometheus reads as a counter reset and turns into a silently wrong `rate()`. Data
that is visibly missing is preferred to data that is quietly wrong.

## `/readyz` and `/healthz`

Two probes with two different jobs. Neither needs authentication and neither reports
anything about projects or usage.

| | `/healthz` | `/readyz` |
|---|---|---|
| Question | Is this process alive? | Should this replica receive traffic? |
| Checks | The pool answers a ping | The pool answers **and** `MAX(version)` in `schema_migrations` has reached the head migration this binary embeds |
| Used by | The container `HEALTHCHECK`, `ragmux -healthcheck` | A load balancer or Kubernetes readiness probe |
| Unchanged in 0.4 | yes | new |

`/healthz` is deliberately untouched, so every existing probe and the image's
`HEALTHCHECK` keep their meaning.

`/readyz` answers `200`:

```json
{"status":"ok","version":"0.4.0","migrations":24,"expected_migrations":24}
```

and `503` with `"status":"migrating"` while the applied version is **behind** the binary's:
the tables this build expects do not exist yet, so the replica cannot serve.

**A schema ahead of the binary is not a readiness failure.** The comparison is deliberately
one-directional. In a `maxSurge` rolling upgrade the first new pod migrates the shared
database; from that moment every old replica — all of them still carrying traffic — sees a
schema newer than its own. Failing readiness there would take the entire fleet out of
rotation in the middle of a deploy that is supposed to be seamless, and a rollback to the
previous image could never become ready at all. Ragmux migrations are additive
(`ADD COLUMN IF NOT EXISTS`, new tables, no drops), so the older code keeps working against
the newer schema. It answers `200` and says so:

```json
{
  "status": "ok",
  "version": "0.4.0",
  "migrations": 25,
  "expected_migrations": 24,
  "degraded": ["the database schema is at migration 25, ahead of the 24 this binary embeds; this replica is running older code against a newer schema"]
}
```

Because `/readyz` returns `200`, nothing alerts on this by itself — that is the operator's
to wire up. It is expected during a deploy and unexpected afterwards, so alert on the
`degraded` entry persisting rather than on its appearance. **This rests on migrations
staying additive**; a migration that drops a column or narrows a type would break old
replicas that this decision keeps in rotation, and would have to be handled separately
(see [Rolling restarts](scaling.md#rolling-restarts-and-shutdown)).

**A degraded search backend is not a readiness failure either.** A RAG store configured for
`pg_search` on a server without the extension keeps answering — the search falls back to
pgvector — so taking the replica out of rotation would turn a degraded-but-working
condition into an outage. It is reported instead as an informational field alongside the
`200`:

```json
{
  "status": "ok",
  "version": "0.4.0",
  "migrations": 24,
  "expected_migrations": 24,
  "degraded": ["pg_search is not installed on this server; 2 rag store(s) configured for it fall back to pgvector"]
}
```

Alert on the `degraded` field or on `ragmux_search_backend_available`; do not gate
traffic on either.

## Tracing

### Turning tracing on

| Variable | Default | Meaning |
|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(none)* | Collector base URL, e.g. `http://otel-collector:4318`. `/v1/traces` is appended unless already present. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | *(none)* | Traces-signal URL; wins over the base URL. |
| `TRACING_ENABLED` | *(endpoint set)* | `true` when an endpoint is configured, `false` otherwise. An explicit `false` always wins. |
| `TRACING_TRUST_INCOMING` | `false` | Continue a trace the client started. |
| `OTEL_EXPORTER_OTLP_HEADERS` | *(none)* | `k=v,k2=v2` sent with every export. |
| `OTEL_SERVICE_NAME` | `ragmux` | `service.name`. |
| `OTEL_RESOURCE_ATTRIBUTES` | *(none)* | Extra resource attributes, same form. |
| `OTEL_TRACES_SAMPLER_ARG` | `0.05` | Head sampling probability, `0` to `1`. |

Configuring an endpoint is what turns tracing on; there is no separate switch to
remember. `TRACING_ENABLED=false` is the override for an environment where a sidecar
injects the `OTEL_*` variables and you want this one process quiet.

While tracing is off, a span site costs one atomic load and one branch, and allocates
nothing.

### The seven spans

There are exactly seven span sites and no more. They are hand-written, so each one is a
decision rather than a by-product.

| Span | Kind | Where | Attributes |
|---|---|---|---|
| `http.server` | Server | middleware after the request-id middleware | `http.request.method`, `http.route` (the chi pattern, read after the handler ran), `http.response.status_code`, `ragmux.request_id` |
| `gateway.chat_completion` | Internal | the `/v1/chat/completions` handler | `ragmux.project.id`, `gen_ai.request.model`, `ragmux.stream`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens` |
| `rag.retrieve` | Internal | `Retriever.SearchWith` | `ragmux.rag.store_id`, `.mode`, `.backend`, `.top_k`, `.hits` |
| `rag.embed_query` | Client | around the query embedding call | `gen_ai.system` |
| `rag.rerank` | Client | around the rerank call | `ragmux.rerank.backend`, `ragmux.rerank.fallback` |
| `provider.chat` / `provider.embed` / `provider.rerank` | Client | `internal/provider/http.go`, `doRequest` / `doStream` | `server.address` (host only), `gen_ai.system`, `http.response.status_code`, `ragmux.stream.bytes` |
| `ingest.document` | Internal | the ingestion worker; its own trace root | `ragmux.document.id`, `.store_id`, `.chunks`, `.attempt` |

Two details worth knowing:

- **A streaming `provider.chat` span ends when the response body closes**, not when the
  headers arrive, so it covers the whole stream. A span that stopped at the header phase
  would report a forty-second completion as a two-hundred-millisecond call — precisely
  the number you opened the trace to find.
- **`ingest.document` is a trace root with no inbound parent.** A document reprocessed
  from the dashboard would otherwise hang a job that may run for minutes off the HTTP
  request that only queued it, and that request's span closes in milliseconds.

`internal/provider/http.go` is also the single place `traceparent` (and `tracestate`) is
written outbound, since every chat, embed, stream and rerank call funnels through
`doRequest`. Propagation is one code path rather than one per adapter.

### Sampling

Head-based, at `OTEL_TRACES_SAMPLER_ARG` (default `0.05`). The decision is made once when
a trace starts and inherited by every span beneath it, so a sampled request is complete
or absent, never half there. One request in twenty is enough to see the latency structure
of the gateway without paying to store every chat completion.

An unsampled trace still propagates its context downstream, so a service you call can
sample its own side into the same trace id.

### The trust gate

`TRACING_TRUST_INCOMING` is `false` by default, which means an inbound `traceparent`
header is **ignored** and every request starts a new trace here.

That is a deliberate refusal of a convenience. The gateway is reachable by anyone holding
an API key, and a client that can set `traceparent` can pin every request it makes into a
single trace id and set the sampled flag on all of it. That is a cheap way to flood the
collector from outside and to make one trace unreadable. Turn the gate on only when the
callers are your own services behind your own boundary — which is exactly the case where
a continued trace is worth something.

The `tracestate` header is forwarded verbatim but bounded at 512 bytes: it goes out to
every upstream, and an unbounded client-supplied value does not belong there.

### No span carries content

**One rule: no attribute value ever carries message content, the retrieval query, passage
text, a filename, the system prompt, or a credential.** A span is exported to a
third-party collector; it is metadata, not payload.

Two consequences that are easy to get wrong and are handled in the code:

- An upstream failure is recorded on a span by its **bounded error type**, not its
  message. A provider that quotes the offending request back inside a `400` would
  otherwise export prompt text to the collector. The full message still reaches the
  client and the request log, which stay inside your own boundary.
- `rag.retrieve` says which store was searched, how, and how many passages came back. It
  never says what was searched for.

`internal/e2e` enforces this with a test that pushes five distinct markers through the
stack — message content, passage text, a filename, the system prompt and a provider
credential — flushes the exporter into a stand-in collector, and greps the captured OTLP
payload for each of them. The same test asserts that all eight span names did arrive, so
it cannot pass by exporting nothing.

### What a hand-rolled exporter does not get

`internal/tracing` is about three hundred lines of standard library. That buys a
dependency-free binary and costs the following, all of which you would get from the
OpenTelemetry Go SDK:

- **No auto-instrumentation.** Only the seven spans above exist. pgx queries, chi
  routing, DNS resolution and TLS handshakes produce no spans at all, so a slow query
  shows up as unexplained time inside its parent span rather than as a span of its own.
  This is the largest single difference and the one to remember when reading a trace.
- **No metrics or logs signal.** OTLP carries three signals; this exports one. Metrics go
  to Prometheus at `/metrics`, logs to stdout as JSON.
- **No OTLP/gRPC.** HTTP only, port 4318, not 4317.
- **No compression.** Payloads are uncompressed JSON. At a 5 % sample rate on a gateway
  this is small; at 100 % on a busy one it is not.
- **No span links, events or baggage.** Attributes and a status, nothing else.
- **No resource auto-detection.** Nothing is inferred about the host, container,
  Kubernetes pod or cloud region. Set what you need in `OTEL_RESOURCE_ATTRIBUTES`, or let
  the Collector's `resourcedetection` processor add it.
- **No tail sampling, no retry queue.** One retry after a second, then the batch is
  dropped and counted in `ragmux_tracing_export_failures_total`. A backlog of retries
  would compete with the requests that produced it.

The exporter speaks **OTLP/HTTP with the JSON encoding**, which is what an OpenTelemetry
Collector accepts on port 4318 with no extra configuration. Vendor-direct endpoints vary
and several are protobuf-only, so pointing Ragmux straight at a vendor may or may not
work depending on the vendor. **The supported configuration is to point Ragmux at a
Collector** and let the Collector talk to your backend — which is also where you add
compression, retries, tail sampling and resource detection, all of the things in the list
above.

### Running a Collector

`docker-compose.otel.yml` and `otel-collector.yaml` in the repository root are a
copy-paste starting point:

```bash
docker compose -f docker-compose.yml -f docker-compose.otel.yml up -d
```

The overlay adds an `otel-collector` service on the gateway's network and sets
`OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318` on the `ragmux` service. Out of
the box the Collector logs the spans it receives, which is enough to confirm the wiring;
`otel-collector.yaml` has commented exporters for Jaeger, Tempo and an OTLP vendor
endpoint to replace the debug one with.

The Collector address is operator-configured, like `DATABASE_URL`, so the outbound SSRF
guard that governs provider calls does not apply to it: `http://otel-collector:4318` is a
private address on purpose and does not need `ALLOW_PRIVATE_UPSTREAMS`.
