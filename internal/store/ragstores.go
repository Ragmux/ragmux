package store

import (
	"context"
	"fmt"
	"time"
)

// RAGStore is a named collection of documents sharing one embedding model.
type RAGStore struct {
	ID                    int64  `json:"id"`
	Name                  string `json:"name"`
	EmbeddingConnectionID int64  `json:"embedding_connection_id"`
	ChunkSize             int    `json:"chunk_size"`
	ChunkOverlap          int    `json:"chunk_overlap"`
	TopK                  int    `json:"top_k"`
	Dimensions            int    `json:"dimensions"`
	DocumentCount         int    `json:"document_count"`
	ChunkCount            int    `json:"chunk_count"`
	CreatedAt             string `json:"created_at"`
}

const ragCols = `r.id, r.name, r.embedding_connection_id, r.chunk_size, r.chunk_overlap, r.top_k, r.dimensions, r.created_at,
	(SELECT COUNT(*) FROM documents d WHERE d.rag_store_id = r.id),
	(SELECT COUNT(*) FROM chunks c WHERE c.rag_store_id = r.id)`

func scanRAG(row interface{ Scan(...any) error }) (*RAGStore, error) {
	r := &RAGStore{}
	var created time.Time
	err := row.Scan(&r.ID, &r.Name, &r.EmbeddingConnectionID, &r.ChunkSize, &r.ChunkOverlap, &r.TopK,
		&r.Dimensions, &created, &r.DocumentCount, &r.ChunkCount)
	if err != nil {
		return nil, scanErr(err)
	}
	r.CreatedAt = ts(created)
	return r, nil
}

// CreateRAGStore inserts a store.
func (s *Store) CreateRAGStore(ctx context.Context, r *RAGStore) (*RAGStore, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO rag_stores
		(name, embedding_connection_id, chunk_size, chunk_overlap, top_k) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetRAGStore(ctx, id)
}

// UpdateRAGStore changes name/chunking/top_k. The embedding connection is
// immutable once vectors exist because dimensions would no longer match.
func (s *Store) UpdateRAGStore(ctx context.Context, r *RAGStore) (*RAGStore, error) {
	cur, err := s.GetRAGStore(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	if cur.EmbeddingConnectionID != r.EmbeddingConnectionID && cur.ChunkCount > 0 {
		return nil, fmt.Errorf("cannot change embedding connection while store has %d chunks", cur.ChunkCount)
	}
	_, err = s.pool.Exec(ctx, `UPDATE rag_stores SET name=$1, embedding_connection_id=$2, chunk_size=$3,
		chunk_overlap=$4, top_k=$5 WHERE id=$6`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK, r.ID)
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
