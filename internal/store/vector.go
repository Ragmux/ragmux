package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
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

// vecTable names the embedding table for one vector width. Embeddings of
// different models have different dimensions and pgvector needs a fixed
// width per column, hence one table per dimension.
func vecTable(dims int) string { return fmt.Sprintf("chunk_embeddings_%d", dims) }

// ensureVecTable creates the embedding table and its indexes for a dimension
// if they do not exist yet. DDL is serialised with an advisory lock so
// concurrent ingesters on several replicas do not race.
func (s *Store) ensureVecTable(ctx context.Context, dims int) error {
	if _, ok := s.vecTables.Load(dims); ok {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", lockVecTables, int32(dims)); err != nil {
		return err
	}
	table := vecTable(dims)
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			chunk_id     BIGINT PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
			rag_store_id BIGINT NOT NULL,
			embedding    vector(%d) NOT NULL
		)`, table, dims),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_hnsw ON %s USING hnsw (embedding vector_cosine_ops)", table, table),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_store ON %s (rag_store_id)", table, table),
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.vecTables.Store(dims, true)
	return nil
}

// ReplaceDocumentChunks atomically swaps a document's chunks and embeddings
// and marks it ready. All chunks must share the store's embedding width.
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
		return fmt.Errorf("create embedding table: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Old embeddings go away through ON DELETE CASCADE.
	if _, err := tx.Exec(ctx, "DELETE FROM chunks WHERE document_id = $1", doc.ID); err != nil {
		return err
	}
	batch := &pgx.Batch{}
	for _, c := range chunks {
		batch.Queue(`INSERT INTO chunks (document_id, rag_store_id, idx, content, token_estimate)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, doc.ID, doc.RAGStoreID, c.Index, c.Content, c.TokenEstimate)
	}
	br := tx.SendBatch(ctx, batch)
	for _, c := range chunks {
		if err := br.QueryRow().Scan(&c.ID); err != nil {
			br.Close()
			return fmt.Errorf("insert chunk: %w", err)
		}
		c.DocumentID, c.RAGStoreID = doc.ID, doc.RAGStoreID
	}
	if err := br.Close(); err != nil {
		return err
	}

	insVec := fmt.Sprintf("INSERT INTO %s (chunk_id, rag_store_id, embedding) VALUES ($1, $2, $3)", vecTable(dims))
	batch = &pgx.Batch{}
	for _, c := range chunks {
		batch.Queue(insVec, c.ID, doc.RAGStoreID, pgvector.NewVector(c.Embedding))
	}
	br = tx.SendBatch(ctx, batch)
	for range chunks {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return fmt.Errorf("insert embedding: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, "UPDATE documents SET status=$1, error='', chunk_count=$2, updated_at=now() WHERE id=$3",
		DocReady, len(chunks), doc.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SearchTopK returns the k nearest chunks in a store for a query vector,
// ordered by cosine distance (0 = identical).
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

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	// HNSW returns at most ef_search candidates; keep it comfortably above k.
	efSearch := k * 4
	if efSearch < 40 {
		efSearch = 40
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		SELECT e.chunk_id, e.embedding <=> $1::vector AS distance, c.document_id, c.idx, c.content, d.filename
		FROM %s e
		JOIN chunks c ON c.id = e.chunk_id
		JOIN documents d ON d.id = c.document_id
		WHERE e.rag_store_id = $2
		ORDER BY e.embedding <=> $1::vector
		LIMIT $3`, vecTable(r.Dimensions)), pgvector.NewVector(query), r.ID, k)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, tx.Commit(ctx)
}
