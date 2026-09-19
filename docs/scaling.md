# Running more than one replica

Ragmux keeps no durable state in the process. Everything that outlives a request
lives in PostgreSQL, so scaling out is a matter of pointing several gateway
processes at the same database, putting a plain round-robin proxy in front, and
setting `SECRET_KEY` so every replica reads the same credentials.

This page covers what that takes, what the gateway does differently with more
than one replica, and what is deliberately not replicated.

## What is already shared

| Concern | Where the state lives | Replica-safe |
|---|---|---|
| Sessions and API keys | `sessions`, `projects.api_key_hash` | yes - a cookie issued by one replica authenticates on any other |
| Rate limits and budgets (RPM, TPM, daily/monthly) | `project_usage`, reserved atomically per UTC window | yes - the counter is a row, not a process variable |
| Login throttling and lockouts | `login_attempts` | yes |
| Request logs, audit log, metrics | `request_logs`, `audit_logs` | yes |
| Provider credentials | `model_connections.api_key_enc`, AES-256-GCM | yes, **with a shared `SECRET_KEY`** (see below) |
| Documents, chunks, embeddings | `documents`, `chunks`, `chunk_embeddings_<dims>` | yes |
| Document ingestion queue | `documents.status` + claim columns | yes - see [Ingestion across replicas](#ingestion-across-replicas) |
| Retention job | leader-elected with a session advisory lock | yes - see [The retention leader lock](#the-retention-leader-lock) |
| Schema migrations | `schema_migrations` under an advisory lock | yes - replicas may start together |
| Streaming completions (SSE) | nothing; a per-request passthrough | yes - see [Streaming](#streaming) |
| First-run setup / `ADMIN_PASSWORD` bootstrap | `users`, guarded so exactly one wins | yes |

## What you must set

### SECRET_KEY

`SECRET_KEY` is the AES-256 key that encrypts provider credentials at rest. With
it unset, a replica generates one into `DATA_DIR/secret.key` - and with several
replicas, **each generates its own**, so the second one cannot read what the
first wrote. Set it before you scale:

```
SECRET_KEY=$(openssl rand -hex 32)
```

Since 0.4 a wrong or diverged key is a boot failure rather than a puzzle. On
start, the gateway seals a fixed value into the `instance_settings` table (the
first replica to reach an empty table wins; the others verify against it) and
reads it back before it touches a single credential. When it does not decrypt,
the start fails with the key's source named:

```
SECRET_KEY does not match the one this database was written with (key source: env):
the canary stored in instance_settings does not decrypt. Start with the original key,
or run "ragmux rotate-key" with it in the environment to move the database to a new one.
```

That catches the cases operators actually hit: a second replica falling back to
its own key file, a dump restored onto a host that has a different key, a
rotation applied to one node only, a wiped data volume. It costs nothing on a
single instance.

`ragmux rotate-key` re-seals the canary in the same transaction as the
credentials, so a rotation needs no extra step. See
[Rotating SECRET_KEY](backup-restore.md#rotating-secret_key).

### An external PostgreSQL

The default all-in-one layout (`docker-compose.yml`) bundles PostgreSQL in the
same container and **cannot** be scaled: a second replica would start a second
database on its own volume. Use `docker-compose.split.yml`, or any managed
PostgreSQL 14+ with the `vector` extension, and point every replica at it.

### Connection pooling

Each replica opens its own pool of `DB_MAX_CONNS` connections (default 10), so
the fleet needs `DB_MAX_CONNS × replicas` connections plus headroom for backups
and psql sessions, and the server's `max_connections` must cover that. Either
lower `DB_MAX_CONNS` per replica, or put a pooler in front.

If you use **PgBouncer**, two things matter:

- **Session mode** keeps everything working, including the retention leader
  lock. A session advisory lock belongs to a connection, and transaction mode
  hands that connection to somebody else between statements, so the lock cannot
  be held. Nothing breaks - every step of the retention pass is idempotent - but
  several replicas will each run a pass.
- **pgx v5 prepares statements** and caches them per connection. Under
  transaction mode a cached statement can be sent to a connection that never
  prepared it. Either use session mode, or disable the cache in the connection
  string:

  ```
  DATABASE_URL=postgres://user:pass@pgbouncer:6432/ragmux?default_query_exec_mode=exec&statement_cache_capacity=0
  ```

## Ingestion across replicas

Uploaded documents are not processed by the replica that received them. They are
a queue in the `documents` table, and every replica works it.

- **Claim.** One dispatcher goroutine per replica (not one per worker, so the
  database sees one poll per replica) takes the next claimable row with
  `FOR UPDATE SKIP LOCKED`: a document that is `pending`, or one that is
  `processing` but whose owner stopped renewing its lease. Skipping locked rows
  is what lets every replica poll at once without queueing behind each other.
- **Lease.** The claim writes `claimed_by`, `claimed_at` and `lease_until`.
  While the job runs, the worker pushes `lease_until` out every third of
  `INGEST_LEASE`. If a replica is killed, nothing renews the lease and the
  document becomes claimable again once it expires - at worst `INGEST_LEASE`
  after the crash.
- **Heartbeat.** When a renewal finds the claim gone (another replica took the
  document over), the job's context is cancelled, the worker stops, logs `lost
  ingestion lease; another replica took over`, and deliberately writes **no**
  status: the row belongs to its new owner now.
- **Attempt cap.** Each claim increments `attempts`, and a document at
  `INGEST_MAX_ATTEMPTS` is skipped by the claim query. Without that cap, a
  document that reliably takes the process down becomes a cluster-wide crash
  loop as replicas take turns on it. The retention job gives those rows their
  final `failed` status. A successful ingest and an explicit retry (upload,
  reprocess) both reset the counter, so a healthy document never accumulates.
- **Clean shutdown.** A replica that stops in an orderly way puts whatever it
  still owned straight back into the queue instead of leaving it to the lease,
  and gives the attempt back. A single-instance restart therefore picks its work
  up immediately, exactly as it did before the queue moved into the database.

**Honest about the failure mode:** a lease that expires while the replica is
alive but slow - a long GC pause, a database blip that ate the heartbeats - can
have one document processed twice. That costs embedding money, not data
integrity: `ReplaceDocumentChunks` is a single transactional delete-and-insert
with the status write inside it, so the last writer wins cleanly and no half
state is ever visible. The heartbeat, and the context it cancels the moment the
claim is gone, is what keeps it rare.

### Settings

| Variable | Default | What it trades |
|---|---|---|
| `INGEST_LEASE` | `2m` | How long a crashed replica's document stays untouchable. Lower means faster failover and more heartbeat traffic; it is renewed, not sized to the job (a job may run 15 min). |
| `INGEST_POLL_INTERVAL` | `5s` | How quickly a replica notices work queued by another replica. Uploads on the same replica kick the dispatcher immediately. |
| `INGEST_MAX_ATTEMPTS` | `5` | How many claims a document gets before the retention job fails it. |
| `MAX_PENDING_DOCUMENTS` | `1024` | The backlog an upload is still accepted into. |

### The 503 on upload changed meaning

`MAX_PENDING_DOCUMENTS` replaced the old in-process channel of 1024. A `503` on
`POST /admin/api/rag-stores/{id}/documents` used to mean *this replica's channel
is full* - a number that said nothing about the fleet, and that a second replica
would have happily accepted. It now means *every replica together is this far
behind*, counted as `SELECT count(*) FROM documents WHERE status='pending'` and
cached for about a second. That is genuine cross-replica backpressure and the
number an operator can act on.

## The retention leader lock

The retention job (request logs, audit entries, login attempts, expired
sessions, usage counters, exhausted documents) runs on every replica, an hour
apart. Before each pass a replica takes `pg_try_advisory_lock`; one gets it and
does the work, the others log at `debug` and return. The first pass is jittered
over an extra minute so a fleet restarted together does not wake up together.

Every step of the pass is an idempotent `DELETE ... WHERE created_at < cutoff`,
so **the lock is an optimisation against duplicated work, not a correctness
requirement**. That is also why a connection pooler in transaction mode - where
a session lock cannot be held - costs a duplicated pass and nothing else.

The alternative, a `leader_election` table holding a lease row, was rejected: it
needs schema, a renewal loop, and a window in which a dead leader's lease has
not expired yet. `pg_try_advisory_lock` needs no schema at all, releases the
moment the connection dies, and matches how the codebase already serialises
migrations.

The lock key is scoped to the current schema, so two Ragmux instances sharing
one database in separate schemas elect their own leader.

## Streaming

**Streaming needs no session affinity.** `POST /v1/chat/completions` with
`"stream": true` is a passthrough: the gateway opens the upstream request,
copies SSE chunks to the client as they arrive, and writes one `request_logs`
row when it is done. There is no stream id, no resume token, no server-side
buffer to reattach to - so there is nothing for a second request to be routed
back to. Configure your proxy for plain round robin; `ip_hash` or a sticky
cookie only costs you an unbalanced fleet.

What this means in practice:

- Two clients using the same project key can stream from two replicas at the
  same time. Each replica writes its own log row, and both count against the
  same shared RPM/TPM budget, because that budget is a database row.
- If the replica serving a stream dies, the client sees the connection drop.
  The replica records the interrupted request with `status_code = 499` (nginx's
  convention for a client that went away) and the upstream call is cancelled.
  There is no state to clean up. **The client retries, and any replica can
  serve the retry.** A retry is a new completion, not a resumption - the same
  as a dropped stream against any provider API.
- `internal/e2e/streaming_replica_test.go` is that behaviour as a test: two full
  router stacks over one database, concurrent streams on one key, a severed
  replica mid-stream, and the shared rate limit.

On the proxy, the only requirements are the ones a single instance already has:
do not buffer responses (`proxy_buffering off`) and keep the read timeout above
`STREAM_MAX_DURATION` (default `30m`). See
[Behind a reverse proxy](configuration.md#behind-a-reverse-proxy).

## Rolling restarts and shutdown

On `SIGINT`/`SIGTERM` a replica shuts down in this order:

1. Stop accepting HTTP and drain in-flight requests - **15 s**. A streaming
   completion still running at the end of that window is cut off and logged as
   `499`.
2. Wait for running ingestion jobs - **30 s**, then their context is cancelled.
3. Put every document this replica still owned back into the queue, and give
   the cancelled attempt back.
4. Stop the retention job (releasing the leader lock with its connection) and
   close the pool.

So the worst case for the fleet is: a replica is `SIGKILL`ed before step 3 runs,
and the documents it was working on wait out `INGEST_LEASE` (2 min by default)
before another replica claims them. Nothing is lost; a document is only ever
`ready` after one transactional write.

- Give the container time for steps 1 and 2: `stop_grace_period: 60s` in
  Compose, `terminationGracePeriodSeconds: 60` in Kubernetes. A shorter grace
  period turns every deploy into the lease-expiry case.
- Roll one replica at a time. Migrations are serialised by an advisory lock, so
  a new replica starting against a database an old one is still using is safe as
  long as the schema change is backwards compatible - which is the rule these
  migrations follow (`ADD COLUMN IF NOT EXISTS`, new tables, no drops).

## The Compose example

```bash
# .env needs SECRET_KEY, POSTGRES_PASSWORD and RAGMUX_DB_PASSWORD
docker compose -f docker-compose.split.yml -f docker-compose.scale.yml \
    up -d --scale ragmux=3
```

`docker-compose.scale.yml` is an overlay on the split layout. It removes the
`container_name` and the fixed `127.0.0.1:8765` host port (either of which alone
stops `--scale` from working), adds an nginx front end
(`docker/nginx/ragmux.conf`) that resolves the `ragmux` service name per request
so it picks up a scale change without a reload, and sets
`stop_grace_period: 60s`.

Check that all three replicas are working the queue:

```bash
docker compose -f docker-compose.split.yml -f docker-compose.scale.yml ps
docker compose -f docker-compose.split.yml -f docker-compose.scale.yml \
    logs ragmux | grep 'ingestion ready'
```

Each replica logs its own `owner` (host, pid and a random suffix), which is what
appears in `documents.claimed_by`.

## Kubernetes

- A plain `Deployment` with `replicas: N`; no `StatefulSet`, no volume. The only
  mounts a replica needs are its configuration.
- `SECRET_KEY` and `DATABASE_URL` from a `Secret`. `SECRET_KEY_FILE` and
  `DATABASE_URL_FILE` read a mounted file instead, if you prefer that.
- `terminationGracePeriodSeconds: 60` or more - see above.
- Readiness and liveness on `GET /healthz`, which pings the database. Give
  readiness a short period; the process is ready as soon as it listens.
- A `Service` of type `ClusterIP` and any ingress controller. **Do not set
  session affinity.** Turn response buffering off for the ingress
  (`nginx.ingress.kubernetes.io/proxy-buffering: "off"`) and raise
  `proxy-read-timeout` past `STREAM_MAX_DURATION`.
- Scale ingestion with `INGEST_WORKERS` per replica rather than with replicas
  alone: workers are cheap, replicas each cost a connection pool.
- A `PodDisruptionBudget` of `maxUnavailable: 1` keeps a node drain from
  emptying the fleet into the lease-expiry case at once.

## What is not replicated

None of these need to be, but it is worth knowing why:

- **The PDF worker subprocess.** PDFs are parsed by re-executing the gateway
  binary as `ragmux pdf-extract`, so a malformed file cannot take the server
  down with it. That subprocess is local to the replica running the ingestion
  job and lives for the length of one parse. It holds the document's claim
  through the parent, so nothing else touches the row meanwhile.
- **In-process caches.** Each replica keeps the set of
  `chunk_embeddings_<dims>` tables it has already seen, and an HTTP transport
  with idle upstream connections. Both are derived, not authoritative: the
  worst a cold replica does is one extra `CREATE TABLE IF NOT EXISTS` or one
  extra TLS handshake. Nothing invalidates across replicas, and nothing has to.
- **The one-second ingestion backlog count.** Each replica caches the pending
  count for about a second, so a burst of uploads arriving at different
  replicas within the same second can overshoot `MAX_PENDING_DOCUMENTS` by the
  size of the burst. The ceiling is a coarse guard against an unbounded queue,
  not a quota.
- **The `secret.key` fallback file.** Deliberately per-replica and deliberately
  unusable for a fleet - which is the whole point of the canary above.
