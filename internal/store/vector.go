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
	// MaxDistance drops candidates with a cosine distance above it; 0 = off.
	MaxDistance float64
	// Candidates caps the vector and full-text candidate lists and the
	// number of fused results returned.
	Candidates int
	// Filter restricts the search to documents whose metadata it lets through
	// (ADR-025). It is applied to every candidate list before that list's
	// LIMIT, so a filtered search still returns Candidates rows whenever that
	// many chunks match. Nil or empty restricts nothing.
	Filter *MetadataFilter
}

// vecTable names the embedding table for one vector width. Embeddings of
// different models have different dimensions and pgvector needs a fixed
// width per column, hence one table per dimension.
// maxVectorDims is pgvector's limit for the vector type.
const maxVectorDims = 16000

// MaxIndexedVectorDims is the widest vector pgvector can put an HNSW index
// on with the vector type. Wider embeddings are indexed through a halfvec
// expression index instead (see MaxHalfvecIndexedDims).
const MaxIndexedVectorDims = 2000

// MaxHalfvecIndexedDims is the widest embedding that can be HNSW-indexed at
// all: pgvector builds a halfvec HNSW index up to 4000 dimensions. The
// embedding column stays vector(N); the index is on embedding::halfvec(N)
// (ADR-026), so nothing is copied and no table changes shape.
const MaxHalfvecIndexedDims = 4000

// ErrVectorDimsUnsupported is returned when a store would be bound to an
// embedding that cannot be indexed: wider than MaxHalfvecIndexedDims, or
// wider than MaxIndexedVectorDims on a pgvector without halfvec.
var ErrVectorDimsUnsupported = errors.New("embedding dimension not supported")

// ErrVectorIndexMissing is returned when a store would be bound to a
// 2001..4000 wide table that has no valid halfvec index: binding to it would
// put a new store on an exact scan. The width has to be reindexed first.
var ErrVectorIndexMissing = errors.New("embedding width has no halfvec index")

// ErrEmbeddingConnectionChanged stops an ingest whose vectors were produced
// by an embedding connection the store no longer uses: they would be written
// next to vectors of another model, or with the wrong width.
var ErrEmbeddingConnectionChanged = errors.New("embedding connection of the store changed during ingestion")

// CheckVectorDims rejects an embedding width no pgvector can index. Stores
// that already carry a wider dimension keep working (see ensureVecTable);
// this only guards the first assignment. The server-dependent limits (the
// halfvec ceiling needs pgvector 0.7.0) and the index state of the width are
// checked by Store.CheckBindable.
func CheckVectorDims(dims int) error {
	if dims > MaxHalfvecIndexedDims {
		return fmt.Errorf("%w: the embedding model returns %d dimensions, but at most %d can be indexed "+
			"(HNSW on halfvec); choose a model (or a dimensions setting) of %d or fewer -- binary quantization "+
			"for wider models is planned for v0.9", ErrVectorDimsUnsupported, dims, MaxHalfvecIndexedDims, MaxHalfvecIndexedDims)
	}
	return nil
}

// checkVectorDims is CheckVectorDims plus what this server's pgvector can do:
// without halfvec (before 0.7.0) the limit stays at MaxIndexedVectorDims.
func (c Capabilities) checkVectorDims(dims int) error {
	if err := CheckVectorDims(dims); err != nil {
		return err
	}
	if dims > MaxIndexedVectorDims && !c.SupportsHalfvec() {
		have := "pgvector " + c.PgvectorVersion
		if c.PgvectorVersion == "" {
			have = "the installed pgvector (its version could not be read)"
		}
		return fmt.Errorf("%w: the embedding model returns %d dimensions, but %s can index at most %d "+
			"with the vector type; indexing up to %d needs halfvec, which exists from pgvector 0.7.0 -- upgrade "+
			"pgvector or choose a model (or a dimensions setting) of %d or fewer",
			ErrVectorDimsUnsupported, dims, have, MaxIndexedVectorDims, MaxHalfvecIndexedDims, MaxIndexedVectorDims)
	}
	return nil
}

func vecTable(dims int) string { return fmt.Sprintf("chunk_embeddings_%d", dims) }

// ensureVecTable creates the embedding table and its indexes for a dimension
// if they do not exist yet. DDL is serialised with an advisory lock so
// concurrent ingesters on several replicas do not race.
//
// A table it creates for a width of 2001..4000 gets the halfvec HNSW index in
// the same transaction (ADR-026), so it is born indexed. A table that already
// exists is never indexed here -- that is the explicit reindex action
// (StartVectorReindex), because building the index on a populated table takes
// minutes and must not hide inside an ingest -- with one exception: a table
// that is empty and that no store is bound to is indexed in place, since
// there is nothing to wait for and no store left to ask for a reindex.
//
// Widths above MaxHalfvecIndexedDims, and widths above MaxIndexedVectorDims on
// a pgvector without halfvec, get the table without any HNSW index: stores
// bound to such a model before the limit was enforced keep working with an
// exact (sequential) scan, and are reported at startup (see AuditRAGStores).
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
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
		return err
	}
	// A table that exists gets no DDL here, not even an idempotent
	// CREATE INDEX IF NOT EXISTS: that statement takes a ShareLock on the
	// table before it looks, which waits for a running CREATE INDEX
	// CONCURRENTLY of the halfvec index, and the build in turn waits for this
	// transaction -- a deadlock that kills the build. The catalog is read
	// instead, and the tables this code made always carry their indexes (the
	// create is one transaction).
	//
	// Remembered as existing only when nothing is left to do for it later: a
	// halfvec width that is not indexed is looked at again by the next write,
	// since a reindex or the deletion of its last store changes the answer.
	var stmts []string
	cacheable := true
	if !exists {
		stmts = append(stmts, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			chunk_id     BIGINT PRIMARY KEY REFERENCES chunks(id) ON DELETE CASCADE,
			rag_store_id BIGINT NOT NULL,
			embedding    vector(%d) NOT NULL
		)`, table, dims))
		if dims <= MaxIndexedVectorDims {
			stmts = append(stmts,
				fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_hnsw ON %s USING hnsw (embedding vector_cosine_ops)", table, table))
		}
		stmts = append(stmts, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s_store ON %s (rag_store_id)", table, table))
		if halfvecIndexable(dims, s.caps) {
			stmts = append(stmts, halfvecIndexDDL(dims, false))
		}
	} else if halfvecIndexable(dims, s.caps) {
		state, err := s.halfvecIndexState(ctx, tx, dims)
		if err != nil {
			return err
		}
		cacheable = state == halfvecValid
		if state == halfvecAbsent {
			// Unreferenced and empty: index it now. The state read is the
			// one a reindex would make, so an invalid or running build is
			// left to that action, and the reindex lock (tried, never
			// waited for) keeps this from meeting one that is just starting.
			orphan, err := s.orphanVecTable(ctx, tx, dims)
			if err != nil {
				return err
			}
			var got bool
			if orphan {
				if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1, $2)", lockVecReindex, int32(dims)).Scan(&got); err != nil {
					return err
				}
			}
			if got {
				stmts = append(stmts, halfvecIndexDDL(dims, false))
				cacheable = true
			}
		}
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if cacheable {
		s.vecTables.Store(dims, true)
	}
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
// The first write also binds the store to the vectors' width; a width that
// cannot be indexed is refused there (ErrVectorDimsUnsupported), and so is a
// 2001..4000 width whose table has no valid halfvec index yet
// (ErrVectorIndexMissing).
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
	if r.Dimensions == 0 {
		if err := s.checkBindable(ctx, s.pool, dims); err != nil {
			return err
		}
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
	var language string
	if err := tx.QueryRow(ctx, "SELECT embedding_connection_id, dimensions, language FROM rag_stores WHERE id = $1 FOR NO KEY UPDATE",
		doc.RAGStoreID).Scan(&curConn, &curDims, &language); err != nil {
		return scanErr(err)
	}
	// The language is read under the row lock a language change also takes,
	// so every chunk is written with the language the store has when the
	// write commits (ADR-027). A 'simple' store keeps chunks.fts NULL and
	// searches the generated chunks.tsv; any other language gets chunks.fts
	// here, also while a rebuild of the older chunks is still running.
	ftsCfg, ok := LanguageConfig(language)
	if !ok {
		return fmt.Errorf("rag store %d has language %q, which maps to no text search configuration", doc.RAGStoreID, language)
	}
	ftsExpr := "NULL"
	if language != LanguageSimple {
		ftsExpr = "to_tsvector($8::regconfig, $4)"
	}
	if err := checkStoreBinding(curConn, curDims, embeddingConnID, dims); err != nil {
		return err
	}
	if curDims == 0 {
		// The table exists now, so a width without a usable index shows up.
		if err := s.checkBindable(ctx, tx, dims); err != nil {
			return err
		}
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
		// $8 is only referenced when the store has a language; a statement
		// that does not mention it is sent without it.
		args := []any{doc.ID, doc.RAGStoreID, c.Index, c.Content, c.TokenEstimate, meta, embedText}
		if language != LanguageSimple {
			args = append(args, ftsCfg)
		}
		batch.Queue(`INSERT INTO chunks (document_id, rag_store_id, idx, content, token_estimate, metadata, embed_text, fts)
			VALUES ($1, $2, $3, $4, $5, $6, $7, `+ftsExpr+`) RETURNING id`, args...)
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

// useHalfvecOrder reports whether the vector leg of a search on a table of
// dims dimensions orders by the halfvec expression (ADR-026): the widths the
// halfvec index covers, on a server that has halfvec. It does not look at
// whether the index exists -- without it the same statement is answered by a
// sequential scan, in the same order, so a store that has not been reindexed
// yet and one that has return the same results.
func (s *Store) useHalfvecOrder(dims int) bool { return halfvecIndexable(dims, s.caps) }

// searchStatement builds the statement and arguments of one search. It is the
// whole SQL half of runSearch, separate so tests can run it under planner
// settings runSearch does not expose.
//
// halfvec says the store's table is searched in halfvec order (see
// useHalfvecOrder).
func searchStatement(backend SearchBackend, r *RAGStore, query string, queryVec []float32, opts SearchOptions,
	halfvec bool) (string, []any) {
	table := vecTable(r.Dimensions)
	vec := pgvector.NewVector(queryVec)
	halfDims := 0
	if halfvec {
		halfDims = r.Dimensions
	}
	if opts.Mode != SearchVector {
		column, config := r.lexicalPlan()
		return backend.HybridQuery(SearchParams{VecTable: table, HalfvecDims: halfDims, Vector: vec, StoreID: r.ID,
			Candidates: opts.Candidates, MaxDistance: opts.MaxDistance, FTSColumn: column, FTSConfig: config, Query: query,
			Filter: opts.Filter})
	}
	// Vector-only retrieval is identical under every backend: it never
	// reads a lexical index. The twelfth column is the constant lex
	// score, so one scan loop serves both modes.
	args := []any{vec, r.ID, opts.Candidates, opts.MaxDistance}
	leg, filterArgs := vectorLeg(table, halfDims, opts.Filter, len(args)+1)
	args = append(args, filterArgs...)
	return fmt.Sprintf(`
			WITH %s
			SELECT v.chunk_id, c.document_id, c.idx, c.content, d.filename,
			       COALESCE(c.metadata->>'section', ''), COALESCE((c.metadata->>'page')::int, 0),
			       v.distance, 1.0/(%d+v.rank), v.rank, 0, 0::float8
			FROM v
			JOIN chunks c ON c.id = v.chunk_id
			JOIN documents d ON d.id = c.document_id
			WHERE ($4::float8 = 0 OR v.distance <= $4::float8)
			ORDER BY v.distance, v.chunk_id`, leg, rrfK), args
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
	// One snapshot for the whole search, taken by the store row read below.
	// The language of a store, the state of its chunks.fts and the chunks the
	// lexical leg reads then agree even when the language is changed -- and
	// its rebuild batches commit -- while the search runs: the lexical leg
	// never queries a column with a configuration it was not built with
	// (ADR-027).
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	cur := *r
	if err := tx.QueryRow(ctx, "SELECT language, fts_language_applied FROM rag_stores WHERE id = $1", r.ID).
		Scan(&cur.Language, &cur.FTSLanguageApplied); err != nil {
		return nil, scanErr(err)
	}
	r = &cur
	// HNSW returns at most ef_search candidates; keep it comfortably above N.
	efSearch := opts.Candidates * 4
	if efSearch < 40 {
		efSearch = 40
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", efSearch)); err != nil {
		return nil, err
	}
	// Filtered or not, every search walks the HNSW index in iterative mode.
	// The embedding tables are shared by all stores of one width, so even an
	// unfiltered search is filtered by rag_store_id after the index scan, and
	// a small store beside a big one would otherwise get back only the few
	// candidates of ef_search that happen to be its own. relaxed_order keeps
	// scanning until LIMIT rows survive the filters; the legs re-sort by
	// distance (see vectorLeg). hnsw.max_scan_tuples stays at its default so
	// a filter nothing matches cannot scan the whole index.
	if s.caps.SupportsIterativeScan() {
		if _, err := tx.Exec(ctx, "SET LOCAL hnsw.iterative_scan = relaxed_order"); err != nil {
			return nil, err
		}
	} else if s.caps.PgvectorVersion == "" {
		s.warnBackendOnce(r.ID, warnIterativeScan,
			"the pgvector version could not be read, so hnsw.iterative_scan is left unset: filtered searches, and "+
				"searches in a store that shares its embedding table with a larger one, can return fewer results than requested",
			"rag_store", r.ID)
	} else {
		s.warnBackendOnce(r.ID, warnIterativeScan,
			"pgvector is older than 0.8.0 and has no iterative index scan: filtered searches, and searches in a "+
				"store that shares its embedding table with a larger one, can return fewer results than requested",
			"rag_store", r.ID, "pgvector_version", s.caps.PgvectorVersion)
	}
	sql, args := searchStatement(backend, r, query, queryVec, opts, s.useHalfvecOrder(r.Dimensions))
	var rows pgx.Rows
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
