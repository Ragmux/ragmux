#!/usr/bin/env bash
# Logical backup of the Ragmux database with pg_dump (custom format).
#
# Usage: scripts/backup.sh [-p COMPOSE_PROJECT] [-h]
#
# See docs/backup-restore.md for the full guide.
set -euo pipefail
# Dumps contain password hashes, encrypted provider keys and every uploaded
# document: nothing this script writes may be readable by other users.
umask 077

usage() {
  cat <<'USAGE'
Usage: scripts/backup.sh [-p COMPOSE_PROJECT] [-h]

Dumps the Ragmux database to BACKUP_DIR/ragmux-YYYYmmdd-HHMMSS.dump
(pg_dump custom format), verifies the file with pg_restore --list and
deletes dumps older than KEEP_DAYS.

Where the dump is taken from (first match wins):
  1. DATABASE_URL is set and pg_dump is on PATH  -> local pg_dump against that URL
  2. the compose project has a <POSTGRES_SERVICE> -> docker compose exec <POSTGRES_SERVICE> pg_dump
     service (docker-compose.split.yml)
  3. otherwise (all-in-one, docker-compose.yml)   -> docker compose exec -u postgres <RAGMUX_SERVICE> pg_dump
     over the container-local unix socket

Options:
  -p NAME   docker compose project name (same as COMPOSE_PROJECT env; docker
            compose also honours COMPOSE_PROJECT_NAME and COMPOSE_FILE)
  -h        show this help

Environment:
  BACKUP_DIR          output directory                      (default ./backups)
  KEEP_DAYS           delete dumps older than N days, 0 keeps all (default 14)
  LAYOUT              auto | split | aio: which compose layout to assume
                      (default auto: split when <POSTGRES_SERVICE> exists)
  POSTGRES_SERVICE    compose service running Postgres      (default postgres)
  RAGMUX_SERVICE      all-in-one service holding Postgres   (default ragmux)
  POSTGRES_DB         database name                         (default ragmux)
  POSTGRES_USER       database superuser (default ragmux; postgres in aio)
  DATABASE_URL        use a local pg_dump against this URL instead of compose
  INCLUDE_SECRET_KEY  1 = also write SECRET_KEY (from env or .env) to
                      SECRET_KEY_DIR/<name>.key with mode 600
  SECRET_KEY_DIR      where the .key file goes             (default BACKUP_DIR)
                      Prefer a different, separately protected location: a
                      dump plus its key on one disk decrypts every provider
                      credential for whoever reads that disk.
  COMPOSE_PROJECT     compose project name (same as -p)

Exit codes: 0 success, 1 dump or verification failed, 2 usage/configuration error.
USAGE
}

log() { printf '%s %s\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$*" >&2; }
die() { log "error: $1"; exit "${2:-1}"; }

COMPOSE_PROJECT="${COMPOSE_PROJECT:-}"
while getopts ':p:h' opt; do
  case "$opt" in
    p) COMPOSE_PROJECT="$OPTARG" ;;
    h) usage; exit 0 ;;
    :) die "option -$OPTARG requires an argument (see -h)" 2 ;;
    *) die "unknown option -$OPTARG (see -h)" 2 ;;
  esac
done
shift $((OPTIND - 1))
[ $# -eq 0 ] || die "unexpected argument: $1 (see -h)" 2

BACKUP_DIR="${BACKUP_DIR:-./backups}"
KEEP_DAYS="${KEEP_DAYS:-14}"
LAYOUT="${LAYOUT:-auto}"
POSTGRES_SERVICE="${POSTGRES_SERVICE:-postgres}"
RAGMUX_SERVICE="${RAGMUX_SERVICE:-ragmux}"
POSTGRES_DB="${POSTGRES_DB:-ragmux}"
INCLUDE_SECRET_KEY="${INCLUDE_SECRET_KEY:-0}"
SECRET_KEY_DIR="${SECRET_KEY_DIR:-$BACKUP_DIR}"

case "$KEEP_DAYS" in
  ''|*[!0-9]*) die "KEEP_DAYS must be a non-negative integer, got '$KEEP_DAYS'" 2 ;;
esac

# Pick the dump/verify commands. Direct mode never prints DATABASE_URL.
mode=""
if [ -n "${DATABASE_URL:-}" ] && command -v pg_dump >/dev/null 2>&1 && command -v pg_restore >/dev/null 2>&1; then
  mode="direct"
  dump_cmd() { pg_dump -Fc --no-owner --no-privileges -d "$DATABASE_URL"; }
  list_cmd() { pg_restore --list "$1" >/dev/null; }
else
  mode="compose"
  command -v docker >/dev/null 2>&1 || die "docker not found and DATABASE_URL/pg_dump not usable" 2
  compose=(docker compose)
  [ -n "$COMPOSE_PROJECT" ] && compose+=(-p "$COMPOSE_PROJECT")
  # Layout: the split stack has a Postgres service of its own; the all-in-one
  # container carries Postgres inside the gateway service (unix socket only).
  if [ "$LAYOUT" = auto ]; then
    if "${compose[@]}" config --services 2>/dev/null | grep -qx "$POSTGRES_SERVICE"; then
      LAYOUT="split"
    else
      LAYOUT="aio"
    fi
  fi
  case "$LAYOUT" in
    split) db_service="$POSTGRES_SERVICE"; exec_opts=(-T); POSTGRES_USER="${POSTGRES_USER:-ragmux}" ;;
    aio) db_service="$RAGMUX_SERVICE"; exec_opts=(-T -u postgres); POSTGRES_USER="${POSTGRES_USER:-postgres}" ;;
    *) die "LAYOUT must be auto, split or aio, got '$LAYOUT'" 2 ;;
  esac
  if ! "${compose[@]}" ps --status running --services 2>/dev/null | grep -qx "$db_service"; then
    die "compose service '$db_service' is not running (run from the directory holding docker-compose.yml, or set COMPOSE_FILE / -p)" 2
  fi
  mode="compose/$LAYOUT"
  dump_cmd() {
    "${compose[@]}" exec "${exec_opts[@]}" "$db_service" \
      pg_dump -Fc --no-owner --no-privileges -U "$POSTGRES_USER" -d "$POSTGRES_DB"
  }
  list_cmd() { "${compose[@]}" exec "${exec_opts[@]}" "$db_service" pg_restore --list >/dev/null <"$1"; }
fi
POSTGRES_USER="${POSTGRES_USER:-ragmux}"

install -d -m 700 "$BACKUP_DIR"
stamp="$(date '+%Y%m%d-%H%M%S')"
name="ragmux-$stamp"
out="$BACKUP_DIR/$name.dump"
tmp="$out.partial"
trap 'rm -f "$tmp"' EXIT

log "backup: mode=$mode db=$POSTGRES_DB -> $out"
if ! dump_cmd >"$tmp"; then
  die "pg_dump failed"
fi
[ -s "$tmp" ] || die "pg_dump produced an empty file"

if ! list_cmd "$tmp"; then
  die "verification failed: pg_restore --list could not read the dump"
fi
mv "$tmp" "$out"
trap - EXIT

if [ "$INCLUDE_SECRET_KEY" = "1" ]; then
  key="${SECRET_KEY:-}"
  if [ -z "$key" ] && [ -f .env ]; then
    key="$(sed -n 's/^[[:space:]]*SECRET_KEY=//p' .env | tail -n1 | tr -d '"'"'"' ' | tr -d '\r')"
  fi
  if [ -z "$key" ]; then
    log "warning: INCLUDE_SECRET_KEY=1 but SECRET_KEY is neither in the environment nor in .env; no key file written"
  else
    install -d -m 700 "$SECRET_KEY_DIR"
    keyfile="$SECRET_KEY_DIR/$name.key"
    printf '%s\n' "$key" >"$keyfile"
    chmod 600 "$keyfile"
    log "secret key written to $keyfile (mode 600)"
    if [ "$SECRET_KEY_DIR" = "$BACKUP_DIR" ]; then
      log "warning: the key sits next to the dump; set SECRET_KEY_DIR to keep them apart"
    fi
  fi
fi

size="$(wc -c <"$out" | tr -d ' ')"
log "done: $out ($size bytes)"

if [ "$KEEP_DAYS" -gt 0 ]; then
  removed=0
  while IFS= read -r f; do
    rm -f "$f"
    removed=$((removed + 1))
    log "rotated: $f"
  done < <(find "$BACKUP_DIR" "$SECRET_KEY_DIR" -maxdepth 1 -type f \( -name 'ragmux-*.dump' -o -name 'ragmux-*.key' \) -mtime +"$KEEP_DAYS" -print | sort -u)
  log "rotation: KEEP_DAYS=$KEEP_DAYS, removed $removed file(s)"
fi
printf '%s\n' "$out"
