#!/usr/bin/env bash
# First-start hook of the split stack's Postgres service (docker-compose.split.yml mounts
# it into /docker-entrypoint-initdb.d, the official image runs it once on an
# empty data volume as the superuser POSTGRES_USER). It creates the vector
# extension and the least-privilege role ragmux_app the gateway connects as.
#
# The SQL lives in 01-ragmux.sql next to this file (mounted at
# /ragmux-init/01-ragmux.sql so the entrypoint does not run it a second time
# without the password variable).
set -euo pipefail

: "${RAGMUX_DB_PASSWORD:?Set RAGMUX_DB_PASSWORD (password of the ragmux_app role)}"
sql="${RAGMUX_INIT_SQL:-/ragmux-init/01-ragmux.sql}"
[ -r "$sql" ] || { echo "01-ragmux.sh: $sql not found" >&2; exit 1; }

# psql quotes :'pw' as a literal and :"db" as an identifier, so neither value
# needs escaping here. --no-password: the init server only listens on the
# unix socket with trust authentication.
psql -v ON_ERROR_STOP=1 --no-password \
    --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
    -v pw="$RAGMUX_DB_PASSWORD" -v db="$POSTGRES_DB" \
    -f "$sql"
echo "01-ragmux.sh: role ragmux_app ready"
