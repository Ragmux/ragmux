#!/usr/bin/env bash
# Restore a pg_dump custom-format dump into the Ragmux database.
#
# Usage: scripts/restore.sh [--yes] [-p COMPOSE_PROJECT] <dump-file>
#
# See docs/backup-restore.md for the full runbook.
set -euo pipefail
umask 077

usage() {
  cat <<'USAGE'
Usage: scripts/restore.sh [--yes] [-p COMPOSE_PROJECT] <dump-file>

Restores a dump made by scripts/backup.sh (pg_dump -Fc). Without --yes the
script only prints what it would do and exits 2.

Steps:
  1. verify the dump with pg_restore --list
  2. stop the gateway (compose: `docker compose stop <RAGMUX_SERVICE>`,
     direct: STOP_CMD if set). In the all-in-one layout this also stops the
     embedded Postgres; the script then starts a one-off container
     (`docker compose run -d --rm <RAGMUX_SERVICE> postgres-only`) that runs
     Postgres alone and restores into it over the unix socket
  3. pg_restore --clean --if-exists --no-owner --no-privileges
     Errors about the vector extension (already exists / cannot drop / must
     be owner) are expected when the target already has it and are ignored;
     any other error fails the restore.
  4. hand the restored tables to the application role: when the role named
     by APP_ROLE (default ragmux_app, created by docker/postgres-init) exists,
     ALTER ... OWNER TO and GRANT ALL on every table and sequence in public.
     pg_restore runs as POSTGRES_USER (the superuser) with --no-owner, so
     without this step the gateway could neither read the tables nor apply
     the next migration (ALTER TABLE needs ownership). Skipped with a note
     when the role does not exist (deployments still using the superuser URL).
  5. start the gateway (compose: `docker compose up -d <RAGMUX_SERVICE>`,
     direct: START_CMD if set); migrations run automatically. All-in-one:
     the one-off container is stopped first (clean Postgres shutdown)
  6. wait for RAGMUX_URL/healthz, then print migrations_version from
     /admin/api/system when ADMIN_USER and ADMIN_PASSWORD are set

Where the restore goes (first match wins):
  1. DATABASE_URL is set and pg_restore is on PATH -> local pg_restore
  2. the compose project has a <POSTGRES_SERVICE>  -> docker compose exec <POSTGRES_SERVICE> pg_restore
     service (docker-compose.split.yml)
  3. otherwise (all-in-one, docker-compose.yml)    -> one-off postgres-only container, see step 2

Options:
  --yes     actually run (required)
  -p NAME   docker compose project name (same as COMPOSE_PROJECT env; docker
            compose also honours COMPOSE_PROJECT_NAME and COMPOSE_FILE)
  -h        show this help

Environment:
  LAYOUT             auto | split | aio: which compose layout to assume
                     (default auto: split when <POSTGRES_SERVICE> exists)
  POSTGRES_SERVICE   compose service running Postgres    (default postgres)
  RAGMUX_SERVICE     compose service running the gateway (default ragmux)
  POSTGRES_DB        database name                       (default ragmux)
  POSTGRES_USER      database superuser (default ragmux; postgres in aio)
  APP_ROLE           role the gateway connects as; owner of the restored
                     tables after step 4                  (default ragmux_app)
  DATABASE_URL       use local pg_restore against this URL instead of compose
  STOP_CMD/START_CMD shell commands run around the restore in direct mode
  RAGMUX_URL         base URL used for the health wait   (default http://localhost:8765)
  WAIT_TIMEOUT       seconds to wait for /healthz        (default 120)
  ADMIN_USER, ADMIN_PASSWORD  optional; used to read /admin/api/system after start
  COMPOSE_PROJECT    compose project name (same as -p)

Exit codes: 0 success, 1 restore/start failed, 2 usage/configuration error or missing --yes.
USAGE
}

log() { printf '%s %s\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$*" >&2; }
die() { log "error: $1"; exit "${2:-1}"; }

# login_json USER PASSWORD prints the login request body with the credentials
# properly JSON-escaped. Uses jq or python3 when available, otherwise escapes
# backslashes, double quotes and control characters by hand.
login_json() {
  if command -v jq >/dev/null 2>&1; then
    jq -cn --arg u "$1" --arg p "$2" '{username: $u, password: $p, bearer: true}'
  elif command -v python3 >/dev/null 2>&1; then
    RAGMUX_U="$1" RAGMUX_P="$2" python3 -c 'import json,os,sys; sys.stdout.write(json.dumps({"username": os.environ["RAGMUX_U"], "password": os.environ["RAGMUX_P"], "bearer": True}))'
  else
    local u p
    u="$(printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\000-\037')"
    p="$(printf '%s' "$2" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\000-\037')"
    printf '{"username":"%s","password":"%s","bearer":true}' "$u" "$p"
  fi
}

COMPOSE_PROJECT="${COMPOSE_PROJECT:-}"
yes=0
dump=""
while [ $# -gt 0 ]; do
  case "$1" in
    --yes) yes=1 ;;
    -p) [ $# -ge 2 ] || die "option -p requires an argument (see -h)" 2; COMPOSE_PROJECT="$2"; shift ;;
    -p*) COMPOSE_PROJECT="${1#-p}" ;;
    -h|--help) usage; exit 0 ;;
    -*) die "unknown option $1 (see -h)" 2 ;;
    *) [ -z "$dump" ] || die "unexpected argument: $1 (see -h)" 2; dump="$1" ;;
  esac
  shift
done
[ -n "$dump" ] || { usage >&2; exit 2; }
[ -f "$dump" ] || die "dump file not found: $dump" 2
[ -s "$dump" ] || die "dump file is empty: $dump" 2

LAYOUT="${LAYOUT:-auto}"
POSTGRES_SERVICE="${POSTGRES_SERVICE:-postgres}"
RAGMUX_SERVICE="${RAGMUX_SERVICE:-ragmux}"
POSTGRES_DB="${POSTGRES_DB:-ragmux}"
APP_ROLE="${APP_ROLE:-ragmux_app}"
RAGMUX_URL="${RAGMUX_URL:-http://localhost:8765}"
case "$APP_ROLE" in
  *[!A-Za-z0-9_]*|'') die "APP_ROLE must be a plain identifier, got '$APP_ROLE'" 2 ;;
esac
WAIT_TIMEOUT="${WAIT_TIMEOUT:-120}"

mode=""
if [ -n "${DATABASE_URL:-}" ] && command -v pg_restore >/dev/null 2>&1; then
  mode="direct"
  list_cmd() { pg_restore --list "$dump" >/dev/null; }
  restore_cmd() { pg_restore --clean --if-exists --no-owner --no-privileges -d "$DATABASE_URL" "$dump"; }
  psql_cmd() {
    if command -v psql >/dev/null 2>&1; then
      psql -v ON_ERROR_STOP=1 -qAt -d "$DATABASE_URL" "$@"
    else
      log "warning: psql not on PATH; run scripts/restore.sh's ownership step by hand (see -h, step 4)"
      return 3
    fi
  }
  stop_app() { if [ -n "${STOP_CMD:-}" ]; then sh -c "$STOP_CMD"; else log "direct mode: no STOP_CMD set, make sure no gateway instance is connected"; fi; }
  start_app() { if [ -n "${START_CMD:-}" ]; then sh -c "$START_CMD"; else log "direct mode: no START_CMD set, start the gateway yourself"; return 3; fi; }
else
  mode="compose"
  command -v docker >/dev/null 2>&1 || die "docker not found and DATABASE_URL/pg_restore not usable" 2
  compose=(docker compose)
  [ -n "$COMPOSE_PROJECT" ] && compose+=(-p "$COMPOSE_PROJECT")
  if [ "$LAYOUT" = auto ]; then
    if "${compose[@]}" config --services 2>/dev/null | grep -qx "$POSTGRES_SERVICE"; then
      LAYOUT="split"
    else
      LAYOUT="aio"
    fi
  fi
  case "$LAYOUT" in
    split)
      POSTGRES_USER="${POSTGRES_USER:-ragmux}"
      if ! "${compose[@]}" ps --status running --services 2>/dev/null | grep -qx "$POSTGRES_SERVICE"; then
        die "compose service '$POSTGRES_SERVICE' is not running (run from the directory holding docker-compose.yml, or set COMPOSE_FILE / -p)" 2
      fi
      list_cmd() { "${compose[@]}" exec -T "$POSTGRES_SERVICE" pg_restore --list >/dev/null <"$dump"; }
      restore_cmd() {
        "${compose[@]}" exec -T "$POSTGRES_SERVICE" \
          pg_restore --clean --if-exists --no-owner --no-privileges -U "$POSTGRES_USER" -d "$POSTGRES_DB" <"$dump"
      }
      psql_cmd() {
        "${compose[@]}" exec -T "$POSTGRES_SERVICE" \
          psql -v ON_ERROR_STOP=1 -qAt -U "$POSTGRES_USER" -d "$POSTGRES_DB" "$@"
      }
      stop_app() { "${compose[@]}" stop "$RAGMUX_SERVICE"; }
      start_app() { "${compose[@]}" up -d --no-build "$RAGMUX_SERVICE"; }
      ;;
    aio)
      # Postgres lives inside the gateway container and only listens on a
      # unix socket. The container is stopped as a whole, a one-off copy of
      # the service runs Postgres alone on the same volume, the restore goes
      # through `docker exec` into it, and the normal service comes back up.
      POSTGRES_USER="${POSTGRES_USER:-postgres}"
      "${compose[@]}" config --services 2>/dev/null | grep -qx "$RAGMUX_SERVICE" \
        || die "compose service '$RAGMUX_SERVICE' not found (run from the directory holding docker-compose.yml, or set COMPOSE_FILE / -p)" 2
      oneoff=""
      stop_oneoff() {
        if [ -n "$oneoff" ]; then
          log "stopping the postgres-only container"
          docker stop -t 90 "$oneoff" >/dev/null || log "warning: could not stop container $oneoff"
          oneoff=""
        fi
      }
      list_cmd() {
        # Verifies with the image's own pg_restore before anything is stopped.
        "${compose[@]}" run --rm -T --no-deps --entrypoint pg_restore "$RAGMUX_SERVICE" --list >/dev/null <"$dump"
      }
      stop_app() {
        "${compose[@]}" stop "$RAGMUX_SERVICE" || return 1
        log "starting a postgres-only container on the data volume"
        oneoff="$("${compose[@]}" run -d --rm --no-deps "$RAGMUX_SERVICE" postgres-only)" || return 1
        local deadline=$(( $(date +%s) + 90 ))
        until docker exec -u postgres "$oneoff" pg_isready -q -U postgres 2>/dev/null; do
          if ! docker inspect -f '{{.State.Running}}' "$oneoff" 2>/dev/null | grep -q true; then
            oneoff=""; log "the postgres-only container exited (docker compose logs shows why)"; return 1
          fi
          [ "$(date +%s)" -lt "$deadline" ] || { log "postgres did not become ready within 90s"; return 1; }
          sleep 1
        done
      }
      restore_cmd() {
        docker exec -i -u postgres "$oneoff" \
          pg_restore --clean --if-exists --no-owner --no-privileges -U "$POSTGRES_USER" -d "$POSTGRES_DB" <"$dump"
      }
      psql_cmd() {
        docker exec -i -u postgres "$oneoff" \
          psql -v ON_ERROR_STOP=1 -qAt -U "$POSTGRES_USER" -d "$POSTGRES_DB" "$@"
      }
      start_app() { stop_oneoff; "${compose[@]}" up -d --no-build "$RAGMUX_SERVICE"; }
      ;;
    *) die "LAYOUT must be auto, split or aio, got '$LAYOUT'" 2 ;;
  esac
  mode="compose/$LAYOUT"
fi
POSTGRES_USER="${POSTGRES_USER:-ragmux}"

size="$(wc -c <"$dump" | tr -d ' ')"
cat >&2 <<PLAN
Restore plan (mode=$mode):
  dump:      $dump ($size bytes)
  database:  $POSTGRES_DB as $POSTGRES_USER$( [ "$mode" = direct ] && printf ' (DATABASE_URL)' )
  gateway:   $( [ "$mode" != direct ] && printf 'compose service %s' "$RAGMUX_SERVICE" || printf 'STOP_CMD/START_CMD' ), health at $RAGMUX_URL/healthz
  action:    stop gateway -> pg_restore --clean --if-exists --no-owner --no-privileges -> start gateway
  WARNING:   every table in the database is dropped and replaced by the dump's content.
PLAN
if [ "$yes" -ne 1 ]; then
  log "refusing to continue without --yes"
  exit 2
fi

log "verifying dump"
list_cmd || die "verification failed: pg_restore --list could not read $dump"

log "stopping gateway"
stop_app || die "could not stop the gateway"

log "restoring"
errlog="$(mktemp "${TMPDIR:-/tmp}/ragmux-restore.XXXXXX")"
hdr=""
cleanup() {
  rm -f "$errlog" "$hdr"
  if [ "$mode" = compose/aio ]; then stop_oneoff; fi
}
trap cleanup EXIT
status=0
restore_cmd 2>"$errlog" || status=$?

# pg_restore exits 1 whenever any statement failed. Separate the harmless
# extension noise from real failures.
ext_re='extension'
real_errors="$(grep -E '^pg_restore: error:' "$errlog" | grep -Eiv "$ext_re" || true)"
ext_errors="$(grep -E '^pg_restore: error:' "$errlog" | grep -Ei "$ext_re" || true)"
if [ -n "$ext_errors" ]; then
  log "ignored extension errors (the vector extension already exists on the target):"
  printf '%s\n' "$ext_errors" >&2
fi
if [ -n "$real_errors" ] || { [ "$status" -ne 0 ] && [ -z "$ext_errors" ]; }; then
  log "pg_restore failed (exit $status):"
  grep -Ev '^pg_restore: error:.*extension|^pg_restore: warning: errors ignored' "$errlog" >&2 || true
  die "restore failed; the gateway was left stopped$( [ "$mode" = compose/aio ] && printf ' (the postgres-only container is stopped too; the data volume keeps whatever pg_restore wrote)' )"
fi
log "restore finished"

# The dump was written with --no-owner and restored by the superuser, so every
# table now belongs to POSTGRES_USER. The gateway connects as APP_ROLE (a
# plain role without superuser rights, see docker/postgres-init/01-ragmux.sql)
# and needs to own its tables: migrations run ALTER TABLE, which only the
# owner may do, and the GRANTs cover reads and writes on everything else.
log "handing tables in schema public to $APP_ROLE"
own_status=0
psql_cmd -v role="$APP_ROLE" <<'SQL' || own_status=$?
SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'role')
            THEN 'role ' || :'role' || ' found'
            ELSE 'role ' || :'role' || ' does not exist; tables stay with the superuser' END;
-- psql variables are not expanded inside dollar quotes: pass the role name
-- to the DO block through a session setting instead.
SET ragmux.app_role TO :'role';
DO $$
DECLARE
    r RECORD;
    role TEXT := current_setting('ragmux.app_role');
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role) THEN
        RETURN;
    END IF;
    FOR r IN SELECT tablename AS name FROM pg_tables WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO %I', r.name, role);
    END LOOP;
    FOR r IN SELECT sequencename AS name FROM pg_sequences WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER SEQUENCE public.%I OWNER TO %I', r.name, role);
    END LOOP;
    EXECUTE format('GRANT ALL ON ALL TABLES IN SCHEMA public TO %I', role);
    EXECUTE format('GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO %I', role);
END
$$;
SQL
case "$own_status" in
  0) ;;
  3) ;; # direct mode without psql: already warned
  *) die "could not hand the restored tables to $APP_ROLE (exit $own_status); the gateway was left stopped" ;;
esac

log "starting gateway"
start_status=0
start_app || start_status=$?
if [ "$start_status" -eq 3 ]; then
  log "done: restore complete; start the gateway and check $RAGMUX_URL/healthz"
  exit 0
fi
[ "$start_status" -eq 0 ] || die "could not start the gateway"

log "waiting up to ${WAIT_TIMEOUT}s for $RAGMUX_URL/healthz"
deadline=$(( $(date +%s) + WAIT_TIMEOUT ))
until curl -fsS -m 3 "$RAGMUX_URL/healthz" >/dev/null 2>&1; do
  if [ "$(date +%s)" -ge "$deadline" ]; then
    die "gateway did not become healthy within ${WAIT_TIMEOUT}s (check the container log)"
  fi
  sleep 2
done
log "gateway healthy"

if [ -n "${ADMIN_USER:-}" ] && [ -n "${ADMIN_PASSWORD:-}" ]; then
  # Credentials never appear on a command line (visible in `ps` and shell
  # history): the login body goes through stdin and the bearer header
  # through a private temp file that curl reads with -H @file.
  hdr="$(mktemp "${TMPDIR:-/tmp}/ragmux-restore-hdr.XXXXXX")"
  token="$(login_json "$ADMIN_USER" "$ADMIN_PASSWORD" \
    | curl -fsS -m 10 -H 'Content-Type: application/json' -d @- "$RAGMUX_URL/admin/api/login" 2>/dev/null \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p' || true)"
  if [ -z "$token" ]; then
    log "warning: login as $ADMIN_USER failed; skipping the /admin/api/system check"
  else
    printf 'Authorization: Bearer %s\n' "$token" >"$hdr"
    sys="$(curl -fsS -m 10 -H "@$hdr" "$RAGMUX_URL/admin/api/system" || true)"
    mig="$(printf '%s' "$sys" | sed -n 's/.*"migrations_version":\([0-9]*\).*/\1/p')"
    tables="$(printf '%s' "$sys" | sed -n 's/.*"tables":\([0-9]*\).*/\1/p')"
    docs="$(printf '%s' "$sys" | sed -n 's/.*"documents_bytes":\([0-9]*\).*/\1/p')"
    log "system: migrations_version=${mig:-?} vector_tables=${tables:-?} documents_bytes=${docs:-?}"
    curl -fsS -m 10 -X POST -H "@$hdr" "$RAGMUX_URL/admin/api/logout" >/dev/null 2>&1 || true
    unset token
  fi
fi
log "done: restore complete"
