package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// Chunk is a slice of a document together with its embedding.
type Chunk struct {
	ID            int64     `json:"id"`
	DocumentID    int64     `json:"document_id"`
	RAGStoreID    int64     `json:"rag_store_id"`
	Index         int       `json:"index"`
	Content       string    `json:"content"`
	TokenEstimate int       `json:"token_estimate"`
	Embedding     []float32 `json:"-"`
}

// SearchHit is one nearest-neighbour result.
type SearchHit struct {
	ChunkID    int64   `json:"chunk_id"`
	DocumentID int64   `json:"document_id"`
	Filename   string  `json:"filename"`
	Index      int     `json:"index"`
	Content    string  `json:"content"`
	Distance   float64 `json:"distance"`
}

func vecTable(dims int) string { return fmt.Sprintf("vec_chunks_%d", dims) }

// EncodeVector serialises float32s as little-endian bytes (sqlite-vec format).
func EncodeVector(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(f))
	}
	return buf
}

// DecodeVector reverses EncodeVector.
func DecodeVector(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

// ensureVecTable creates the vec0 virtual table for a dimension if needed.
func (s *Store) ensureVecTable(ctx context.Context, dims int) error {
	if !s.VecAvailable {
		return nil
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING vec0(
			chunk_id INTEGER PRIMARY KEY,
			rag_store_id INTEGER,
			embedding float[%d] distance_metric=cosine
		)`, vecTable(dims), dims))
	return err
}

// ReplaceDocumentChunks atomically swaps a document's chunks and vectors and
// marks it ready. All chunks must share the store's embedding width.
func (s *Store) ReplaceDocumentChunks(ctx context.Context, doc *Document, chunks []*Chunk) error {
	if len(chunks) == 0 {
		return fmt.Errorf("no chunks to store")
	}
	dims := len(chunks[0].Embedding)
	if dims == 0 {
		return fmt.Errorf("chunks have no embeddings")
	}
	for _, c := range chunks {
		if len(c.Embedding) != dims {
			return fmt.Errorf("inconsistent embedding dimensions (%d vs %d)", len(c.Embedding), dims)
		}
	}
	if err := s.SetRAGStoreDimensions(ctx, doc.RAGStoreID, dims); err != nil {
		return err
	}
	r, err := s.GetRAGStore(ctx, doc.RAGStoreID)
	if err != nil {
		return err
	}
	if r.Dimensions != dims {
		return fmt.Errorf("embedding dimension %d does not match store dimension %d", dims, r.Dimensions)
	}
	if err := s.ensureVecTable(ctx, dims); err != nil {
		return fmt.Errorf("create vec table: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if s.VecAvailable {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			"DELETE FROM %s WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)", vecTable(dims)), doc.ID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM chunks WHERE document_id = ?", doc.ID); err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO chunks (document_id, rag_store_id, idx, content, token_estimate, embedding)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, c := range chunks {
		blob := EncodeVector(c.Embedding)
		res, err := ins.ExecContext(ctx, doc.ID, doc.RAGStoreID, c.Index, c.Content, c.TokenEstimate, blob)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		c.ID = id
		if s.VecAvailable {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(
				"INSERT INTO %s (chunk_id, rag_store_id, embedding) VALUES (?, ?, ?)", vecTable(dims)),
				id, doc.RAGStoreID, blob); err != nil {
				return fmt.Errorf("insert vector: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE documents SET status=?, error='', chunk_count=?, updated_at=? WHERE id=?",
		DocReady, len(chunks), now(), doc.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// SearchTopK returns the k nearest chunks in a store for a query vector.
func (s *Store) SearchTopK(ctx context.Context, storeID int64, query []float32, k int) ([]SearchHit, error) {
	if k <= 0 {
		k = 5
	}
	r, err := s.GetRAGStore(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if r.Dimensions == 0 || r.ChunkCount == 0 {
		return []SearchHit{}, nil
	}
	if len(query) != r.Dimensions {
		return nil, fmt.Errorf("query vector has %d dimensions, store expects %d", len(query), r.Dimensions)
	}
	if s.VecAvailable {
		return s.searchVec(ctx, r, query, k)
	}
	return s.searchBruteForce(ctx, r, query, k)
}

func (s *Store) searchVec(ctx context.Context, r *RAGStore, query []float32, k int) ([]SearchHit, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT v.chunk_id, v.distance, c.document_id, c.idx, c.content, d.filename
		FROM %s v
		JOIN chunks c ON c.id = v.chunk_id
		JOIN documents d ON d.id = c.document_id
		WHERE v.embedding MATCH ? AND v.rag_store_id = ? AND k = ?
		ORDER BY v.distance`, vecTable(r.Dimensions)), EncodeVector(query), r.ID, k)
	if err != nil {
		return nil, fmt.Errorf("vector search: %w", err)
	}
	defer rows.Close()
	hits := []SearchHit{}
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ChunkID, &h.Distance, &h.DocumentID, &h.Index, &h.Content, &h.Filename); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (s *Store) searchBruteForce(ctx context.Context, r *RAGStore, query []float32, k int) ([]SearchHit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.document_id, c.idx, c.content, c.embedding, d.filename
		FROM chunks c JOIN documents d ON d.id = c.document_id
		WHERE c.rag_store_id = ? AND c.embedding IS NOT NULL`, r.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var blob []byte
		if err := rows.Scan(&h.ChunkID, &h.DocumentID, &h.Index, &h.Content, &blob, &h.Filename); err != nil {
			return nil, err
		}
		emb := DecodeVector(blob)
		if len(emb) != len(query) {
			continue
		}
		h.Distance = CosineDistance(query, emb)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Distance < hits[j].Distance })
	if len(hits) > k {
		hits = hits[:k]
	}
	if hits == nil {
		hits = []SearchHit{}
	}
	return hits, nil
}

// CosineDistance returns 1 - cosine similarity, matching sqlite-vec's metric.
func CosineDistance(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}
