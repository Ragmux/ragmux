package store

import (
	"context"
	"fmt"
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
	err := row.Scan(&r.ID, &r.Name, &r.EmbeddingConnectionID, &r.ChunkSize, &r.ChunkOverlap, &r.TopK,
		&r.Dimensions, &r.CreatedAt, &r.DocumentCount, &r.ChunkCount)
	if err != nil {
		return nil, scanErr(err)
	}
	return r, nil
}

// CreateRAGStore inserts a store.
func (s *Store) CreateRAGStore(ctx context.Context, r *RAGStore) (*RAGStore, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO rag_stores
		(name, embedding_connection_id, chunk_size, chunk_overlap, top_k) VALUES (?, ?, ?, ?, ?)`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
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
	_, err = s.db.ExecContext(ctx, `UPDATE rag_stores SET name=?, embedding_connection_id=?, chunk_size=?,
		chunk_overlap=?, top_k=? WHERE id=?`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK, r.ID)
	if err != nil {
		return nil, err
	}
	return s.GetRAGStore(ctx, r.ID)
}

// GetRAGStore fetches one store.
func (s *Store) GetRAGStore(ctx context.Context, id int64) (*RAGStore, error) {
	return scanRAG(s.db.QueryRowContext(ctx, "SELECT "+ragCols+" FROM rag_stores r WHERE r.id = ?", id))
}

// ListRAGStores lists all stores.
func (s *Store) ListRAGStores(ctx context.Context) ([]*RAGStore, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+ragCols+" FROM rag_stores r ORDER BY r.name")
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

// DeleteRAGStore removes a store, its documents, chunks and vectors.
func (s *Store) DeleteRAGStore(ctx context.Context, id int64) error {
	r, err := s.GetRAGStore(ctx, id)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.VecAvailable && r.Dimensions > 0 {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE rag_store_id = ?", vecTable(r.Dimensions)), id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM rag_stores WHERE id = ?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetRAGStoreDimensions records the embedding width once it is known.
func (s *Store) SetRAGStoreDimensions(ctx context.Context, id int64, dims int) error {
	_, err := s.db.ExecContext(ctx, "UPDATE rag_stores SET dimensions = ? WHERE id = ? AND dimensions = 0", dims, id)
	return err
}
