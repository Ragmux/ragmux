-- Least-privilege application role for the Ragmux database.
--
-- Run once per database as a superuser. Both bundled layouts use this one
-- file: the split stack (docker-compose.split.yml) runs it through
-- 01-ragmux.sh on the first start of an empty data volume with the role's
-- password in the psql variable "pw"; the all-in-one image
-- (docker/aio/entrypoint.sh) runs it without "pw", because there the role
-- logs in over the container-local unix socket with trust authentication and
-- no password exists. Against an existing or external server run it by hand:
--
--   psql -v ON_ERROR_STOP=1 -v pw="$RAGMUX_DB_PASSWORD" -v db=ragmux \
--        -U postgres -d ragmux -f docker/postgres-init/01-ragmux.sql
--
-- :'pw' and :"db" are psql variables; psql quotes them as a string literal
-- and an identifier, so the password may contain any character. The
-- gateway then connects as ragmux_app (see DATABASE_URL in the Compose
-- files). The role may create tables in "public" (migrations and the
-- per-dimension chunk_embeddings_<dims> tables) and nothing else: no
-- superuser, no CREATEDB, no CREATEROLE, and it cannot create extensions.

-- The extension needs a superuser; the gateway only checks that it exists.
CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;

-- Optional: ParadeDB's BM25 index, used by the pg_search search backend.
-- Guarded on pg_available_extensions so this file stays the same on every
-- image; a plain PostgreSQL simply skips it and every rag store keeps using
-- the pgvector backend. 02-extensions.sql carries the identical block so an
-- existing database can be upgraded without re-running this whole file.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'pg_search') THEN
        CREATE EXTENSION IF NOT EXISTS pg_search;
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ragmux_app') THEN
        CREATE ROLE ragmux_app;
    END IF;
END
$$;

ALTER ROLE ragmux_app WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS;
-- The password is only set when the caller passed one (-v pw=...); the
-- all-in-one image authenticates over the unix socket and skips it.
\if :{?pw}
ALTER ROLE ragmux_app WITH PASSWORD :'pw';
\endif
ALTER ROLE ragmux_app SET search_path = public;

GRANT CONNECT, TEMPORARY ON DATABASE :"db" TO ragmux_app;
-- CREATE on the schema: migrations create the tables the role then owns, and
-- ingestion creates chunk_embeddings_<dims> when a new embedding width appears.
GRANT CREATE, USAGE ON SCHEMA public TO ragmux_app;

-- ParadeDB installs its functions into the "paradedb" schema. Ragmux always
-- schema-qualifies them (paradedb.term, paradedb.match, paradedb.score), so
-- the role's search_path stays "public" and USAGE is the only grant needed.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'paradedb') THEN
        EXECUTE 'GRANT USAGE ON SCHEMA paradedb TO ragmux_app';
    END IF;
END
$$;

-- Tables that already exist (an existing deployment switching from the
-- superuser URL, or a pg_restore run as the superuser) must be handed over,
-- otherwise the next migration's ALTER TABLE fails for lack of ownership.
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN SELECT tablename AS name FROM pg_tables WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER TABLE public.%I OWNER TO ragmux_app', r.name);
    END LOOP;
    FOR r IN SELECT sequencename AS name FROM pg_sequences WHERE schemaname = 'public' LOOP
        EXECUTE format('ALTER SEQUENCE public.%I OWNER TO ragmux_app', r.name);
    END LOOP;
END
$$;
