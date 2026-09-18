package store

import (
	"context"
	"fmt"
	"time"
)

// Search modes for a RAG store.
const (
	SearchVector = "vector"
	SearchHybrid = "hybrid"
)

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
	// FTSConfig is the text search configuration used to parse queries.
	// Chunks are always indexed with 'simple'.
	FTSConfig string `json:"fts_config"`
	// Rerank enables LLM reranking of the top RerankCandidates hits.
	Rerank           bool `json:"rerank"`
	RerankCandidates int  `json:"rerank_candidates"`
	// MaxDistance drops hits whose cosine distance exceeds it; 0 disables.
	MaxDistance float64 `json:"max_distance"`
	// ContextualChunks prefixes the document title and section to the text
	// that is embedded (not to the stored content).
	ContextualChunks bool   `json:"contextual_chunks"`
	Dimensions       int    `json:"dimensions"`
	DocumentCount    int    `json:"document_count"`
	ChunkCount       int    `json:"chunk_count"`
	CreatedAt        string `json:"created_at"`
}

const ragCols = `r.id, r.name, r.embedding_connection_id, r.chunk_size, r.chunk_overlap, r.top_k,
	r.search_mode, r.fts_config, r.rerank, r.rerank_candidates, r.max_distance, r.contextual_chunks,
	r.dimensions, r.created_at,
	(SELECT COUNT(*) FROM documents d WHERE d.rag_store_id = r.id),
	(SELECT COUNT(*) FROM chunks c WHERE c.rag_store_id = r.id)`

func scanRAG(row interface{ Scan(...any) error }) (*RAGStore, error) {
	r := &RAGStore{}
	var created time.Time
	var maxDist float32
	err := row.Scan(&r.ID, &r.Name, &r.EmbeddingConnectionID, &r.ChunkSize, &r.ChunkOverlap, &r.TopK,
		&r.SearchMode, &r.FTSConfig, &r.Rerank, &r.RerankCandidates, &maxDist, &r.ContextualChunks,
		&r.Dimensions, &created, &r.DocumentCount, &r.ChunkCount)
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
	if r.FTSConfig == "" {
		r.FTSConfig = "simple"
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
		 search_mode, fts_config, rerank, rerank_candidates, max_distance, contextual_chunks)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK,
		r.SearchMode, r.FTSConfig, r.Rerank, r.RerankCandidates, float32(r.MaxDistance), r.ContextualChunks).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetRAGStore(ctx, id)
}

// UpdateRAGStore changes name, chunking and retrieval settings. The embedding
// connection is immutable once vectors exist because dimensions would no
// longer match.
func (s *Store) UpdateRAGStore(ctx context.Context, r *RAGStore) (*RAGStore, error) {
	cur, err := s.GetRAGStore(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	if cur.EmbeddingConnectionID != r.EmbeddingConnectionID && cur.ChunkCount > 0 {
		return nil, fmt.Errorf("cannot change embedding connection while store has %d chunks", cur.ChunkCount)
	}
	applyRAGDefaults(r)
	_, err = s.pool.Exec(ctx, `UPDATE rag_stores SET name=$1, embedding_connection_id=$2, chunk_size=$3,
		chunk_overlap=$4, top_k=$5, search_mode=$6, fts_config=$7, rerank=$8, rerank_candidates=$9,
		max_distance=$10, contextual_chunks=$11 WHERE id=$12`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK,
		r.SearchMode, r.FTSConfig, r.Rerank, r.RerankCandidates, float32(r.MaxDistance), r.ContextualChunks, r.ID)
	if err != nil {
		return nil, err
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
	return nil
}

// SetRAGStoreDimensions records the embedding width once it is known.
func (s *Store) SetRAGStoreDimensions(ctx context.Context, id int64, dims int) error {
	_, err := s.pool.Exec(ctx, "UPDATE rag_stores SET dimensions = $1 WHERE id = $2 AND dimensions = 0", dims, id)
	return err
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
