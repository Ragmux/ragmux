package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The halfvec index of an embedding table (ADR-026). The table keeps its
// vector(N) column; widths of 2001..4000, which the vector type cannot index,
// get an HNSW index on the expression embedding::halfvec(N) instead. Nothing
// is stored about it: whether the index exists, is valid or is being built is
// read from the catalog every time.

// Values of the vector_index field of an admin store response.
const (
	// VectorIndexHNSW is the plain vector HNSW index (width up to 2000).
	VectorIndexHNSW = "hnsw"
	// VectorIndexHalfvec is the halfvec expression index (2001..4000).
	VectorIndexHalfvec = "hnsw_halfvec"
	// VectorIndexBuilding means the halfvec index is being built right now.
	VectorIndexBuilding = "building"
	// VectorIndexMissing means a 2001..4000 width has no valid halfvec index:
	// it is searched with an exact scan until it is reindexed.
	VectorIndexMissing = "missing"
	// VectorIndexNotIndexable means the width cannot be indexed at all (above
	// 4000, or above 2000 on a pgvector without halfvec).
	VectorIndexNotIndexable = "not_indexable"
)

// ErrVectorIndexPresent is returned by StartVectorReindex when there is
// nothing to build: the width already has its index, or needs none.
var ErrVectorIndexPresent = errors.New("embedding width is already indexed")

// ErrVectorNotIndexable is returned by StartVectorReindex for a width no
// index can be built for.
var ErrVectorNotIndexable = errors.New("embedding width cannot be indexed")

// ErrVectorNotBound is returned by StartVectorReindex for a store that has no
// embedding width yet: there is no index to speak of.
var ErrVectorNotBound = errors.New("store is not bound to an embedding width")

// queryRower is what the pool, a transaction and a single connection share.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// halfvecIndexable reports whether a width of dims dimensions can carry a
// halfvec HNSW index on this server.
func halfvecIndexable(dims int, caps Capabilities) bool {
	return dims > MaxIndexedVectorDims && dims <= MaxHalfvecIndexedDims && caps.SupportsHalfvec()
}

func halfvecIndexName(dims int) string { return vecTable(dims) + "_hnsw_half" }

// halfvecIndexDDL is the statement that builds the halfvec index of a width.
// The name and the width are derived from a validated integer, never from
// input. A concurrent build cannot run inside a transaction.
func halfvecIndexDDL(dims int, concurrently bool) string {
	conc := ""
	if concurrently {
		conc = "CONCURRENTLY "
	}
	return fmt.Sprintf("CREATE INDEX %sIF NOT EXISTS %s ON %s USING hnsw ((embedding::halfvec(%d)) halfvec_cosine_ops)",
		conc, halfvecIndexName(dims), vecTable(dims), dims)
}

// halfvecCatalog is what the catalog says about the halfvec index of a width.
type halfvecCatalog struct {
	tableExists bool
	indexExists bool
	valid       bool
	// inProgress is a pg_stat_progress_create_index row for the index.
	inProgress bool
	percent    int
	// locked is the reindex advisory lock of the width, held by whichever
	// replica is building (or about to build) the index.
	locked bool
}

type halfvecState int

const (
	halfvecNoTable  halfvecState = iota // the embedding table does not exist (yet)
	halfvecAbsent                       // the table exists, the index does not
	halfvecValid                        // the index is usable
	halfvecBuilding                     // a build is running
	halfvecBroken                       // an invalid index and nobody building it: a build was cut short
)

func (c halfvecCatalog) state() halfvecState {
	switch {
	case !c.tableExists:
		return halfvecNoTable
	case c.indexExists && c.valid:
		return halfvecValid
	case c.inProgress || c.locked:
		return halfvecBuilding
	case c.indexExists:
		return halfvecBroken
	}
	return halfvecAbsent
}

// readHalfvecCatalog reads the state from the catalog; it is the whole
// "no state is kept" rule of ADR-026. pg_stat_progress_create_index is what
// tells a running build from an abandoned one (both leave indisvalid false);
// the advisory lock covers the moment before the build has created its
// catalog row and the replicas that are not this one.
func readHalfvecCatalog(ctx context.Context, q queryRower, dims int) (halfvecCatalog, error) {
	var c halfvecCatalog
	err := q.QueryRow(ctx, `
		SELECT to_regclass($1::text) IS NOT NULL,
		       i.indexrelid IS NOT NULL,
		       COALESCE(i.indisvalid, false),
		       EXISTS (SELECT 1 FROM pg_stat_progress_create_index p WHERE p.index_relid = i.indexrelid),
		       COALESCE((SELECT CASE WHEN p.tuples_total > 0
		                             THEN LEAST(100, p.tuples_done * 100 / p.tuples_total) ELSE 0 END
		                   FROM pg_stat_progress_create_index p WHERE p.index_relid = i.indexrelid LIMIT 1), 0),
		       EXISTS (SELECT 1 FROM pg_locks l
		                WHERE l.locktype = 'advisory' AND l.granted
		                  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		                  AND l.classid::bigint = $3::bigint AND l.objid::bigint = $4::bigint AND l.objsubid = 2)
		FROM (SELECT 1) one
		LEFT JOIN pg_index i ON i.indexrelid = to_regclass($2::text) AND i.indrelid = to_regclass($1::text)`,
		vecTable(dims), halfvecIndexName(dims), int64(lockVecReindex), int64(dims),
	).Scan(&c.tableExists, &c.indexExists, &c.valid, &c.inProgress, &c.percent, &c.locked)
	return c, err
}

func (s *Store) halfvecIndexState(ctx context.Context, q queryRower, dims int) (halfvecState, error) {
	c, err := readHalfvecCatalog(ctx, q, dims)
	if err != nil {
		return halfvecNoTable, err
	}
	return c.state(), nil
}

// orphanVecTable reports whether a table can be indexed without a reindex
// action: it holds no vector and no store is bound to its width.
func (s *Store) orphanVecTable(ctx context.Context, q queryRower, dims int) (bool, error) {
	var orphan bool
	err := q.QueryRow(ctx, fmt.Sprintf(`SELECT NOT EXISTS (SELECT 1 FROM %s)
		AND NOT EXISTS (SELECT 1 FROM rag_stores WHERE dimensions = $1)`, vecTable(dims)), dims).Scan(&orphan)
	return orphan, err
}

// CheckBindable reports whether a store can be bound to an embedding of dims
// dimensions, for the first time (store creation, a connection change of an
// empty store, the first ingest). It refuses a width no index can be built
// for (ErrVectorDimsUnsupported) and a 2001..4000 width whose table exists
// but has no valid halfvec index (ErrVectorIndexMissing): a new store must
// not land on an exact scan, and the existing ones are not migrated behind
// the operator's back, so the width is reindexed first.
func (s *Store) CheckBindable(ctx context.Context, dims int) error {
	return s.checkBindable(ctx, s.pool, dims)
}

func (s *Store) checkBindable(ctx context.Context, q queryRower, dims int) error {
	if err := s.caps.checkVectorDims(dims); err != nil {
		return err
	}
	if !halfvecIndexable(dims, s.caps) {
		return nil
	}
	state, err := s.halfvecIndexState(ctx, q, dims)
	if err != nil {
		return err
	}
	var why string
	switch state {
	case halfvecNoTable, halfvecValid:
		return nil // born indexed (ensureVecTable), or indexed
	case halfvecAbsent:
		orphan, err := s.orphanVecTable(ctx, q, dims)
		if err != nil || orphan {
			return err
		}
		why = "it has no halfvec index"
	case halfvecBuilding:
		why = "its halfvec index is still being built"
	default:
		why = "its halfvec index build was interrupted"
	}
	return fmt.Errorf("%w: the %d-dimension embedding table already exists and %s; reindex this width first "+
		"(the reindex action of a store with %d dimensions, POST /admin/api/rag-stores/{id}/reindex), then bind "+
		"the new store", ErrVectorIndexMissing, dims, why, dims)
}

// VectorIndexInfo is the vector index state of one width, as the admin API
// reports it.
type VectorIndexInfo struct {
	// State is one of the VectorIndex* values; empty for an unbound store.
	State string
	// ProgressPercent is 0..100 while State is VectorIndexBuilding.
	ProgressPercent int
}

// VectorIndex reads the vector index state of a width (0 = not bound).
func (s *Store) VectorIndex(ctx context.Context, dims int) (VectorIndexInfo, error) {
	switch {
	case dims < 1:
		return VectorIndexInfo{}, nil
	case dims <= MaxIndexedVectorDims:
		return VectorIndexInfo{State: VectorIndexHNSW}, nil
	}
	if !halfvecIndexable(dims, s.caps) {
		return VectorIndexInfo{State: VectorIndexNotIndexable}, nil
	}
	c, err := readHalfvecCatalog(ctx, s.pool, dims)
	if err != nil {
		return VectorIndexInfo{}, err
	}
	if c.state() == halfvecValid {
		return VectorIndexInfo{State: VectorIndexHalfvec}, nil
	}
	if c.state() == halfvecBuilding {
		return VectorIndexInfo{State: VectorIndexBuilding, ProgressPercent: c.percent}, nil
	}
	return VectorIndexInfo{State: VectorIndexMissing}, nil
}

// AnnotateVectorIndex fills VectorIndex and VectorIndexProgress of the given
// stores. It costs a catalog read per distinct wide width, which is why the
// stores do not carry it from GetRAGStore: that runs on every search.
func (s *Store) AnnotateVectorIndex(ctx context.Context, stores ...*RAGStore) error {
	seen := map[int]VectorIndexInfo{}
	for _, r := range stores {
		info, ok := seen[r.Dimensions]
		if !ok {
			var err error
			if info, err = s.VectorIndex(ctx, r.Dimensions); err != nil {
				return err
			}
			seen[r.Dimensions] = info
		}
		r.VectorIndex, r.VectorIndexProgress = info.State, nil
		if info.State == VectorIndexBuilding {
			p := info.ProgressPercent
			r.VectorIndexProgress = &p
		}
	}
	return nil
}

// StoresWithDimensions lists the stores bound to a width: the ones a reindex
// of that width affects.
func (s *Store) StoresWithDimensions(ctx context.Context, dims int) ([]*RAGStore, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+ragCols+" FROM rag_stores r WHERE r.dimensions = $1 ORDER BY r.name", dims)
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

// StartVectorReindex starts building the halfvec index of a width in the
// background and returns at once. The build is detached from the caller: it
// runs CREATE INDEX CONCURRENTLY on a connection of its own, so searches and
// ingests go on (slowly, exactly) while it runs, and it outlives the request.
//
//   - A width of 2000 or less is indexed already (ErrVectorIndexPresent); so
//     is one whose halfvec index is valid.
//   - A width above 4000, or above 2000 without halfvec, cannot be indexed
//     (ErrVectorNotIndexable).
//   - A build that is already running is not started twice; the call returns
//     its state.
//   - An invalid index with no build behind it (the server restarted, the
//     build was cancelled) is dropped first and built again.
//
// The returned info is the state right after the call.
func (s *Store) StartVectorReindex(ctx context.Context, dims int) (VectorIndexInfo, error) {
	switch {
	case dims < 1:
		return VectorIndexInfo{}, fmt.Errorf("%w: the store has not been bound to an embedding width yet "+
			"(its first document sets it), so there is nothing to index", ErrVectorNotBound)
	case dims <= MaxIndexedVectorDims:
		return VectorIndexInfo{}, fmt.Errorf("%w: %d dimensions need no halfvec index", ErrVectorIndexPresent, dims)
	case !halfvecIndexable(dims, s.caps):
		return VectorIndexInfo{}, s.notIndexableErr(dims)
	}
	c, err := readHalfvecCatalog(ctx, s.pool, dims)
	if err != nil {
		return VectorIndexInfo{}, err
	}
	if !c.tableExists {
		// A bound store whose table is gone is not a case that arises, but
		// the build below would fail on it; the table is recreated born
		// indexed. ensureVecTable is called for this case only: on a table
		// that exists it would have no business here while a build runs.
		if err := s.ensureVecTable(ctx, dims); err != nil {
			return VectorIndexInfo{}, err
		}
		if c, err = readHalfvecCatalog(ctx, s.pool, dims); err != nil {
			return VectorIndexInfo{}, err
		}
	}
	switch c.state() {
	case halfvecValid:
		return VectorIndexInfo{}, fmt.Errorf("%w: the halfvec index of the %d-dimension table is valid", ErrVectorIndexPresent, dims)
	case halfvecBuilding:
		return VectorIndexInfo{State: VectorIndexBuilding, ProgressPercent: c.percent}, nil
	}

	pc, err := s.pool.Acquire(ctx)
	if err != nil {
		return VectorIndexInfo{}, err
	}
	conn := pc.Hijack() // a build holds its connection for minutes; it never goes back to the pool
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1, $2)", lockVecReindex, int32(dims)).Scan(&got); err != nil { //nolint:gosec // G115: dims is a bound embedding width, at most 4000
		_ = conn.Close(context.Background())
		return VectorIndexInfo{}, err
	}
	if !got { // another replica started it a moment ago
		_ = conn.Close(context.Background())
		return VectorIndexInfo{State: VectorIndexBuilding}, nil
	}
	s.bgWG.Add(1)
	go s.buildHalfvecIndex(conn, dims)
	return VectorIndexInfo{State: VectorIndexBuilding}, nil
}

func (s *Store) notIndexableErr(dims int) error {
	if dims > MaxHalfvecIndexedDims {
		return fmt.Errorf("%w: %d dimensions is above the %d an HNSW index can cover; binary quantization is planned for v0.9",
			ErrVectorNotIndexable, dims, MaxHalfvecIndexedDims)
	}
	return fmt.Errorf("%w: halfvec indexes need pgvector 0.7.0 or later (installed: %q)", ErrVectorNotIndexable, s.caps.PgvectorVersion)
}

// buildHalfvecIndex is the detached half of StartVectorReindex. It owns conn
// and the advisory lock on it; closing the connection releases the lock.
func (s *Store) buildHalfvecIndex(conn *pgx.Conn, dims int) {
	defer s.bgWG.Done()
	defer func() { _ = conn.Close(context.Background()) }()
	ctx := s.bg
	log := s.log.With("dimensions", dims, "index", halfvecIndexName(dims))

	// The lock is ours, so an invalid index with no progress row is an
	// abandoned build, not a running one.
	c, err := readHalfvecCatalog(ctx, conn, dims)
	if err != nil {
		log.Error("halfvec index build: read catalog", "err", err)
		return
	}
	if c.indexExists && c.valid {
		return
	}
	if c.indexExists && !c.inProgress {
		log.Info("halfvec index build: dropping the invalid index a cut-short build left behind")
		if _, err := conn.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+halfvecIndexName(dims)); err != nil {
			log.Error("halfvec index build: drop invalid index", "err", err)
			return
		}
	}
	log.Info("halfvec index build started")
	if _, err := conn.Exec(ctx, halfvecIndexDDL(dims, true)); err != nil {
		if ctx.Err() != nil {
			log.Info("halfvec index build cancelled by shutdown; the next reindex cleans up")
		} else {
			log.Error("halfvec index build failed; the next reindex cleans up", "err", err)
		}
		return
	}
	log.Info("halfvec index build finished")
	// The gauge counts stores without a valid index; this one just got it.
	s.recountIndexMissing(ctx)
}
