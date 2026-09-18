CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS sessions (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS model_connections (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    provider_type TEXT NOT NULL,
    base_url      TEXT NOT NULL DEFAULT '',
    api_key_enc   BLOB,
    model_name    TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS rag_stores (
    id                      INTEGER PRIMARY KEY,
    name                    TEXT NOT NULL UNIQUE,
    embedding_connection_id INTEGER NOT NULL REFERENCES model_connections(id) ON DELETE RESTRICT,
    chunk_size              INTEGER NOT NULL DEFAULT 1000,
    chunk_overlap           INTEGER NOT NULL DEFAULT 200,
    top_k                   INTEGER NOT NULL DEFAULT 5,
    dimensions              INTEGER NOT NULL DEFAULT 0,
    created_at              TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS documents (
    id           INTEGER PRIMARY KEY,
    rag_store_id INTEGER NOT NULL REFERENCES rag_stores(id) ON DELETE CASCADE,
    filename     TEXT NOT NULL,
    mime         TEXT NOT NULL DEFAULT '',
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'pending',
    error        TEXT NOT NULL DEFAULT '',
    chunk_count  INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_documents_store ON documents(rag_store_id);

CREATE TABLE IF NOT EXISTS chunks (
    id             INTEGER PRIMARY KEY,
    document_id    INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    rag_store_id   INTEGER NOT NULL REFERENCES rag_stores(id) ON DELETE CASCADE,
    idx            INTEGER NOT NULL,
    content        TEXT NOT NULL,
    token_estimate INTEGER NOT NULL DEFAULT 0,
    embedding      BLOB
);
CREATE INDEX IF NOT EXISTS idx_chunks_store ON chunks(rag_store_id);
CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(document_id);

CREATE TABLE IF NOT EXISTS projects (
    id                  INTEGER PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    model_connection_id INTEGER NOT NULL REFERENCES model_connections(id) ON DELETE RESTRICT,
    rag_store_id        INTEGER REFERENCES rag_stores(id) ON DELETE SET NULL,
    api_key_hash        TEXT NOT NULL UNIQUE,
    api_key_prefix      TEXT NOT NULL,
    system_prompt       TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS request_logs (
    id                INTEGER PRIMARY KEY,
    project_id        INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    model_name        TEXT NOT NULL DEFAULT '',
    status_code       INTEGER NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    estimated         INTEGER NOT NULL DEFAULT 0,
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    streamed          INTEGER NOT NULL DEFAULT 0,
    rag_used          INTEGER NOT NULL DEFAULT 0,
    error             TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_request_logs_project ON request_logs(project_id, created_at);
CREATE INDEX IF NOT EXISTS idx_request_logs_created ON request_logs(created_at);
