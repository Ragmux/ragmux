#!/usr/bin/env bash
# Restore a pg_dump custom-format dump into the Ragmux database.
#
# Usage: scripts/restore.sh [--yes] [-p COMPOSE_PROJECT] <dump-file>
#
# See docs/backup-restore.md for the full runbook.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/restore.sh [--yes] [-p COMPOSE_PROJECT] <dump-file>

Restores a dump made by scripts/backup.sh (pg_dump -Fc). Without --yes the
script only prints what it would do and exits 2.

Steps:
  1. verify the dump with pg_restore --list
  2. stop the gateway (compose: `docker compose stop <RAGMUX_SERVICE>`,
     direct: STOP_CMD if set)
  3. pg_restore --clean --if-exists --no-owner --no-privileges
     Errors about the vector extension (already exists / cannot drop / must
     be owner) are expected when the target already has it and are ignored;
     any other error fails the restore.
  4. start the gateway (compose: `docker compose up -d <RAGMUX_SERVICE>`,
     direct: START_CMD if set); migrations run automatically
  5. wait for RAGMUX_URL/healthz, then print migrations_version from
     /admin/api/system when ADMIN_USER and ADMIN_PASSWORD are set

Where the restore goes (first match wins):
  1. DATABASE_URL is set and pg_restore is on PATH -> local pg_restore
  2. otherwise                                      -> docker compose exec <POSTGRES_SERVICE> pg_restore

Options:
  --yes     actually run (required)
  -p NAME   docker compose project name (same as COMPOSE_PROJECT env; docker
            compose also honours COMPOSE_PROJECT_NAME and COMPOSE_FILE)
  -h        show this help

Environment:
  POSTGRES_SERVICE   compose service running Postgres    (default postgres)
  RAGMUX_SERVICE     compose service running the gateway (default ragmux)
  POSTGRES_DB        database name                       (default ragmux)
  POSTGRES_USER      database role                       (default ragmux)
  DATABASE_URL       use local pg_restore against this URL instead of compose
  STOP_CMD/START_CMD shell commands run around the restore in direct mode
  RAGMUX_URL         base URL used for the health wait   (default http://localhost:8080)
  WAIT_TIMEOUT       seconds to wait for /healthz        (default 120)
  ADMIN_USER, ADMIN_PASSWORD  optional; used to read /admin/api/system after start
  COMPOSE_PROJECT    compose project name (same as -p)

Exit codes: 0 success, 1 restore/start failed, 2 usage/configuration error or missing --yes.
USAGE
}

log() { printf '%s %s\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$*" >&2; }
die() { log "error: $1"; exit "${2:-1}"; }

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

POSTGRES_SERVICE="${POSTGRES_SERVICE:-postgres}"
RAGMUX_SERVICE="${RAGMUX_SERVICE:-ragmux}"
POSTGRES_DB="${POSTGRES_DB:-ragmux}"
POSTGRES_USER="${POSTGRES_USER:-ragmux}"
RAGMUX_URL="${RAGMUX_URL:-http://localhost:8080}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-120}"

mode=""
if [ -n "${DATABASE_URL:-}" ] && command -v pg_restore >/dev/null 2>&1; then
  mode="direct"
  list_cmd() { pg_restore --list "$dump" >/dev/null; }
  restore_cmd() { pg_restore --clean --if-exists --no-owner --no-privileges -d "$DATABASE_URL" "$dump"; }
  stop_app() { if [ -n "${STOP_CMD:-}" ]; then sh -c "$STOP_CMD"; else log "direct mode: no STOP_CMD set, make sure no gateway instance is connected"; fi; }
  start_app() { if [ -n "${START_CMD:-}" ]; then sh -c "$START_CMD"; else log "direct mode: no START_CMD set, start the gateway yourself"; return 3; fi; }
else
  mode="compose"
  command -v docker >/dev/null 2>&1 || die "docker not found and DATABASE_URL/pg_restore not usable" 2
  compose=(docker compose)
  [ -n "$COMPOSE_PROJECT" ] && compose+=(-p "$COMPOSE_PROJECT")
  if ! "${compose[@]}" ps --status running --services 2>/dev/null | grep -qx "$POSTGRES_SERVICE"; then
    die "compose service '$POSTGRES_SERVICE' is not running (run from the directory holding docker-compose.yml, or set COMPOSE_FILE / -p)" 2
  fi
  list_cmd() { "${compose[@]}" exec -T "$POSTGRES_SERVICE" pg_restore --list >/dev/null <"$dump"; }
  restore_cmd() {
    "${compose[@]}" exec -T "$POSTGRES_SERVICE" \
      pg_restore --clean --if-exists --no-owner --no-privileges -U "$POSTGRES_USER" -d "$POSTGRES_DB" <"$dump"
  }
  stop_app() { "${compose[@]}" stop "$RAGMUX_SERVICE"; }
  start_app() { "${compose[@]}" up -d --no-build "$RAGMUX_SERVICE"; }
fi

size="$(wc -c <"$dump" | tr -d ' ')"
cat >&2 <<PLAN
Restore plan (mode=$mode):
  dump:      $dump ($size bytes)
  database:  $POSTGRES_DB as $POSTGRES_USER$( [ "$mode" = direct ] && printf ' (DATABASE_URL)' )
  gateway:   $( [ "$mode" = compose ] && printf 'compose service %s' "$RAGMUX_SERVICE" || printf 'STOP_CMD/START_CMD' ), health at $RAGMUX_URL/healthz
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
trap 'rm -f "$errlog"' EXIT
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
  die "restore failed; the gateway was left stopped"
fi
log "restore finished"

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
  body="$(printf '{"username":"%s","password":"%s"}' "$ADMIN_USER" "$ADMIN_PASSWORD")"
  token="$(curl -fsS -m 10 -H 'Content-Type: application/json' -d "$body" "$RAGMUX_URL/admin/api/login" 2>/dev/null \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p' || true)"
  if [ -z "$token" ]; then
    log "warning: login as $ADMIN_USER failed; skipping the /admin/api/system check"
  else
    sys="$(curl -fsS -m 10 -H "Authorization: Bearer $token" "$RAGMUX_URL/admin/api/system" || true)"
    mig="$(printf '%s' "$sys" | sed -n 's/.*"migrations_version":\([0-9]*\).*/\1/p')"
    tables="$(printf '%s' "$sys" | sed -n 's/.*"tables":\([0-9]*\).*/\1/p')"
    docs="$(printf '%s' "$sys" | sed -n 's/.*"documents_bytes":\([0-9]*\).*/\1/p')"
    log "system: migrations_version=${mig:-?} vector_tables=${tables:-?} documents_bytes=${docs:-?}"
    curl -fsS -m 10 -X POST -H "Authorization: Bearer $token" "$RAGMUX_URL/admin/api/logout" >/dev/null 2>&1 || true
  fi
fi
log "done: restore complete"
