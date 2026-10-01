package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Search modes for a RAG store.
const (
	SearchVector = "vector"
	SearchHybrid = "hybrid"
)

// Rerank backends for a RAG store. RerankLLM is the project's own chat model
// (the original behaviour); the others are dedicated rerank APIs reached
// through a model_connections row named by RerankConnectionID.
const (
	RerankLLM    = "llm"
	RerankCohere = "cohere"
	RerankVoyage = "voyage"
)

// RerankBackends lists the known rerank backends in a stable order.
var RerankBackends = []string{RerankLLM, RerankCohere, RerankVoyage}

// IsValidRerankBackend reports whether name is a known rerank backend.
func IsValidRerankBackend(name string) bool {
	for _, v := range RerankBackends {
		if v == name {
			return true
		}
	}
	return false
}

// RerankConnectionType maps a rerank backend onto the provider_type its
// model connection must carry. The empty string means the backend needs no
// connection.
func RerankConnectionType(backend string) string {
	switch backend {
	case RerankCohere:
		return "cohere_rerank"
	case RerankVoyage:
		return "voyage_rerank"
	}
	return ""
}

// RAGStore is a named collection of documents sharing one embedding model.
type RAGStore struct {
	ID                    int64  `json:"id"`
	Name                  string `json:"name"`
	EmbeddingConnectionID int64  `json:"embedding_connection_id"`
	ChunkSize             int    `json:"chunk_size"`
	ChunkOverlap          int    `json:"chunk_overlap"`
	TopK                  int    `json:"top_k"`
	// SearchMode is "vector" or "hybrid" (vector + full-text fused with RRF).
	SearchMode string `json:"search_mode"`
	// SearchBackend answers the lexical half of a hybrid search:
	// BackendPgvector (tsvector + GIN) or BackendPgSearch (ParadeDB BM25).
	// Switching it needs no reprocessing: both read chunks.content.
	SearchBackend string `json:"search_backend"`
	// FTSConfig is the text search configuration used to parse queries.
	// Chunks are always indexed with 'simple'. Ignored by the pg_search
	// backend, whose index is global and tokenised once at index time.
	FTSConfig string `json:"fts_config"`
	// Rerank enables reranking of the top RerankCandidates hits.
	Rerank           bool `json:"rerank"`
	RerankCandidates int  `json:"rerank_candidates"`
	// RerankBackend is RerankLLM, RerankCohere or RerankVoyage.
	RerankBackend string `json:"rerank_backend"`
	// RerankConnectionID is the model connection holding the rerank API's
	// credentials; nil for RerankLLM. A NULL behind an API backend (the
	// connection was deleted, ON DELETE SET NULL) means the reranker is
	// unavailable: the search skips it and logs, as it does for a failure.
	RerankConnectionID *int64 `json:"rerank_connection_id"`
	// MaxDistance drops hits whose cosine distance exceeds it; 0 disables.
	MaxDistance float64 `json:"max_distance"`
	// ContextualChunks prefixes the document title and section to the text
	// that is embedded (not to the stored content).
	ContextualChunks bool `json:"contextual_chunks"`
	// MaxDocuments and MaxBytes cap what editors may upload into the store;
	// 0 means unlimited. Instance-wide ceilings apply on top (see admin).
	MaxDocuments int   `json:"max_documents"`
	MaxBytes     int64 `json:"max_bytes"`
	Dimensions   int   `json:"dimensions"`
	// DocumentCount and BytesUsed are the current usage the quotas are
	// checked against (documents in any status, sum of their size_bytes).
	DocumentCount int    `json:"document_count"`
	BytesUsed     int64  `json:"bytes_used"`
	ChunkCount    int    `json:"chunk_count"`
	CreatedAt     string `json:"created_at"`
}

const ragCols = `r.id, r.name, r.embedding_connection_id, r.chunk_size, r.chunk_overlap, r.top_k,
	r.search_mode, r.search_backend, r.fts_config, r.rerank, r.rerank_candidates,
	r.rerank_backend, r.rerank_connection_id, r.max_distance, r.contextual_chunks,
	r.max_documents, r.max_bytes, r.dimensions, r.created_at,
	(SELECT COUNT(*) FROM documents d WHERE d.rag_store_id = r.id),
	COALESCE((SELECT SUM(d.size_bytes) FROM documents d WHERE d.rag_store_id = r.id), 0),
	(SELECT COUNT(*) FROM chunks c WHERE c.rag_store_id = r.id)`

func scanRAG(row interface{ Scan(...any) error }) (*RAGStore, error) {
	r := &RAGStore{}
	var created time.Time
	var maxDist float32
	err := row.Scan(&r.ID, &r.Name, &r.EmbeddingConnectionID, &r.ChunkSize, &r.ChunkOverlap, &r.TopK,
		&r.SearchMode, &r.SearchBackend, &r.FTSConfig, &r.Rerank, &r.RerankCandidates,
		&r.RerankBackend, &r.RerankConnectionID, &maxDist, &r.ContextualChunks,
		&r.MaxDocuments, &r.MaxBytes, &r.Dimensions, &created, &r.DocumentCount, &r.BytesUsed, &r.ChunkCount)
	if err != nil {
		return nil, scanErr(err)
	}
	r.MaxDistance = float64(maxDist)
	r.CreatedAt = ts(created)
	return r, nil
}

// applyRAGDefaults fills zero-valued retrieval settings so callers that only
// know the pre-0.2 fields keep working.
func applyRAGDefaults(r *RAGStore) {
	if r.SearchMode == "" {
		r.SearchMode = SearchHybrid
	}
	if r.SearchBackend == "" {
		r.SearchBackend = BackendPgvector
	}
	if r.FTSConfig == "" {
		r.FTSConfig = "simple"
	}
	if r.RerankBackend == "" {
		r.RerankBackend = RerankLLM
	}
	if r.RerankBackend == RerankLLM {
		// The connection only ever carries rerank API credentials; leaving
		// one on an LLM store would resurrect it on a later backend switch.
		r.RerankConnectionID = nil
	}
	if r.RerankCandidates <= 0 {
		r.RerankCandidates = 15
	}
}

// CreateRAGStore inserts a store.
func (s *Store) CreateRAGStore(ctx context.Context, r *RAGStore) (*RAGStore, error) {
	applyRAGDefaults(r)
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO rag_stores
		(name, embedding_connection_id, chunk_size, chunk_overlap, top_k,
		 search_mode, search_backend, fts_config, rerank, rerank_candidates,
		 rerank_backend, rerank_connection_id, max_distance, contextual_chunks,
		 max_documents, max_bytes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING id`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK,
		r.SearchMode, r.SearchBackend, r.FTSConfig, r.Rerank, r.RerankCandidates,
		r.RerankBackend, r.RerankConnectionID, float32(r.MaxDistance), r.ContextualChunks,
		r.MaxDocuments, r.MaxBytes).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetRAGStore(ctx, id)
}

// UpdateRAGStore changes name, chunking and retrieval settings. The embedding
// connection is immutable once vectors exist because dimensions would no
// longer match.
//
// Changing the connection of an empty store resets its dimension to 0 (not
// known): the next ingest binds it to the new model's width, and an
// in-flight ingest of the old model stops with ErrEmbeddingConnectionChanged.
// The row is locked for the check and the write, the same lock
// ReplaceDocumentChunksFrom takes, so the two cannot interleave. The lock is
// FOR NO KEY UPDATE: it conflicts with itself but not with the FOR KEY SHARE
// that foreign-key inserts (documents, projects) take on the row.
func (s *Store) UpdateRAGStore(ctx context.Context, r *RAGStore) (*RAGStore, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	var curConn int64
	var curDims, chunkCount int
	if err := tx.QueryRow(ctx, `SELECT embedding_connection_id, dimensions FROM rag_stores WHERE id = $1 FOR NO KEY UPDATE`,
		r.ID).Scan(&curConn, &curDims); err != nil {
		return nil, scanErr(err)
	}
	connChanged := curConn != r.EmbeddingConnectionID
	if connChanged {
		if err := tx.QueryRow(ctx, "SELECT COUNT(*) FROM chunks WHERE rag_store_id = $1", r.ID).Scan(&chunkCount); err != nil {
			return nil, err
		}
		if chunkCount > 0 {
			return nil, fmt.Errorf("cannot change embedding connection while store has %d chunks", chunkCount)
		}
	}
	applyRAGDefaults(r)
	_, err = tx.Exec(ctx, `UPDATE rag_stores SET name=$1, embedding_connection_id=$2, chunk_size=$3,
		chunk_overlap=$4, top_k=$5, search_mode=$6, search_backend=$7, fts_config=$8, rerank=$9,
		rerank_candidates=$10, rerank_backend=$11, rerank_connection_id=$12,
		max_distance=$13, contextual_chunks=$14, max_documents=$15, max_bytes=$16,
		dimensions = CASE WHEN $18 THEN 0 ELSE dimensions END WHERE id=$17`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK,
		r.SearchMode, r.SearchBackend, r.FTSConfig, r.Rerank, r.RerankCandidates,
		r.RerankBackend, r.RerankConnectionID, float32(r.MaxDistance), r.ContextualChunks,
		r.MaxDocuments, r.MaxBytes, r.ID, connChanged)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if connChanged && curDims > MaxIndexedVectorDims {
		// The store no longer carries the unindexable width.
		s.recountIndexMissing(ctx)
	}
	return s.GetRAGStore(ctx, r.ID)
}

// GetRAGStore fetches one store.
func (s *Store) GetRAGStore(ctx context.Context, id int64) (*RAGStore, error) {
	return scanRAG(s.pool.QueryRow(ctx, "SELECT "+ragCols+" FROM rag_stores r WHERE r.id = $1", id))
}

// ListRAGStores lists all stores.
func (s *Store) ListRAGStores(ctx context.Context) ([]*RAGStore, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+ragCols+" FROM rag_stores r ORDER BY r.name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RAGStore{}
	for rows.Next() {
		r, err := scanRAG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRAGStore removes a store; documents, chunks and embeddings cascade.
func (s *Store) DeleteRAGStore(ctx context.Context, id int64) error {
	res, err := s.pool.Exec(ctx, "DELETE FROM rag_stores WHERE id = $1", id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.recountIndexMissing(ctx)
	return nil
}

// StoreUsage returns how many documents a store holds (in any status) and
// the sum of their uploaded sizes; the quota check in uploads runs on it.
func (s *Store) StoreUsage(ctx context.Context, storeID int64) (docs int, bytes int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(size_bytes), 0)
		FROM documents WHERE rag_store_id = $1`, storeID).Scan(&docs, &bytes)
	return docs, bytes, err
}

// FTSConfigWarning returns the operator-facing warning for a store whose
// fts_config is not 'simple', or "" when there is nothing to warn about.
// Chunks are always indexed with 'simple', so a stemming configuration
// parses queries into stems the index never holds and silently loses
// matches on the pgvector backend. Behaviour is unchanged; the permanent
// fix (a per-store language for indexing) is planned for v0.6-9.
func FTSConfigWarning(ftsConfig, backend string) string {
	cfg := strings.ToLower(strings.TrimSpace(ftsConfig))
	if cfg == "" || cfg == "simple" || backend == BackendPgSearch {
		return ""
	}
	return "fts_config '" + cfg + "' only affects query parsing: chunks are indexed with 'simple', " +
		"so stemmed query terms can miss matches in hybrid search; use 'simple' until per-store " +
		"indexing language lands in v0.6-9"
}

// RAGStoreAudit is what AuditRAGStores found.
type RAGStoreAudit struct {
	// IndexMissing counts stores whose embedding width is above
	// MaxIndexedVectorDims: their vectors have no HNSW index.
	IndexMissing int
	// FTSMismatch counts stores whose fts_config is not 'simple'.
	FTSMismatch int
}

// AuditRAGStores logs a warning for every store the current schema cannot
// serve as configured: a width above MaxIndexedVectorDims (searched with an
// exact scan, no HNSW index) and an fts_config other than 'simple'. Neither
// store is changed or disabled. The index-missing count is kept for
// RAGIndexMissing. It runs at startup.
func (s *Store) AuditRAGStores(ctx context.Context) (RAGStoreAudit, error) {
	var out RAGStoreAudit
	stores, err := s.ListRAGStores(ctx)
	if err != nil {
		return out, err
	}
	for _, r := range stores {
		if r.Dimensions > MaxIndexedVectorDims {
			out.IndexMissing++
			s.log.Warn("rag store embedding is wider than pgvector can index; it keeps working with an exact "+
				"scan but has no HNSW index -- move it to a model of 2000 dimensions or fewer, or wait for "+
				"halfvec support in v0.6",
				"store_id", r.ID, "store", r.Name, "dimensions", r.Dimensions, "max_indexed", MaxIndexedVectorDims)
		}
		if msg := FTSConfigWarning(r.FTSConfig, r.SearchBackend); msg != "" {
			out.FTSMismatch++
			s.log.Warn("rag store: "+msg, "store_id", r.ID, "store", r.Name, "fts_config", r.FTSConfig)
		}
	}
	s.indexMissing.Store(int64(out.IndexMissing))
	return out, nil
}

// RAGIndexMissing is the number of stores whose embeddings have no HNSW
// index, as counted at startup and recounted whenever such a store is
// deleted or moves to another embedding connection. It reads no database.
func (s *Store) RAGIndexMissing() int64 { return s.indexMissing.Load() }

// recountIndexMissing refreshes RAGIndexMissing from the table. It is a
// recount rather than a decrement so the gauge cannot drift (or go below
// zero) when the startup audit failed. A failed recount keeps the old value.
func (s *Store) recountIndexMissing(ctx context.Context) {
	var n int64
	if err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM rag_stores WHERE dimensions > $1",
		MaxIndexedVectorDims).Scan(&n); err != nil {
		s.log.Warn("rag store: recount of stores without an HNSW index failed", "err", err)
		return
	}
	s.indexMissing.Store(n)
}

// TextSearchConfigExists reports whether name is a text search configuration
// known to the server (e.g. simple, english, turkish).
func (s *Store) TextSearchConfigExists(ctx context.Context, name string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_ts_config WHERE cfgname = $1)", name).Scan(&ok)
	return ok, err
}

// ResetStoreDocuments marks every document of a store pending and returns
// their ids so the caller can enqueue them for reprocessing.
func (s *Store) ResetStoreDocuments(ctx context.Context, storeID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `UPDATE documents SET status=$1, error='', updated_at=now()
		WHERE rag_store_id = $2 RETURNING id`, DocPending, storeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
