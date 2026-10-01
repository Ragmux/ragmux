package store

import (
	"context"
	"errors"
	"fmt"

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
	// FTSRank keeps its JSON name across backends: it is the rank on the
	// lexical side whatever produced that side.
	FTSRank int `json:"fts_rank"`
	// LexScore is the raw lexical relevance of the hit: a BM25 score under
	// the pg_search backend, 0 under pgvector, where ts_rank_cd is neither
	// comparable across queries nor a BM25 score and is therefore not
	// exposed at all.
	LexScore float64 `json:"lex_score,omitempty"`
}

// SearchOptions tunes Search.
type SearchOptions struct {
	// Mode is SearchVector or SearchHybrid (default hybrid).
	Mode string
	// Backend selects the lexical backend of a hybrid search
	// (BackendPgvector or BackendPgSearch; default pgvector). It is ignored
	// in vector mode, which never touches a lexical index.
	Backend string
	// FTSConfig is the text search configuration for parsing the query
	// (default "simple"); indexing always uses "simple". Only the pgvector
	// backend reads it.
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

// MaxIndexedVectorDims is the widest vector pgvector can put an HNSW index
// on. A wider store cannot be indexed with the vector type at all; halfvec
// support, which lifts the ceiling to 4000, is planned for v0.6.
const MaxIndexedVectorDims = 2000

// ErrVectorDimsUnsupported is returned when a store would be bound to an
// embedding wider than MaxIndexedVectorDims for the first time.
var ErrVectorDimsUnsupported = errors.New("embedding dimension not supported")

// ErrEmbeddingConnectionChanged stops an ingest whose vectors were produced
// by an embedding connection the store no longer uses: they would be written
// next to vectors of another model, or with the wrong width.
var ErrEmbeddingConnectionChanged = errors.New("embedding connection of the store changed during ingestion")

// CheckVectorDims rejects an embedding width a new store cannot be bound to.
// Stores that already carry a wider dimension keep working (see
// ensureVecTable); this only guards the first assignment.
func CheckVectorDims(dims int) error {
	if dims > MaxIndexedVectorDims {
		return fmt.Errorf("%w: the embedding model returns %d dimensions, but pgvector can index at most %d "+
			"with the vector type; choose a model (or a dimensions setting) of %d or fewer -- halfvec support "+
			"for wider models is planned for v0.6", ErrVectorDimsUnsupported, dims, MaxIndexedVectorDims, MaxIndexedVectorDims)
	}
	return nil
}

func vecTable(dims int) string { return fmt.Sprintf("chunk_embeddings_%d", dims) }

// ensureVecTable creates the embedding table and its indexes for a dimension
// if they do not exist yet. DDL is serialised with an advisory lock so
// concurrent ingesters on several replicas do not race.
//
// Widths above MaxIndexedVectorDims get the table without the HNSW index,
// which pgvector refuses to build for them: stores bound to such a model
// before the limit was enforced keep working with an exact (sequential)
// scan, and are reported at startup (see AuditRAGStores).
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
	}
	if dims <= MaxIndexedVectorDims {
		stmts = append(stmts,
			fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_hnsw ON %s USING hnsw (embedding vector_cosine_ops)", table, table))
	}
	stmts = append(stmts, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_store ON %s (rag_store_id)", table, table))
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
// and marks it ready. All chunks must share the store's embedding width. It
// does not check which embedding connection produced the vectors; ingestion
// uses ReplaceDocumentChunksFrom.
func (s *Store) ReplaceDocumentChunks(ctx context.Context, doc *Document, chunks []*Chunk, owner string) error {
	return s.ReplaceDocumentChunksFrom(ctx, doc, 0, chunks, owner)
}

// ReplaceDocumentChunksFrom is ReplaceDocumentChunks for vectors produced by
// the embedding connection embeddingConnID (0 skips the check). The store
// row is locked for the length of the write, the same lock UpdateRAGStore
// takes, so a connection change and an ingest are serialised: whichever
// commits second sees the other's result. An ingest that loses that race
// stops with ErrEmbeddingConnectionChanged instead of writing vectors of
// the old model into the store.
//
// The first write also binds the store to the vectors' width; a width above
// MaxIndexedVectorDims is refused there (ErrVectorDimsUnsupported).
func (s *Store) ReplaceDocumentChunksFrom(ctx context.Context, doc *Document, embeddingConnID int64,
	chunks []*Chunk, owner string) error {
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
	// Unlocked pre-check, so a refused width or a stale ingest never gets
	// as far as creating an embedding table. It is repeated under the lock.
	r, err := s.GetRAGStore(ctx, doc.RAGStoreID)
	if err != nil {
		return err
	}
	if err := checkStoreBinding(r.EmbeddingConnectionID, r.Dimensions, embeddingConnID, dims); err != nil {
		return err
	}
	if err := s.ensureVecTable(ctx, dims); err != nil {
		return fmt.Errorf("create embedding table: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	var curConn int64
	var curDims int
	if err := tx.QueryRow(ctx, "SELECT embedding_connection_id, dimensions FROM rag_stores WHERE id = $1 FOR NO KEY UPDATE",
		doc.RAGStoreID).Scan(&curConn, &curDims); err != nil {
		return scanErr(err)
	}
	if err := checkStoreBinding(curConn, curDims, embeddingConnID, dims); err != nil {
		return err
	}
	if curDims == 0 {
		if _, err := tx.Exec(ctx, "UPDATE rag_stores SET dimensions = $1 WHERE id = $2", dims, doc.RAGStoreID); err != nil {
			return err
		}
	}

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

	// The same ownership check the progress write makes, and here it also
	// protects the chunk rewrite above: this runs in the transaction that
	// deleted the old chunks, so a worker whose lease expired mid-job rolls
	// its own work back instead of replacing what the new owner wrote.
	q := "UPDATE documents SET status=$1, error='', chunk_count=$2, progress_percent=100, updated_at=now() WHERE id=$3"
	args := []any{DocReady, len(chunks), doc.ID}
	if owner != "" {
		args = append(args, owner)
		q += fmt.Sprintf(" AND claimed_by=$%d AND status='processing'", len(args))
	}
	tag, err := tx.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 && owner != "" {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// checkStoreBinding compares what a store is bound to (its embedding
// connection and width, 0 = not bound yet) with the vectors about to be
// written. wantConn 0 skips the connection check.
func checkStoreBinding(storeConn int64, storeDims int, wantConn int64, dims int) error {
	if wantConn != 0 && storeConn != wantConn {
		return fmt.Errorf("%w: vectors were embedded with connection %d, the store now uses %d; "+
			"reprocess the document", ErrEmbeddingConnectionChanged, wantConn, storeConn)
	}
	if storeDims == 0 {
		return CheckVectorDims(dims)
	}
	if storeDims != dims {
		return fmt.Errorf("embedding dimension %d does not match store dimension %d", dims, storeDims)
	}
	return nil
}

// SearchTopK returns the k nearest chunks in a store for a query vector,
// ordered by cosine distance (0 = identical). It is Search in vector mode.
func (s *Store) SearchTopK(ctx context.Context, storeID int64, query []float32, k int) ([]SearchHit, error) {
	return s.Search(ctx, storeID, "", query, SearchOptions{Mode: SearchVector, Candidates: k})
}

// Search runs vector or hybrid retrieval. In hybrid mode the vector top-N
// and the lexical top-N are fused with reciprocal rank fusion
// (score = 1/(60+rank) summed over both lists); every candidate also gets
// its cosine distance so MaxDistance applies uniformly.
func (s *Store) Search(ctx context.Context, storeID int64, query string, queryVec []float32, opts SearchOptions) ([]SearchHit, error) {
	hits, _, err := s.SearchWithBackend(ctx, storeID, query, queryVec, opts)
	return hits, err
}

// SearchWithBackend is Search plus the backend that actually answered the
// lexical half. It differs from opts.Backend when the configured backend is
// not available on this server, when its index could not be prepared, or
// when its query failed; in each case the search degrades to pgvector and
// logs rather than failing, because a retrieval problem must not take the
// request down with it (PRD behaviour rule 8).
func (s *Store) SearchWithBackend(ctx context.Context, storeID int64, query string, queryVec []float32, opts SearchOptions) ([]SearchHit, string, error) {
	if opts.Candidates <= 0 {
		opts.Candidates = 5
	}
	if opts.Mode == "" {
		opts.Mode = SearchHybrid
	}
	if opts.FTSConfig == "" {
		opts.FTSConfig = "simple"
	}
	if opts.Mode != SearchVector && opts.Mode != SearchHybrid {
		return nil, "", fmt.Errorf("unknown search mode %q", opts.Mode)
	}
	// Resolved even in vector mode: it costs one map lookup, and a store
	// left pointing at a backend this server cannot run should say so on
	// every search rather than only on the hybrid ones.
	backend := s.resolveBackend(opts.Backend, storeID)
	r, err := s.GetRAGStore(ctx, storeID)
	if err != nil {
		return nil, backend.Name(), err
	}
	if r.Dimensions == 0 || r.ChunkCount == 0 {
		return []SearchHit{}, backend.Name(), nil
	}
	if len(queryVec) != r.Dimensions {
		return nil, backend.Name(), fmt.Errorf("query vector has %d dimensions, store expects %d", len(queryVec), r.Dimensions)
	}
	if opts.Mode == SearchHybrid {
		if err := backend.Prepare(ctx, s); err != nil {
			// A backend that cannot prepare its index is as unusable as one
			// the server does not carry; degrade rather than fail the search.
			s.warnBackendOnce(storeID, warnPrepare,
				"search backend could not be prepared; falling back to pgvector",
				"rag_store", storeID, "backend", backend.Name(), "err", err)
			backend = searchBackends[BackendPgvector]
		} else {
			s.clearBackendWarning(storeID, warnPrepare)
		}
	}

	hits, err := s.runSearch(ctx, backend, r, query, queryVec, opts)
	if err == nil {
		if backend.Name() != BackendPgvector {
			s.clearBackendWarning(storeID, warnQuery)
		}
		return hits, backend.Name(), nil
	}
	// Retry only what the lexical backend is actually to blame for. A
	// vector-only search never reads a lexical index; a cancelled context
	// would fail the retry too; and an error from Begin, SET LOCAL, Scan or
	// Commit is the database or the pool, not the backend -- retrying those
	// would double the load on a server that is already struggling, clear a
	// perfectly good index flag into an extra DDL transaction per search,
	// and name the wrong culprit in the log.
	if opts.Mode != SearchHybrid || ctx.Err() != nil || backend.Name() == BackendPgvector ||
		!errors.Is(err, errLexicalQuery) {
		return hits, backend.Name(), err
	}
	// What is left is the statement the backend built, rejected by the
	// server. The case this exists for is an index dropped under a running
	// process -- exactly what docs/configuration.md tells an operator to do
	// to change PG_SEARCH_TOKENIZER: Prepare returns from this process's
	// memory, the query sends @@@ at a table that no longer has a BM25
	// index, and every hybrid search on this replica would fail until it was
	// restarted. Retrieval degrades instead (PRD behaviour rule 8), and
	// Invalidate is what makes the next search rebuild rather than repeat
	// this.
	backend.Invalidate(s)
	s.warnBackendOnce(storeID, warnQuery,
		"hybrid search failed on its lexical backend; falling back to pgvector and rebuilding on the next search",
		"rag_store", storeID, "backend", backend.Name(), "err", err)
	backend = searchBackends[BackendPgvector]
	hits, err = s.runSearch(ctx, backend, r, query, queryVec, opts)
	return hits, backend.Name(), err
}

// errLexicalQuery marks the one failure the fallback may act on: the fused
// statement a backend built was rejected by the server. Everything else
// runSearch can return -- a transaction that would not begin, a session
// setting that would not apply, a row that would not scan, a commit that
// would not land -- is the database or the pool, and answers the same way on
// pgvector.
var errLexicalQuery = errors.New("search statement rejected")

// runSearch executes one search with one backend. It is the whole database
// half of SearchWithBackend, split out so a hybrid search whose lexical
// backend fails can be retried on pgvector: the first attempt's transaction
// is aborted by the failed query, so the retry needs a transaction of its
// own rather than another statement on this one.
func (s *Store) runSearch(ctx context.Context, backend SearchBackend, r *RAGStore,
	query string, queryVec []float32, opts SearchOptions) ([]SearchHit, error) {
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
	var (
		rows pgx.Rows
		sql  string
		args []any
	)
	if opts.Mode == SearchVector {
		// Vector-only retrieval is identical under every backend: it never
		// reads a lexical index. The twelfth column is the constant lex
		// score, so one scan loop serves both modes.
		sql = fmt.Sprintf(`
			WITH v AS (
				SELECT e.chunk_id, e.embedding <=> $1::vector AS distance,
				       ROW_NUMBER() OVER (ORDER BY e.embedding <=> $1::vector) AS rank
				FROM %s e
				WHERE e.rag_store_id = $2
				ORDER BY e.embedding <=> $1::vector
				LIMIT $3)
			SELECT v.chunk_id, c.document_id, c.idx, c.content, d.filename,
			       COALESCE(c.metadata->>'section', ''), COALESCE((c.metadata->>'page')::int, 0),
			       v.distance, 1.0/(%d+v.rank), v.rank, 0, 0::float8
			FROM v
			JOIN chunks c ON c.id = v.chunk_id
			JOIN documents d ON d.id = c.document_id
			WHERE ($4::float8 = 0 OR v.distance <= $4::float8)
			ORDER BY v.distance, v.chunk_id`, table, rrfK)
		args = []any{vec, r.ID, opts.Candidates, opts.MaxDistance}
	} else {
		sql, args = backend.HybridQuery(SearchParams{VecTable: table, Vector: vec, StoreID: r.ID,
			Candidates: opts.Candidates, MaxDistance: opts.MaxDistance, FTSConfig: opts.FTSConfig, Query: query})
	}
	// Query and rows.Err are the two places the server can reject the
	// statement itself -- pgx reports a plan failure from either, depending
	// on how far the protocol got -- so both carry errLexicalQuery and
	// nothing else does.
	rows, err = tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("%s search: %w: %w", opts.Mode, errLexicalQuery, err)
	}
	defer rows.Close()
	hits := []SearchHit{}
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ChunkID, &h.DocumentID, &h.Index, &h.Content, &h.Filename, &h.Section, &h.Page,
			&h.Distance, &h.Score, &h.VectorRank, &h.FTSRank, &h.LexScore); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s search: %w: %w", opts.Mode, errLexicalQuery, err)
	}
	return hits, tx.Commit(ctx)
}
