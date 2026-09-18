package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// Chunk is a slice of a document together with its embedding.
type Chunk struct {
	ID            int64  `json:"id"`
	DocumentID    int64  `json:"document_id"`
	RAGStoreID    int64  `json:"rag_store_id"`
	Index         int    `json:"index"`
	Content       string `json:"content"`
	TokenEstimate int    `json:"token_estimate"`
	// Metadata holds section, page and title information extracted at parse time.
	Metadata map[string]any `json:"metadata,omitempty"`
	// EmbedText is the text that was embedded when it differs from Content
	// (contextual chunks); empty otherwise.
	EmbedText string    `json:"embed_text,omitempty"`
	Embedding []float32 `json:"-"`
}

// SearchHit is one retrieval result. VectorRank and FTSRank are 1-based
// positions in the respective candidate lists (0 when the chunk was not a
// candidate on that side); Score is the fused RRF score.
type SearchHit struct {
	ChunkID    int64   `json:"chunk_id"`
	DocumentID int64   `json:"document_id"`
	Filename   string  `json:"filename"`
	Index      int     `json:"index"`
	Content    string  `json:"content"`
	Section    string  `json:"section"`
	Page       int     `json:"page"`
	Distance   float64 `json:"distance"`
	Score      float64 `json:"score"`
	VectorRank int     `json:"vector_rank"`
	FTSRank    int     `json:"fts_rank"`
}

// SearchOptions tunes Search.
type SearchOptions struct {
	// Mode is SearchVector or SearchHybrid (default hybrid).
	Mode string
	// FTSConfig is the text search configuration for parsing the query
	// (default "simple"); indexing always uses "simple".
	FTSConfig string
	// MaxDistance drops candidates with a cosine distance above it; 0 = off.
	MaxDistance float64
	// Candidates caps the vector and full-text candidate lists and the
	// number of fused results returned.
	Candidates int
}

// vecTable names the embedding table for one vector width. Embeddings of
// different models have different dimensions and pgvector needs a fixed
// width per column, hence one table per dimension.
// maxVectorDims is pgvector's limit for the vector type.
const maxVectorDims = 16000

func vecTable(dims int) string { return fmt.Sprintf("chunk_embeddings_%d", dims) }

// ensureVecTable creates the embedding table and its indexes for a dimension
// if they do not exist yet. DDL is serialised with an advisory lock so
// concurrent ingesters on several replicas do not race.
func (s *Store) ensureVecTable(ctx context.Context, dims int) error {
	if dims < 1 || dims > maxVectorDims {
		return fmt.Errorf("embedding dimension %d out of range (1-%d)", dims, maxVectorDims)
	}
	if _, ok := s.vecTables.Load(dims); ok {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
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
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	// Old embeddings go away through ON DELETE CASCADE.
	if _, err := tx.Exec(ctx, "DELETE FROM chunks WHERE document_id = $1", doc.ID); err != nil {
		return err
	}
	batch := &pgx.Batch{}
	for _, c := range chunks {
		meta := c.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		var embedText *string
		if c.EmbedText != "" {
			embedText = &c.EmbedText
		}
		batch.Queue(`INSERT INTO chunks (document_id, rag_store_id, idx, content, token_estimate, metadata, embed_text)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`, doc.ID, doc.RAGStoreID, c.Index, c.Content, c.TokenEstimate, meta, embedText)
	}
	br := tx.SendBatch(ctx, batch)
	for _, c := range chunks {
		if err := br.QueryRow().Scan(&c.ID); err != nil {
			_ = br.Close()
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
			_ = br.Close()
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
// ordered by cosine distance (0 = identical). It is Search in vector mode.
func (s *Store) SearchTopK(ctx context.Context, storeID int64, query []float32, k int) ([]SearchHit, error) {
	return s.Search(ctx, storeID, "", query, SearchOptions{Mode: SearchVector, Candidates: k})
}

// Search runs vector or hybrid retrieval. In hybrid mode the vector top-N
// and the full-text top-N are fused with reciprocal rank fusion
// (score = 1/(60+rank) summed over both lists); every candidate also gets
// its cosine distance so MaxDistance applies uniformly.
func (s *Store) Search(ctx context.Context, storeID int64, query string, queryVec []float32, opts SearchOptions) ([]SearchHit, error) {
	if opts.Candidates <= 0 {
		opts.Candidates = 5
	}
	if opts.Mode == "" {
		opts.Mode = SearchHybrid
	}
	if opts.FTSConfig == "" {
		opts.FTSConfig = "simple"
	}
	r, err := s.GetRAGStore(ctx, storeID)
	if err != nil {
		return nil, err
	}
	if r.Dimensions == 0 || r.ChunkCount == 0 {
		return []SearchHit{}, nil
	}
	if len(queryVec) != r.Dimensions {
		return nil, fmt.Errorf("query vector has %d dimensions, store expects %d", len(queryVec), r.Dimensions)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	// HNSW returns at most ef_search candidates; keep it comfortably above N.
	efSearch := opts.Candidates * 4
	if efSearch < 40 {
		efSearch = 40
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
		return nil, err
	}
	table := vecTable(r.Dimensions)
	vec := pgvector.NewVector(queryVec)
	var rows pgx.Rows
	switch opts.Mode {
	case SearchVector:
		rows, err = tx.Query(ctx, fmt.Sprintf(`
			WITH v AS (
				SELECT e.chunk_id, e.embedding <=> $1::vector AS distance,
				       ROW_NUMBER() OVER (ORDER BY e.embedding <=> $1::vector) AS rank
				FROM %s e
				WHERE e.rag_store_id = $2
				ORDER BY e.embedding <=> $1::vector
				LIMIT $3)
			SELECT v.chunk_id, c.document_id, c.idx, c.content, d.filename,
			       COALESCE(c.metadata->>'section', ''), COALESCE((c.metadata->>'page')::int, 0),
			       v.distance, 1.0/(60+v.rank), v.rank, 0
			FROM v
			JOIN chunks c ON c.id = v.chunk_id
			JOIN documents d ON d.id = c.document_id
			WHERE ($4::float8 = 0 OR v.distance <= $4::float8)
			ORDER BY v.distance, v.chunk_id`, table), vec, r.ID, opts.Candidates, opts.MaxDistance)
	case SearchHybrid:
		// An empty or stop-word-only query yields a tsquery with numnode = 0;
		// the full-text side is then simply empty instead of an error.
		rows, err = tx.Query(ctx, fmt.Sprintf(`
			WITH q AS (SELECT websearch_to_tsquery($5::text::regconfig, $6::text) AS query),
			v AS (
				SELECT e.chunk_id, ROW_NUMBER() OVER (ORDER BY e.embedding <=> $1::vector) AS rank
				FROM %[1]s e
				WHERE e.rag_store_id = $2
				ORDER BY e.embedding <=> $1::vector
				LIMIT $3),
			f AS (
				SELECT c.id AS chunk_id, ROW_NUMBER() OVER (ORDER BY ts_rank_cd(c.tsv, q.query) DESC, c.id) AS rank
				FROM chunks c, q
				WHERE c.rag_store_id = $2 AND numnode(q.query) > 0 AND c.tsv @@ q.query
				ORDER BY ts_rank_cd(c.tsv, q.query) DESC, c.id
				LIMIT $3),
			m AS (
				SELECT COALESCE(v.chunk_id, f.chunk_id) AS chunk_id,
				       COALESCE(v.rank, 0) AS vrank, COALESCE(f.rank, 0) AS frank,
				       COALESCE(1.0/(60+v.rank), 0) + COALESCE(1.0/(60+f.rank), 0) AS score
				FROM v FULL OUTER JOIN f ON v.chunk_id = f.chunk_id)
			SELECT m.chunk_id, c.document_id, c.idx, c.content, d.filename,
			       COALESCE(c.metadata->>'section', ''), COALESCE((c.metadata->>'page')::int, 0),
			       e.embedding <=> $1::vector AS distance, m.score, m.vrank, m.frank
			FROM m
			JOIN chunks c ON c.id = m.chunk_id
			JOIN documents d ON d.id = c.document_id
			JOIN %[1]s e ON e.chunk_id = m.chunk_id
			WHERE ($4::float8 = 0 OR (e.embedding <=> $1::vector) <= $4::float8)
			ORDER BY m.score DESC, distance, m.chunk_id
			LIMIT $3`, table), vec, r.ID, opts.Candidates, opts.MaxDistance, opts.FTSConfig, ftsQuery(query))
	default:
		return nil, fmt.Errorf("unknown search mode %q", opts.Mode)
	}
	if err != nil {
		return nil, fmt.Errorf("%s search: %w", opts.Mode, err)
	}
	defer rows.Close()
	hits := []SearchHit{}
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ChunkID, &h.DocumentID, &h.Index, &h.Content, &h.Filename, &h.Section, &h.Page,
			&h.Distance, &h.Score, &h.VectorRank, &h.FTSRank); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, tx.Commit(ctx)
}

// ftsQuery prepares free text for websearch_to_tsquery. That function ANDs
// all words, which makes natural-language questions ("what is the zyxquux
// protocol") match only chunks containing every word. Plain queries are
// therefore OR-ed; ts_rank_cd still ranks chunks matching more terms higher.
// Queries using websearch syntax (quotes, "or", a leading "-") are passed
// through unchanged.
func ftsQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" || strings.Contains(q, `"`) {
		return q
	}
	words := strings.Fields(q)
	for _, w := range words {
		if strings.EqualFold(w, "or") || strings.HasPrefix(w, "-") {
			return q
		}
	}
	return strings.Join(words, " or ")
}
