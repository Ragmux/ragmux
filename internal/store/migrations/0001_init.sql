CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS model_connections (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    provider_type TEXT NOT NULL,
    base_url      TEXT NOT NULL DEFAULT '',
    api_key_enc   BYTEA,
    model_name    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rag_stores (
    id                      BIGSERIAL PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    embedding_connection_id BIGINT NOT NULL REFERENCES model_connections(id) ON DELETE RESTRICT,
    chunk_size              INT NOT NULL DEFAULT 1000,
    chunk_overlap           INT NOT NULL DEFAULT 200,
    top_k                   INT NOT NULL DEFAULT 5,
    dimensions              INT NOT NULL DEFAULT 0,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS documents (
    id           BIGSERIAL PRIMARY KEY,
    rag_store_id BIGINT NOT NULL REFERENCES rag_stores(id) ON DELETE CASCADE,
    filename     TEXT NOT NULL,
    mime         TEXT NOT NULL DEFAULT '',
    size_bytes   BIGINT NOT NULL DEFAULT 0,
    content      BYTEA NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending',
    error        TEXT NOT NULL DEFAULT '',
    chunk_count  INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_documents_store ON documents(rag_store_id);

CREATE TABLE IF NOT EXISTS chunks (
    id             BIGSERIAL PRIMARY KEY,
    document_id    BIGINT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    rag_store_id   BIGINT NOT NULL REFERENCES rag_stores(id) ON DELETE CASCADE,
    idx            INT NOT NULL,
    content        TEXT NOT NULL,
    token_estimate INT NOT NULL DEFAULT 0,
    metadata       JSONB NOT NULL DEFAULT '{}',
    tsv            tsvector GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED
);
CREATE INDEX IF NOT EXISTS idx_chunks_store ON chunks(rag_store_id);
CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(document_id);
CREATE INDEX IF NOT EXISTS idx_chunks_tsv ON chunks USING GIN (tsv);

CREATE TABLE IF NOT EXISTS projects (
    id                  BIGSERIAL PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    model_connection_id BIGINT NOT NULL REFERENCES model_connections(id) ON DELETE RESTRICT,
    rag_store_id        BIGINT REFERENCES rag_stores(id) ON DELETE SET NULL,
    api_key_hash        TEXT NOT NULL UNIQUE,
    api_key_prefix      TEXT NOT NULL,
    system_prompt       TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS request_logs (
    id                BIGSERIAL PRIMARY KEY,
    project_id        BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    model_name        TEXT NOT NULL DEFAULT '',
    status_code       INT NOT NULL,
    prompt_tokens     INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    estimated         BOOLEAN NOT NULL DEFAULT false,
    latency_ms        BIGINT NOT NULL DEFAULT 0,
    streamed          BOOLEAN NOT NULL DEFAULT false,
    rag_used          BOOLEAN NOT NULL DEFAULT false,
    error             TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_request_logs_project ON request_logs(project_id, created_at);
CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at);
