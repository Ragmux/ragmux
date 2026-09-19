package store

import (
	"context"
	"sync"
)

// Search backends for the lexical half of a hybrid search.
const (
	// BackendPgvector answers the lexical side with PostgreSQL's own full
	// text search: the generated chunks.tsv column and its GIN index.
	BackendPgvector = "pgvector"
	// BackendPgSearch answers it with ParadeDB's BM25 index (pg_search).
	BackendPgSearch = "pg_search"
)

// rrfK is the reciprocal rank fusion constant: score = 1/(rrfK+rank) summed
// over the two candidate lists. 60 is the value from the original RRF paper
// and the one every stored score was produced with.
//
// Fusion stays rank based on purpose, and a weighted sum of the raw scores is
// deliberately not offered:
//
//   - BM25 scores are unbounded and corpus dependent while a cosine distance
//     is bounded to [0,2]. Summing them needs per-query normalisation, and
//     normalisation is least stable exactly when one leg returns few
//     candidates -- the case where the fusion matters most.
//   - SearchHit.Score is a public contract. It is published in the
//     x-ragmux-rag-sources header, in ragmux.sources[].score, in the admin
//     search panel and in tests. Changing what it means per store is an API
//     change wearing a config flag as a disguise.
//   - The win a better lexical backend actually delivers is a better ordered
//     candidate list, and RRF consumes an ordering directly.
const rrfK = 60

// SearchParams is everything a backend needs to build the hybrid query. The
// values are passed to the driver as bind parameters; VecTable is the only
// piece that is interpolated, and it is derived from a validated integer
// dimension (see vecTable), never from user input.
type SearchParams struct {
	// VecTable is the chunk_embeddings_<dims> table of the store.
	VecTable string
	// Vector is the query embedding, ready to bind as $1.
	Vector any
	// StoreID scopes both candidate lists to one rag store.
	StoreID int64
	// Candidates caps each candidate list and the fused result.
	Candidates int
	// MaxDistance drops candidates further than this cosine distance; 0 = off.
	MaxDistance float64
	// FTSConfig is the text search configuration used to parse the query.
	// Only the pgvector backend reads it: the pg_search index is global and
	// tokenises at index time, so a per-store configuration has nothing to
	// select there.
	FTSConfig string
	// Query is the raw user query. Each backend prepares it its own way.
	Query string
}

// SearchBackend builds the SQL for one hybrid search.
//
// Only hybrid mode is pluggable. Vector-only retrieval is byte-identical
// under every backend -- it never touches the lexical index at all -- so
// Store.Search consults a backend exclusively when opts.Mode is
// SearchHybrid, and a backend never sees a vector-only search.
//
// Backends return SQL and arguments rather than rows on purpose: there stays
// exactly one scan loop and one column contract in Store.Search, so a new
// backend cannot quietly change what a SearchHit means.
type SearchBackend interface {
	// Name is the identifier stored in rag_stores.search_backend.
	Name() string
	// Available reports whether this server can run the backend at all.
	Available(caps Capabilities) bool
	// Prepare creates whatever the backend needs before its first query
	// (an index, typically). It is a no-op for pgvector.
	Prepare(ctx context.Context, s *Store) error
	// Invalidate drops whatever Prepare cached about this server, so the
	// next Prepare re-reads the database. Store.Search calls it when a
	// query fails: a backend whose index went away under a running process
	// is the one thing Prepare cannot notice on its own.
	Invalidate(s *Store)
	// HybridQuery builds the fused query. The result set must carry the
	// twelve columns Store.Search scans, in order: chunk_id, document_id,
	// idx, content, filename, section, page, distance, score, vector_rank,
	// fts_rank, lex_score.
	HybridQuery(p SearchParams) (sql string, args []any)
}

// searchBackends holds one instance of each backend. They are stateless
// builders; everything per-server lives on the Store.
var searchBackends = map[string]SearchBackend{
	BackendPgvector: pgvectorBackend{},
	BackendPgSearch: pgSearchBackend{},
}

// SearchBackendNames lists the known backends in a stable order.
var SearchBackendNames = []string{BackendPgvector, BackendPgSearch}

// IsValidSearchBackend reports whether name is a known backend.
func IsValidSearchBackend(name string) bool {
	_, ok := searchBackends[name]
	return ok
}

// SearchBackendAvailable reports whether this server can run a backend.
func (s *Store) SearchBackendAvailable(name string) bool {
	b, ok := searchBackends[name]
	return ok && b.Available(s.caps)
}

// SearchBackendReason explains why a backend cannot be used here; the empty
// string means it can.
func (s *Store) SearchBackendReason(name string) string {
	b, ok := searchBackends[name]
	if !ok {
		return "unknown search backend"
	}
	if b.Available(s.caps) {
		return ""
	}
	if name == BackendPgSearch && s.caps.PgSearchVersion != "" {
		return "the pg_search extension is installed (version " + s.caps.PgSearchVersion +
			") but does not speak the query API this build uses"
	}
	return "the pg_search extension is not installed on this PostgreSQL server"
}

// backendWarned remembers which backend warnings are already standing, so a
// degraded store costs one log line rather than one per query. At 50 requests
// a second an undeduplicated warning is three thousand lines a minute, which
// buries the one line an operator needs.
//
// The key pairs the Store with the rag store id and the reason: two Stores in
// one process (tests, or a re-Open simulating a restart) must not silence
// each other's first warning, and the three ways a backend can let a search
// down must not silence each other either.
var backendWarned sync.Map

type backendWarnKey struct {
	store  *Store
	rag    int64
	reason string
}

// Reasons a backend warning is filed under.
const (
	// warnUnavailable: this server cannot run the configured backend at
	// all. Capabilities are detected once at boot, so it cannot resolve
	// without a restart and the gate is never cleared.
	warnUnavailable = "unavailable"
	// warnPrepare: the backend's index could not be built.
	warnPrepare = "prepare"
	// warnQuery: the backend built its query and the server rejected it.
	warnQuery = "query"
)

// warnBackendOnce logs a backend warning unless the same one is already
// standing for this rag store.
func (s *Store) warnBackendOnce(storeID int64, reason, msg string, args ...any) {
	if _, warned := backendWarned.LoadOrStore(backendWarnKey{s, storeID, reason}, true); warned {
		return
	}
	s.log.Warn(msg, args...)
}

// clearBackendWarning reopens the gate for one reason, so a backend that
// recovers and then fails again is reported again instead of staying silent
// for the life of the process.
func (s *Store) clearBackendWarning(storeID int64, reason string) {
	backendWarned.Delete(backendWarnKey{s, storeID, reason})
}

// resolveBackend returns the backend a search should use. A store configured
// for a backend this server cannot run -- a dump restored onto a plain
// PostgreSQL, or a pg_search whose query API failed the boot smoke test --
// falls back to pgvector with one warning per store per process. Retrieval
// degrades; it never fails.
func (s *Store) resolveBackend(name string, storeID int64) SearchBackend {
	if name == "" {
		name = BackendPgvector
	}
	b, ok := searchBackends[name]
	if !ok {
		b = searchBackends[BackendPgvector]
		name = BackendPgvector
	}
	if b.Available(s.caps) {
		return b
	}
	s.warnBackendOnce(storeID, warnUnavailable,
		"rag store is configured for a search backend this server cannot run; falling back to pgvector",
		"rag_store", storeID, "backend", name, "reason", s.SearchBackendReason(name))
	return searchBackends[BackendPgvector]
}
