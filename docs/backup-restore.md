# Backup and restore

Ragmux keeps all of its state in one PostgreSQL database plus one secret. Back up
both, and you can rebuild a gateway on any machine. This guide covers what to back
up, the bundled scripts, scheduled backups, physical backups, the restore runbook
and how to rehearse a disaster.

## What is state

| Where | What |
|---|---|
| PostgreSQL (`ragmux-data` volume, `/data/pg`, with the default all-in-one file; `pgdata` volume with `docker-compose.split.yml`) | users and sessions, model connections (provider API keys, AES-256-GCM encrypted), RAG stores, documents (the uploaded files as `bytea`), chunks, one `chunk_embeddings_<dims>` table per embedding width (HNSW vector indexes), projects and their hashed `sk-proj-…` keys, request logs, usage counters, login attempts, audit log, `schema_migrations` |
| `SECRET_KEY` | the 32-byte key (64 hex characters) that decrypts the provider API keys stored in the database. The database records which key that is (`instance_settings`), and a restore with the wrong one fails at start rather than silently |
| `/data/ragmux/secret.key` (all-in-one, only when `SECRET_KEY` is unset) | the generated fallback key; it lives in the same volume as the database, so a volume backup covers it, a `pg_dump` does not |

With the split layout the gateway container itself is stateless: it can be deleted and
recreated at any time. With the all-in-one layout the container is disposable too, as
long as the `ragmux-data` volume stays.

**Since 0.4 a wrong key stops the start instead of every provider call.** The database
holds a canary in `instance_settings` that only the key it was written with can open, so
a dump restored onto a host with a different `SECRET_KEY` fails at boot with
`SECRET_KEY does not match the one this database was written with (key source: …)`
rather than looking healthy until the first chat request. Put the matching key back — see
[SECRET_KEY](scaling.md#secret-key).

**Without the key at all**, the fastest way back is a database whose canary you own:
restore the dump, drop the canary row (`DELETE FROM instance_settings WHERE key =
'secret_key_canary'`) so the next start seals a new one, and then re-enter every provider
API key in the dashboard (Models -> Edit) — users, projects, documents and vectors all
come back, only the encrypted credentials are lost. Keep the key in a
secret manager (or your deployment's env store) *and* in the backup bundle, encrypted
at rest or in a separate location from the dumps. `scripts/backup.sh` can write it next
to the dump with `INCLUDE_SECRET_KEY=1` (see below).

`GET /admin/api/system` reports what a backup will contain, with sample values —
`migrations_version` and `version` are whatever the running build reports:

```json
{
  "backup": {"tables": 1, "documents_bytes": 1048576, "last_migration_at": "2026-09-18T12:34:41Z"},
  "database": {"postgres_version": "17.11", "pgvector_version": "0.8.6", "migrations_version": N, "size_bytes": 8787635},
  "secret_key_source": "env",
  "version": "0.4.0"
}
```

`backup.tables` is the number of `chunk_embeddings_<dims>` tables, `documents_bytes` the
sum of the stored document contents; the same numbers are shown on the dashboard
overview. `ragmux -version` prints the binary version.

## Logical backups with pg_dump

The recommended format is `pg_dump -Fc` (custom format): compressed, verifiable with
`pg_restore --list`, and restorable selectively. By hand, with the default all-in-one
container (Postgres is reached over the container-local unix socket as `postgres`):

```bash
docker compose exec -T -u postgres ragmux pg_dump -Fc --no-owner --no-privileges -d ragmux \
  > backups/ragmux-$(date +%Y%m%d-%H%M%S).dump
```

and with the split stack:

```bash
docker compose -f docker-compose.split.yml exec -T postgres \
  pg_dump -Fc --no-owner --no-privileges -U ragmux -d ragmux \
  > backups/ragmux-$(date +%Y%m%d-%H%M%S).dump
```

Notes:

- The dynamically created `chunk_embeddings_<dims>` tables are ordinary tables and are
  included automatically; so are their HNSW indexes (rebuilt on restore, which is the
  slowest part of a large restore).
- The dump contains `CREATE EXTENSION vector`. The target server must have pgvector
  available (`pgvector/pgvector:pg17` does; on managed Postgres enable it first).
- `pg_dump` runs in a consistent snapshot, so you can take it while the gateway is
  running. Documents that are being ingested at that moment are restored in their
  `pending`/`processing` state; the gateway claims them again after a restart, once any
  lease recorded in the dump has expired.
- Use the same major version of `pg_dump`/`pg_restore` as the server (17). Running the
  tools *inside* the container that holds Postgres (`ragmux` in the all-in-one layout,
  `postgres` in the split one) guarantees that.

## Scripts

Both scripts live in `scripts/`, run with `bash`, print `-h` help, exit `0` on success,
`1` when the dump/restore itself fails and `2` for usage or configuration errors. They
never print `DATABASE_URL`, passwords or `SECRET_KEY`, run with `umask 077` so every file
they create is private to the invoking user, and never put a credential on a command
line (`restore.sh` sends the login body through stdin and the bearer token through a
private header file, so neither shows up in `ps` or shell history).

### Where they connect

1. If `DATABASE_URL` is set **and** `pg_dump`/`pg_restore` are on `PATH`, the scripts use
   the local tools against that URL (external or managed Postgres, or the binary
   without Compose).
2. Otherwise they use `docker compose`. Run them from the directory holding
   `docker-compose.yml`, or set `COMPOSE_FILE` (`COMPOSE_FILE=docker-compose.split.yml`
   for the split stack). The Compose project is chosen the usual way: `-p NAME`,
   `COMPOSE_PROJECT` or the standard `COMPOSE_PROJECT_NAME` environment variable, which
   `docker compose` honours by itself. The layout is detected from the project
   (`LAYOUT=auto`): when it has a `<POSTGRES_SERVICE>` service (`postgres`) the scripts
   run `docker compose exec -T postgres …` as before (*split*); otherwise they treat the
   `<RAGMUX_SERVICE>` container (`ragmux`) as the all-in-one image and run the tools
   inside it as the `postgres` user over the unix socket (*aio*). `LAYOUT=split|aio`
   overrides the detection.

### `scripts/backup.sh`

```bash
scripts/backup.sh                       # -> ./backups/ragmux-YYYYmmdd-HHMMSS.dump
BACKUP_DIR=/mnt/backups KEEP_DAYS=30 INCLUDE_SECRET_KEY=1 SECRET_KEY_DIR=/mnt/keys scripts/backup.sh
make backup                             # same as scripts/backup.sh
```

| Variable | Default | Meaning |
|---|---|---|
| `BACKUP_DIR` | `./backups` | output directory (created with mode `700` if missing) |
| `KEEP_DAYS` | `14` | delete `ragmux-*.dump` / `.key` files older than N days; `0` keeps everything |
| `LAYOUT` | `auto` | `split` or `aio` to skip the detection described above |
| `POSTGRES_SERVICE`, `RAGMUX_SERVICE` | `postgres`, `ragmux` | Compose service that runs Postgres (split) / the all-in-one container |
| `POSTGRES_DB` | `ragmux` | database |
| `POSTGRES_USER` | `ragmux` (split), `postgres` (aio) | superuser the dump runs as |
| `DATABASE_URL` | unset | use local `pg_dump` against this URL instead of Compose |
| `INCLUDE_SECRET_KEY` | `0` | `1` writes `SECRET_KEY` (from the environment, else from `.env`) to `SECRET_KEY_DIR/<name>.key` with mode `600` |
| `SECRET_KEY_DIR` | `BACKUP_DIR` | where the `.key` file goes. **Keep it apart from the dumps**: a dump and its key on the same disk let anyone who reads that disk decrypt every provider credential. The script warns when both land in the same directory |
| `COMPOSE_PROJECT` / `-p` | unset | Compose project name |

What it does: `pg_dump -Fc --no-owner --no-privileges` into `<name>.dump.partial`,
`pg_restore --list` on the result (a corrupt or truncated file fails here and is
deleted), rename to `<name>.dump`, print the size, then rotate. The final line on
stdout is the path of the new dump, so it composes with other tooling:

```bash
f=$(scripts/backup.sh) && aws s3 cp "$f" s3://my-bucket/ragmux/
```

### `scripts/restore.sh`

```bash
scripts/restore.sh backups/ragmux-20260918-153455.dump          # prints the plan, exit 2
scripts/restore.sh --yes backups/ragmux-20260918-153455.dump    # runs it
ADMIN_USER=admin ADMIN_PASSWORD=… scripts/restore.sh --yes …    # also reports migrations_version
make restore FILE=backups/ragmux-20260918-153455.dump YES=1
```

| Variable | Default | Meaning |
|---|---|---|
| `LAYOUT` | `auto` | `split` or `aio` to skip the layout detection |
| `POSTGRES_SERVICE`, `RAGMUX_SERVICE` | `postgres`, `ragmux` | Compose services |
| `POSTGRES_DB` | `ragmux` | database |
| `POSTGRES_USER` | `ragmux` (split), `postgres` (aio) | superuser the restore runs as |
| `DATABASE_URL` | unset | use local `pg_restore` against this URL instead of Compose |
| `STOP_CMD`, `START_CMD` | unset | in `DATABASE_URL` mode: shell commands run before and after the restore (e.g. `systemctl stop ragmux`) |
| `APP_ROLE` | `ragmux_app` | role that receives ownership of the restored tables (step 4) |
| `RAGMUX_URL` | `http://localhost:8765` | where to poll `/healthz` |
| `WAIT_TIMEOUT` | `120` | seconds to wait for the gateway |
| `ADMIN_USER`, `ADMIN_PASSWORD` | unset | when both are set, log in and print `migrations_version`, vector table count and document bytes from `/admin/api/system` |

Steps, in order:

1. Verify the file with `pg_restore --list` (in the all-in-one layout through a
   throwaway `docker compose run --rm --entrypoint pg_restore ragmux --list`, before
   anything is stopped).
2. Stop the gateway (`docker compose stop ragmux`; in `DATABASE_URL` mode run `STOP_CMD`
   or warn). This matters: `--clean` drops tables and blocks on open connections.
   **All-in-one:** stopping the `ragmux` service stops the embedded Postgres too, so
   the script then starts a one-off container on the same volume that runs Postgres
   alone: `docker compose run -d --rm --no-deps ragmux postgres-only`, and waits for
   `pg_isready` inside it (up to 90 s).
3. `pg_restore --clean --if-exists --no-owner --no-privileges -d ragmux`, through
   `docker compose exec -T postgres` (split) or `docker exec -i -u postgres <one-off>`
   (aio). Every object in the dump is dropped and recreated, so the target does not
   need to be empty.
4. Hand the restored tables to the application role (`APP_ROLE`, default `ragmux_app`),
   see *Ownership* below.
5. Start the gateway (`docker compose up -d ragmux`; `START_CMD` in direct mode).
   All-in-one: the one-off container is stopped first (`docker stop`, a clean
   `pg_ctl stop -m fast`), then the normal service comes up. Migrations run at startup.
6. Poll `/healthz` and, if admin credentials are set, print the system info.

When the restore fails the gateway is left stopped (and, all-in-one, the one-off
container is stopped as well) so you can investigate before migrations run on a
half-restored schema. Done by hand, the all-in-one sequence is:

```bash
docker compose stop ragmux
cid=$(docker compose run -d --rm --no-deps ragmux postgres-only)
until docker exec -u postgres "$cid" pg_isready -q; do sleep 1; done
docker exec -i -u postgres "$cid" pg_restore --clean --if-exists --no-owner --no-privileges -d ragmux < <dump>
docker stop "$cid"          # clean Postgres shutdown; the container removes itself
docker compose up -d ragmux # the entrypoint re-runs 01-ragmux.sql: tables handed to ragmux_app
```

**Ownership.** `pg_restore` runs as the superuser (`POSTGRES_USER`) with `--no-owner`,
so every restored table belongs to the superuser, while the gateway connects as the
least-privilege role `ragmux_app` (see [Database privileges](configuration.md#database-privileges)).
Reads would fail and the next migration's `ALTER TABLE` too, so right after the restore
the script changes the owner of every table and sequence in `public` to `APP_ROLE`
(default `ragmux_app`) and grants it `ALL` on them. The step is skipped with a note when
the role does not exist (a deployment that still uses the superuser `DATABASE_URL`), and
in `DATABASE_URL` mode it needs `psql` on `PATH` (otherwise a warning tells you to run it
by hand: `ALTER TABLE ... OWNER TO ragmux_app` for each table, or simply re-run
`docker/postgres-init/01-ragmux.sql`, whose last block does the same). The all-in-one
entrypoint runs that SQL on every start anyway, so a restore done by hand there is
fixed up by the next `docker compose up -d`.

**Extension errors.** The dump contains `DROP EXTENSION IF EXISTS vector` and
`CREATE EXTENSION vector`. On the bundled Postgres (either layout) the restore runs as the superuser,
so both succeed silently. On servers where the extension was created by a superuser and
the restore runs as a plain role (managed Postgres, shared clusters) the drop fails and
the create then reports `extension "vector" already exists`. `pg_restore` marks any failed statement with a
non-zero exit, so the script reads its stderr instead of using `--exit-on-error`:
errors mentioning `extension` are printed as *ignored*, any other `pg_restore: error:`
line fails the restore with exit `1`, and the gateway is left stopped so you can
investigate without it re-running migrations on a half-restored schema.

**Leftover tables.** `--clean` only drops what the dump contains. If the target
already had a `chunk_embeddings_<dims>` table for a width the dump does not include,
that table survives, empty of anything the restored stores reference. It is harmless;
drop it by hand if you want a tidy schema.

## Scheduled backups (Compose profile)

Both Compose files ship a `backup` service under the `backup` profile that runs
[`prodrigestivill/postgres-backup-local:17`](https://github.com/prodrigestivill/docker-postgres-backup-local):

```bash
docker compose --profile backup up -d          # starts ragmux (and postgres) plus backup
docker compose --profile backup exec backup /backup.sh   # trigger a run right now
```

With the default all-in-one file the backup container reaches the embedded Postgres
over the unix socket shared through the `ragmux-pgsocket` volume (`POSTGRES_HOST=/var/run/postgresql`,
user `postgres`, trust authentication; the `POSTGRES_PASSWORD` the image insists on is a
placeholder). With the split file it connects over TCP with `POSTGRES_PASSWORD`.

It dumps `ragmux` on `SCHEDULE` (default `@daily`, cron syntax accepted) into
`./backups/{last,daily,weekly,monthly}/ragmux-<stamp>.sql.gz` and keeps
`BACKUP_KEEP_DAYS=7`, `BACKUP_KEEP_WEEKS=4`, `BACKUP_KEEP_MONTHS=6` of history. Override
these and `BACKUP_SCHEDULE` in `.env`. The image's own healthcheck listens on 8081
inside the container (not published).

These files are **plain SQL, gzip-compressed**, not the custom format `restore.sh`
expects. Restore one with `psql` after stopping the gateway; split stack:

```bash
docker compose -f docker-compose.split.yml stop ragmux
gunzip -c backups/daily/ragmux-20260918.sql.gz \
  | docker compose -f docker-compose.split.yml exec -T postgres psql -v ON_ERROR_STOP=0 -U ragmux -d ragmux
docker compose -f docker-compose.split.yml up -d ragmux
```

All-in-one (a one-off Postgres on the volume, as `restore.sh` does it):

```bash
docker compose stop ragmux
cid=$(docker compose run -d --rm --no-deps ragmux postgres-only)
until docker exec -u postgres "$cid" pg_isready -q; do sleep 1; done
gunzip -c backups/daily/ragmux-20260918.sql.gz \
  | docker exec -i -u postgres "$cid" psql -v ON_ERROR_STOP=0 -d ragmux
docker stop "$cid"
docker compose up -d ragmux
```

The scheduled dumps use `CREATE EXTENSION IF NOT EXISTS`, but they do not `DROP` existing
tables, so restore them into an empty database (`docker compose down -v` first, or a
fresh `CREATE DATABASE`). Point the `backups` directory (or an rsync/object-storage job
reading it) at storage that is not on the same disk as the database volume.

## Physical backups and point-in-time recovery

Logical dumps give you an RPO equal to the dump interval. If losing more than a few
minutes of request logs, uploads or key rotations is unacceptable, add continuous WAL
archiving with [pgBackRest](https://pgbackrest.org/) or
[wal-g](https://github.com/wal-g/wal-g): a periodic base backup plus every WAL segment,
restorable to any second. This is worth it when the database is large (dumps take
long and rebuild HNSW indexes on restore), when ingestion is continuous, or when the
gateway's request log is your audit trail. It needs a sidecar or a custom Postgres
image with the tool installed and access to the data directory; keep `SECRET_KEY` in the
same disaster-recovery bundle exactly as with dumps.

**Managed Postgres** (Amazon RDS/Aurora, Cloud SQL, Azure Database, Neon, Supabase,
Crunchy Bridge…): use the provider's automated snapshots and PITR; they cover the
`chunk_embeddings_*` tables like any other. Still take a periodic `pg_dump` for
off-provider copies and for moving between providers, and still keep `SECRET_KEY`
outside the provider. Enable the `vector` extension on the target before restoring.

## Restore runbook

Prerequisites: a dump, the matching `SECRET_KEY`, the Ragmux image version you intend
to run, and a Postgres 17 with pgvector (the all-in-one image brings its own).

1. **Stop the gateway** so nothing writes during the restore:
   `docker compose stop ragmux` (all-in-one: this stops the embedded Postgres too;
   `restore.sh` brings a Postgres-only container up for the restore).
2. **Restore**: `scripts/restore.sh --yes <dump>` (it stops and starts the gateway for
   you in either layout), or by hand as shown under
   [`scripts/restore.sh`](#scriptsrestoresh): split stack
   `docker compose exec -T postgres pg_restore --clean --if-exists --no-owner --no-privileges -U ragmux -d ragmux < <dump>`
   followed by the ownership hand-over the script performs (re-running
   `docker/postgres-init/01-ragmux.sql` as in [Database privileges](configuration.md#database-privileges) does it);
   all-in-one, the `postgres-only` sequence, after which the entrypoint's own start
   does the hand-over.
3. **Start** with the right `SECRET_KEY` in `.env`: `docker compose up -d ragmux`.
   Migrations run automatically under an advisory lock.
   - Restoring an **older dump into a newer Ragmux** is supported: `schema_migrations`
     comes from the dump and only the missing versions are applied on start.
   - Restoring a **newer dump into an older Ragmux** is not: the old binary does not know
     the newer schema. Upgrade the image first.
4. **Verify** (or let the script do the first three):
   - `curl -fsS http://localhost:8765/healthz` returns `{"status":"ok",…}`.
   - Log in to `/admin/` with a known user.
   - `GET /admin/api/system`: `database.migrations_version` matches the app's expected
     version, `backup.tables` and `backup.documents_bytes` look like the source.
   - Models tab: `api_key_masked` is shown and **Test chat** succeeds for one connection
     (this proves `SECRET_KEY` is right).
   - RAG store: run a search in the dashboard and get hits.
   - Projects: the `api_key_prefix` values are the ones your clients use; make one real
     `POST /v1/chat/completions` with an existing `sk-proj-…` key.
5. Take a fresh backup of the restored system so the chain of dumps continues.

## Disaster-recovery drill

Rehearse on a scratch project at least quarterly and before every major upgrade:

- [ ] `scripts/backup.sh` on production (or fetch the latest scheduled dump).
- [ ] On a clean machine or with a different Compose project name
      (`COMPOSE_PROJECT_NAME=ragmux-drill`, different `ports:`), `docker compose up -d`
      (a fresh `ragmux-data` volume; the dump may come from either layout).
- [ ] `scripts/restore.sh --yes <dump>` with the production `SECRET_KEY`.
- [ ] Run the verification checklist above; record how long the restore took (your RTO).
- [ ] Restore once **without** the key to see the decryption error, so the team knows
      what "lost the key" looks like and where the key lives.
- [ ] `docker compose down -v` the drill stack.

| Setup | Typical RPO | Typical RTO | Notes |
|---|---|---|---|
| Manual `scripts/backup.sh` | since last run | minutes | fine for evaluation and small teams |
| `backup` profile, `@daily` | up to 24 h | minutes | set `BACKUP_SCHEDULE="0 */6 * * *"` for 6 h |
| `backup` profile + off-site copy | up to one interval | tens of minutes | survives loss of the host |
| WAL archiving (pgBackRest / wal-g) | seconds to minutes | tens of minutes | large databases, continuous ingestion |
| Managed Postgres PITR | provider SLA (usually minutes) | provider SLA | plus a periodic `pg_dump` off-provider |

Rules of thumb:

- **Back up before every upgrade.** `docker compose pull && scripts/backup.sh && docker compose up -d`.
  Migrations are forward-only.
- Keep at least 7 daily, 4 weekly and 6 monthly dumps, and one copy off the host.
- Alert when the newest dump is older than twice the schedule.
- Keep dumps from before a `SECRET_KEY` rotation together with the old key.

## Moving between layouts

The all-in-one volume (`ragmux-data`, `/data/pg`) and the split stack's `pgdata` volume
have different names and directory structures, and nothing converts one into the other:
moving between the layouts (including from the 0.3.0 two-container `docker-compose.yml`
to the default single-container one) is a dump and a restore.

1. Take a dump on the old layout: `scripts/backup.sh` (it detects the layout; add
   `-f docker-compose.split.yml` / `COMPOSE_FILE` when the split file is the old one).
2. Stop the old stack: `docker compose [-f docker-compose.split.yml] down` (keep the
   volume until the new layout is verified).
3. Start the new layout on a fresh volume with the **same `SECRET_KEY`** in `.env`:
   `docker compose [-f docker-compose.split.yml] up -d` (the split file also needs
   `POSTGRES_PASSWORD` and `RAGMUX_DB_PASSWORD`).
4. `scripts/restore.sh --yes <dump>`, then the verification checklist above.
5. Once verified, remove the old volume (`docker volume rm <project>_pgdata` or
   `<project>_ragmux-data`).

A deployment that used the secret.key fallback (all-in-one without `SECRET_KEY`) must
copy `/data/ragmux/secret.key` out of the volume first
(`docker compose cp ragmux:/data/ragmux/secret.key ./secret.key`) and set its content as
`SECRET_KEY` on the new layout.

## Rotating SECRET_KEY

Rotate the key when it may have leaked, when someone who knew it leaves, or on a
schedule. `ragmux rotate-key` re-encrypts every stored provider API key in one
transaction, verifies each row decrypts with the new key before committing and prints
the number of rows changed. It needs the **current** key in the environment
(`SECRET_KEY`, `SECRET_KEY_FILE` or the `secret.key` fallback) and `DATABASE_URL`.

1. Take a backup (`scripts/backup.sh`) and keep the old key with it.
2. Stop the gateway so nothing writes a credential with the old key meanwhile:
   `docker compose stop ragmux`.
3. Generate the new key and run the rotation with the old one still configured:

   ```bash
   NEW_KEY=$(openssl rand -hex 32)
   docker compose run --rm ragmux rotate-key --new "$NEW_KEY"
   # rotate-key: re-encrypted 3 model connection(s); start the gateway with the new SECRET_KEY
   ```

   In the all-in-one layout that one-off container starts the embedded Postgres on
   the volume for the duration of the command and shuts it down cleanly afterwards
   (the stopped service must not be running at the same time, hence step 2).

   With the binary: `SECRET_KEY=<old> DATABASE_URL=… ragmux rotate-key --new "$NEW_KEY"`.
4. Put the new key into `.env` (or the secret file) and start the gateway:
   `docker compose up -d ragmux`.
5. Verify: **Test chat** on a model connection succeeds. If the gateway was started with
   the wrong key it refuses to start at all (`SECRET_KEY does not match the one this
   database was written with`); put the right key back, nothing was lost.

Rows are only rewritten when every one of them decrypts with the current key and
re-decrypts with the new one; otherwise the command exits `1` and the database is
unchanged. The same transaction re-seals the `instance_settings` canary, so the first
start after a rotation accepts the new key and refuses the old one. Dumps taken before
the rotation still need the old key.

Since 0.2.3 each stored key is also bound to its connection id (`key_version = 1` in
`model_connections`), so a ciphertext moved to another row does not decrypt. Dumps taken
with an earlier version restore fine: the gateway re-seals every `key_version = 0` row
with the same `SECRET_KEY` on its first start and logs how many it changed. That start
fails with `upgrade stored provider keys` when the key does not match the dump; put the
right key back and start again.

## Security notes

Dumps contain the encrypted provider API keys, every uploaded document, password
hashes, the audit log and request logs (which include model names, token counts and
error messages, but not prompts or completions). Treat them like the database itself:
encrypt at rest (`age`, `gpg`, or an encrypted bucket), restrict who can read the
`backups` directory (`backup.sh` creates it with mode `700` and every file with `600`),
and do not store `<name>.key` next to the dump on the same unencrypted disk unless the
whole disk is encrypted: point `SECRET_KEY_DIR` somewhere else. When you copy the key
file elsewhere, keep its permissions.
