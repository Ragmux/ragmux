-- Optional search extensions, idempotent and safe on any server.
--
-- This is the same DO block 01-ragmux.sql carries, on its own so it can be
-- re-run against a database that already exists:
--
--   psql -v ON_ERROR_STOP=1 -U postgres -d ragmux \
--        -f docker/postgres-init/02-extensions.sql
--
-- The all-in-one image runs 01-ragmux.sql on *every* start (see
-- docker/aio/entrypoint.sh), so upgrading an AIO deployment from the plain
-- PostgreSQL image to the ParadeDB one is a plain image swap: the extension
-- appears on the next boot and the gateway's capability detection picks it
-- up. The split layout's Postgres service only runs its init hooks on an
-- empty data volume, so an existing split deployment swapping the image runs
-- this file by hand, once.
--
-- Nothing here is required. A server without pg_search keeps working exactly
-- as before; rag stores configured for it fall back to the pgvector backend
-- with one warning per store (see docs/rag.md#search-backends).
DO $$
BEGIN
    -- pg_available_extensions lists what the server *could* install, so this
    -- is a no-op rather than an error on a plain PostgreSQL image.
    IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'pg_search') THEN
        CREATE EXTENSION IF NOT EXISTS pg_search;
        -- Ragmux always schema-qualifies its ParadeDB calls
        -- (paradedb.term, paradedb.match, paradedb.score), so search_path
        -- stays "public" for the application role and USAGE is all it needs.
        -- The role is absent in a development database, where tests connect
        -- as the superuser; the grant is then simply not needed.
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ragmux_app') THEN
            EXECUTE 'GRANT USAGE ON SCHEMA paradedb TO ragmux_app';
        END IF;
        RAISE NOTICE 'pg_search enabled; the pg_search search backend is available';
    END IF;
END
$$;
