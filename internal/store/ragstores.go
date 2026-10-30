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
	// Language is the store's language (ADR-027): "simple" or an ISO 639-1
	// code. It picks the text search configuration chunks are indexed and
	// queries parsed with on the pgvector backend. The pg_search backend does
	// not read it: its BM25 index is global and tokenised once, by
	// PG_SEARCH_TOKENIZER (see LanguageAppliesToSearch).
	Language string `json:"language"`
	// FTSConfig is the text search configuration of Language. Deprecated: it
	// is derived from Language on every read and kept in the JSON for clients
	// that still look for it; nothing reads the stored column.
	FTSConfig string `json:"fts_config"`
	// FTSLanguageApplied is the language chunks.fts was last built for, nil
	// while that is pending or running (and always nil for "simple", which
	// searches the generated chunks.tsv).
	FTSLanguageApplied *string `json:"fts_language_applied"`
	// FTSStatus is FTSNotNeeded, FTSRebuilding or FTSReady. While the store
	// is rebuilding, searches run on chunks.tsv with 'simple' and stay valid.
	FTSStatus string `json:"fts_status"`
	// LanguageAppliesToSearch is false when the store's lexical search runs
	// on the pg_search backend, which ignores Language.
	LanguageAppliesToSearch bool `json:"language_applies_to_search"`
	// LanguageExplicit is set by a caller whose request carried Language
	// itself, as opposed to one that only sent the deprecated fts_config. On
	// UpdateRAGStore it makes the stored fts_config follow Language even when
	// the language did not change, which is how an operator acknowledges the
	// startup warning of a store saved before 0027 and keeps it on 'simple'.
	// The admin API cannot tell an operator's choice from a read echoed back:
	// every request that sends language sets it, a GET answer returned
	// unchanged in a PUT included, and clears the leftover. Go callers that
	// read a store and update it leave it false and keep the leftover. Not
	// part of the JSON.
	LanguageExplicit bool `json:"-"`
	// ftsRebuildCursor is the highest chunk id the running rebuild finished.
	ftsRebuildCursor int64
	// Rerank enables reranking of the top RerankCandidates hits.
	Rerank           bool `json:"rerank"`
	RerankCandidates int  `json:"rerank_candidates"`
	// RerankBackend is RerankLLM, RerankCohere or RerankVoyage.
	RerankBackend string `json:"rerank_backend"`
	// RerankConnectionID is the model connection reranking runs on: the rerank
	// API's credentials for RerankCohere and RerankVoyage, the chat model for
	// RerankLLM (optional there). It is also the connection the implicit route
	// of /v1/rerank and the admin search use. A NULL behind an API backend (the
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
	// VectorIndex is the state of the store's vector index (one of the
	// VectorIndex* values; empty while the store is not bound to a width) and
	// VectorIndexProgress the percentage while it is being built. Neither is
	// read by GetRAGStore, which runs on every search: AnnotateVectorIndex
	// fills them for the admin API.
	VectorIndex         string `json:"vector_index,omitempty"`
	VectorIndexProgress *int   `json:"vector_index_progress,omitempty"`
}

const ragCols = `r.id, r.name, r.embedding_connection_id, r.chunk_size, r.chunk_overlap, r.top_k,
	r.search_mode, r.search_backend, r.language, r.fts_language_applied, r.fts_rebuild_cursor, r.rerank, r.rerank_candidates,
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
		&r.SearchMode, &r.SearchBackend, &r.Language, &r.FTSLanguageApplied, &r.ftsRebuildCursor, &r.Rerank, &r.RerankCandidates,
		&r.RerankBackend, &r.RerankConnectionID, &maxDist, &r.ContextualChunks,
		&r.MaxDocuments, &r.MaxBytes, &r.Dimensions, &created, &r.DocumentCount, &r.BytesUsed, &r.ChunkCount)
	if err != nil {
		return nil, scanErr(err)
	}
	r.deriveFTS()
	r.MaxDistance = float64(maxDist)
	r.CreatedAt = ts(created)
	return r, nil
}

// FTS states of a store (RAGStore.FTSStatus).
const (
	// FTSNotNeeded: the language is 'simple'; chunks.tsv serves it.
	FTSNotNeeded = "not_needed"
	// FTSRebuilding: chunks.fts is being (re)built for the current language.
	FTSRebuilding = "rebuilding"
	// FTSReady: chunks.fts is complete for the current language.
	FTSReady = "ready"
)

// deriveFTS fills the fields that are computed from Language and
// FTSLanguageApplied rather than stored.
func (r *RAGStore) deriveFTS() {
	if r.Language == "" {
		r.Language = LanguageSimple
	}
	r.FTSConfig, _ = LanguageConfig(r.Language)
	switch {
	case r.Language == LanguageSimple:
		r.FTSStatus = FTSNotNeeded
	case r.FTSLanguageApplied != nil && *r.FTSLanguageApplied == r.Language:
		r.FTSStatus = FTSReady
	default:
		r.FTSStatus = FTSRebuilding
	}
	r.LanguageAppliesToSearch = r.SearchBackend != BackendPgSearch
}

// lexicalPlan says which column and which text search configuration the
// pgvector lexical leg of a search uses. Index and query always use the same
// configuration (ADR-027):
//
//   - 'simple' store: chunks.tsv with 'simple';
//   - chunks.fts complete for the store's language: chunks.fts with that
//     language's configuration;
//   - chunks.fts being rebuilt: chunks.tsv with 'simple', which is valid
//     throughout, so a language change never makes a search wrong or fail.
func (r *RAGStore) lexicalPlan() (column, config string) {
	if r.Language != "" && r.Language != LanguageSimple && r.FTSLanguageApplied != nil && *r.FTSLanguageApplied == r.Language {
		if cfg, ok := LanguageConfig(r.Language); ok {
			return ftsColumn, cfg
		}
	}
	return tsvColumn, LanguageSimple
}

// The two lexical columns of chunks. They are the only names interpolated
// into a search statement for the column.
const (
	tsvColumn = "tsv"
	ftsColumn = "fts"
)

// applyRAGDefaults fills zero-valued retrieval settings so callers that only
// know the pre-0.2 fields keep working.
func applyRAGDefaults(r *RAGStore) {
	if r.SearchMode == "" {
		r.SearchMode = SearchHybrid
	}
	if r.SearchBackend == "" {
		r.SearchBackend = BackendPgvector
	}
	if r.RerankBackend == "" {
		r.RerankBackend = RerankLLM
	}
	if r.RerankCandidates <= 0 {
		r.RerankCandidates = 15
	}
}

// CreateRAGStore inserts a store. Its language is Language, or the language
// the deprecated FTSConfig names, or "simple" when neither is set; a value the
// server cannot honour is ErrInvalidLanguage. A new store holds no chunks, so
// its chunks.fts is complete for any language from the start and there is
// nothing to rebuild.
func (s *Store) CreateRAGStore(ctx context.Context, r *RAGStore) (*RAGStore, error) {
	applyRAGDefaults(r)
	lang, specified, err := s.ResolveLanguage(ctx, r.Language, r.FTSConfig)
	if err != nil {
		return nil, err
	}
	if !specified {
		lang = LanguageSimple
	}
	cfg, _ := LanguageConfig(lang)
	var applied *string
	if lang != LanguageSimple {
		applied = &lang
	}
	var id int64
	err = s.pool.QueryRow(ctx, `INSERT INTO rag_stores
		(name, embedding_connection_id, chunk_size, chunk_overlap, top_k,
		 search_mode, search_backend, fts_config, rerank, rerank_candidates,
		 rerank_backend, rerank_connection_id, max_distance, contextual_chunks,
		 max_documents, max_bytes, language, fts_language_applied)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18) RETURNING id`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK,
		r.SearchMode, r.SearchBackend, cfg, r.Rerank, r.RerankCandidates,
		r.RerankBackend, r.RerankConnectionID, float32(r.MaxDistance), r.ContextualChunks,
		r.MaxDocuments, r.MaxBytes, lang, applied).Scan(&id)
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
//
// The language is Language, or the one FTSConfig names, or -- when neither is
// set -- the store's current one. A different language marks chunks.fts as
// not applied and restarts the rebuild cursor, which is what makes the
// background rebuild (FTSStatus FTSRebuilding) start; a store without chunks
// has nothing to rebuild and is complete at once. The same language changes
// nothing, so saving a store never restarts a rebuild.
func (s *Store) UpdateRAGStore(ctx context.Context, r *RAGStore) (*RAGStore, error) {
	wantLang, langSpecified, err := s.ResolveLanguage(ctx, r.Language, r.FTSConfig)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	var curConn int64
	var curDims, chunkCount int
	var curLang string
	var curApplied *string
	var curCursor int64
	if err := tx.QueryRow(ctx, `SELECT embedding_connection_id, dimensions, language, fts_language_applied, fts_rebuild_cursor
		FROM rag_stores WHERE id = $1 FOR NO KEY UPDATE`,
		r.ID).Scan(&curConn, &curDims, &curLang, &curApplied, &curCursor); err != nil {
		return nil, scanErr(err)
	}
	newLang, newApplied, newCursor := curLang, curApplied, curCursor
	if langSpecified && wantLang != curLang {
		newLang, newApplied, newCursor = wantLang, nil, 0
		if wantLang != LanguageSimple {
			var hasChunks bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM chunks WHERE rag_store_id = $1)", r.ID).Scan(&hasChunks); err != nil {
				return nil, err
			}
			if !hasChunks {
				newApplied = &newLang
			}
		}
	}
	// The stored fts_config is derived from the language and only rewritten
	// when the language really changes, or when the caller named the language
	// itself (LanguageExplicit): a store saved before 0027 keeps its leftover
	// value, which the startup audit reads to tell the operator which language
	// to set, through any unrelated save -- and stops being warned once the
	// operator sets a language, 'simple' included, on purpose.
	var newCfg *string
	if newLang != curLang || (langSpecified && r.LanguageExplicit) {
		cfg, _ := LanguageConfig(newLang)
		newCfg = &cfg
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
		chunk_overlap=$4, top_k=$5, search_mode=$6, search_backend=$7, fts_config=COALESCE($8, fts_config), rerank=$9,
		rerank_candidates=$10, rerank_backend=$11, rerank_connection_id=$12,
		max_distance=$13, contextual_chunks=$14, max_documents=$15, max_bytes=$16,
		dimensions = CASE WHEN $18 THEN 0 ELSE dimensions END,
		language=$19, fts_language_applied=$20, fts_rebuild_cursor=$21 WHERE id=$17`,
		r.Name, r.EmbeddingConnectionID, r.ChunkSize, r.ChunkOverlap, r.TopK,
		r.SearchMode, r.SearchBackend, newCfg, r.Rerank, r.RerankCandidates,
		r.RerankBackend, r.RerankConnectionID, float32(r.MaxDistance), r.ContextualChunks,
		r.MaxDocuments, r.MaxBytes, r.ID, connChanged, newLang, newApplied, newCursor)
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

// FTSConfigWarning returns the operator-facing warning for a store that still
// carries a deprecated fts_config other than 'simple' while its language is
// 'simple', or "" when there is nothing to warn about. fts_config no longer
// does anything (ADR-027): the store is indexed and queried with 'simple' until
// its language is set, which is what the warning says -- and it names the
// language the old configuration stands for. A store on the pg_search backend
// is not warned: its BM25 index never read either setting.
func FTSConfigWarning(ftsConfig, language, backend string) string {
	cfg := strings.ToLower(strings.TrimSpace(ftsConfig))
	if cfg == "" || cfg == LanguageSimple || backend == BackendPgSearch {
		return ""
	}
	if language != "" && language != LanguageSimple {
		return ""
	}
	hint := "set the store's language to the language of its documents"
	if code, ok := LanguageForConfig(cfg); ok {
		hint = "set the store's language to '" + code + "'"
	}
	return "fts_config '" + cfg + "' is deprecated and no longer used: the store is indexed and queried with 'simple' " +
		"until it has a language; " + hint + " to stem its full-text search (documents are re-indexed in the background)"
}

// RAGStoreAudit is what AuditRAGStores found.
type RAGStoreAudit struct {
	// IndexMissing counts stores whose vectors have no usable HNSW index:
	// a width above MaxHalfvecIndexedDims (or above MaxIndexedVectorDims on
	// a pgvector without halfvec), and a width of 2001..4000 whose table has
	// no valid halfvec index.
	IndexMissing int
	// FTSMismatch counts stores that still carry an fts_config other than
	// 'simple' and have no language set.
	FTSMismatch int
}

// AuditRAGStores logs a warning for every store the current schema cannot
// serve as configured: a vector width without a usable HNSW index (searched
// with an exact scan) and a leftover fts_config other than 'simple' on a store
// whose language is still 'simple'. Neither store is
// changed or disabled -- a 2001..4000 store without its halfvec index is not
// migrated automatically, the warning names the reindex action. The
// index-missing count is kept for RAGIndexMissing. It runs at startup.
func (s *Store) AuditRAGStores(ctx context.Context) (RAGStoreAudit, error) {
	var out RAGStoreAudit
	stores, err := s.ListRAGStores(ctx)
	if err != nil {
		return out, err
	}
	// The raw column is read here and nowhere else: the stores above carry the
	// configuration derived from their language, which is not what a store
	// saved before 0027 was configured with.
	legacy, err := s.storedFTSConfigs(ctx)
	if err != nil {
		return out, err
	}
	for _, r := range stores {
		if r.Dimensions > MaxIndexedVectorDims {
			info, err := s.VectorIndex(ctx, r.Dimensions)
			if err != nil {
				return out, err
			}
			switch info.State {
			case VectorIndexNotIndexable:
				out.IndexMissing++
				s.log.Warn("rag store embedding is wider than an HNSW index can cover; it keeps working with an exact "+
					"scan but has no HNSW index -- move it to a model of 4000 dimensions or fewer; binary "+
					"quantization for wider models is planned for v0.9",
					"store_id", r.ID, "store", r.Name, "dimensions", r.Dimensions, "max_indexed", MaxHalfvecIndexedDims)
			case VectorIndexMissing, VectorIndexBuilding:
				out.IndexMissing++
				s.log.Warn("rag store has no halfvec HNSW index; it keeps working with an exact scan -- run the "+
					"reindex action (POST /admin/api/rag-stores/{id}/reindex) to index this width",
					"store_id", r.ID, "store", r.Name, "dimensions", r.Dimensions, "vector_index", info.State)
			}
		}
		if msg := FTSConfigWarning(legacy[r.ID], r.Language, r.SearchBackend); msg != "" {
			out.FTSMismatch++
			s.log.Warn("rag store: "+msg, "store_id", r.ID, "store", r.Name, "fts_config", legacy[r.ID],
				"language", r.Language)
		}
	}
	s.indexMissing.Store(int64(out.IndexMissing))
	s.indexMissingAt.Store(time.Now().UnixNano())
	return out, nil
}

// storedFTSConfigs reads the legacy rag_stores.fts_config column, by store id.
func (s *Store) storedFTSConfigs(ctx context.Context) (map[int64]string, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, fts_config FROM rag_stores")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var cfg string
		if err := rows.Scan(&id, &cfg); err != nil {
			return nil, err
		}
		out[id] = cfg
	}
	return out, rows.Err()
}

// indexMissingMaxAge is how old the index-missing count may get before a read
// recounts it. The count changes by events this process sees (a store deleted
// or re-bound, a build it ran finishing) and by events it does not (another
// replica finishing a reindex), so it cannot be kept by those alone.
const indexMissingMaxAge = 30 * time.Second

// indexMissingRecountTimeout bounds the recount a stale read makes, so a slow
// database delays one scrape by seconds at most.
const indexMissingRecountTimeout = 3 * time.Second

// RAGIndexMissing is the number of stores whose embeddings have no usable
// HNSW index. It is counted at startup, recounted whenever such a store is
// deleted or moves to another embedding connection and when a build this
// process ran finishes, and -- so that a reindex another replica ran is seen
// too -- recounted from the catalog by a read that finds the count older than
// 30 seconds. Concurrent reads share one recount; a failed one keeps the old
// value and is retried by the next read.
func (s *Store) RAGIndexMissing() int64 {
	ttl := s.indexMissingTTL
	if ttl == 0 {
		ttl = indexMissingMaxAge
	}
	if s.pool != nil && time.Since(time.Unix(0, s.indexMissingAt.Load())) >= ttl &&
		s.indexRefreshing.CompareAndSwap(false, true) {
		ctx, cancel := context.WithTimeout(context.Background(), indexMissingRecountTimeout)
		s.recountIndexMissing(ctx)
		cancel()
		s.indexRefreshing.Store(false)
	}
	return s.indexMissing.Load()
}

// recountIndexMissing refreshes RAGIndexMissing from the catalog. It is a
// recount rather than a decrement so the gauge cannot drift (or go below
// zero) when the startup audit failed. A failed recount keeps the old value.
func (s *Store) recountIndexMissing(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `SELECT dimensions, COUNT(*) FROM rag_stores
		WHERE dimensions > $1 GROUP BY dimensions`, MaxIndexedVectorDims)
	if err != nil {
		s.log.Warn("rag store: recount of stores without an HNSW index failed", "err", err)
		return
	}
	type width struct{ dims, stores int }
	var widths []width
	for rows.Next() {
		var w width
		if err := rows.Scan(&w.dims, &w.stores); err != nil {
			rows.Close()
			s.log.Warn("rag store: recount of stores without an HNSW index failed", "err", err)
			return
		}
		widths = append(widths, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.log.Warn("rag store: recount of stores without an HNSW index failed", "err", err)
		return
	}
	var n int64
	for _, w := range widths {
		info, err := s.VectorIndex(ctx, w.dims)
		if err != nil {
			s.log.Warn("rag store: recount of stores without an HNSW index failed", "err", err)
			return
		}
		if info.State != VectorIndexHalfvec {
			n += int64(w.stores)
		}
	}
	s.indexMissing.Store(n)
	s.indexMissingAt.Store(time.Now().UnixNano())
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
