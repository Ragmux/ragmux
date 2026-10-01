package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RetentionTable names one table the retention job trims by age. Each value
// carries a fixed predicate against a single cutoff; the set of tables and
// what makes a row expired are decided here and nowhere else, so the batched
// janitor pass and the one-shot Delete*/Purge* helpers can never disagree.
type RetentionTable int

// The tables subject to retention. Usage counters (limits) and ingestion
// state are not row deletions by age and are not listed.
const (
	RetainRequestLogs RetentionTable = iota + 1
	RetainAuditLogs
	RetainLoginAttempts
	RetainSessions
	// RetainAPIKeys removes retired keys only while no request log
	// attributes spend to them; see PurgeRetiredAPIKeys.
	RetainAPIKeys
)

type retentionSpec struct {
	table string
	// where is the expiry predicate; $1 is the cutoff.
	where string
	// order, when set, is an indexed column the batch is picked in order
	// of. Without it the planner answers a large backlog with a sequential
	// scan + LIMIT that re-reads every dead tuple earlier batches left at
	// the head of the heap, so each batch costs more than the last; walking
	// the index keeps a batch's reads bounded. Small tables leave it empty.
	order string
}

var retentionSpecs = map[RetentionTable]retentionSpec{
	RetainRequestLogs:   {"request_logs", "created_at < $1", "created_at"}, // idx_request_logs_created
	RetainAuditLogs:     {"audit_logs", "created_at < $1", "created_at"},   // idx_audit_logs_created
	RetainLoginAttempts: {"login_attempts", "created_at < $1", ""},         // kept a day; small
	RetainSessions:      {"sessions", "expires_at <= $1", "expires_at"},    // idx_sessions_expires
	// The NOT EXISTS is what makes the ON DELETE SET NULL on
	// request_logs.api_key_id safe: attribution is never erased, the row
	// just outlives the key it names.
	RetainAPIKeys: {"api_keys",
		"(revoked_at < $1 OR expires_at < $1) AND NOT EXISTS (SELECT 1 FROM request_logs WHERE api_key_id = api_keys.id)", ""},
}

func (t RetentionTable) String() string {
	if spec, ok := retentionSpecs[t]; ok {
		return spec.table
	}
	return fmt.Sprintf("RetentionTable(%d)", int(t))
}

// ErrBatchLimit is returned for a non-positive batch limit: a retention
// delete is always bounded, there is no "everything at once" spelling.
var ErrBatchLimit = errors.New("store: retention batch limit must be positive")

// DeleteExpiredBatch deletes at most limit rows of table that expired before
// cutoff and reports how many it removed. A result smaller than limit means
// nothing expired is left (as of the statement's snapshot).
//
// The batch is picked by ctid so each statement locks and writes a bounded
// number of rows: one unbounded DELETE over a large table holds its locks
// for the whole run, produces one huge WAL burst and stalls replicas. Where
// the spec names an order column the batch is picked along its index, which
// keeps the read side of every batch bounded too (see retentionSpec.order).
//
// Table names, predicates and order columns come from the fixed
// retentionSpecs map; only the cutoff and the limit are parameters.
func (s *Store) DeleteExpiredBatch(ctx context.Context, table RetentionTable, cutoff time.Time, limit int) (int64, error) {
	spec, ok := retentionSpecs[table]
	if !ok {
		return 0, fmt.Errorf("store: unknown retention table %d", int(table))
	}
	if limit <= 0 {
		return 0, ErrBatchLimit
	}
	tag, err := s.pool.Exec(ctx, batchDeleteSQL(spec), cutoff.UTC(), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func batchDeleteSQL(spec retentionSpec) string {
	pick := "SELECT ctid FROM " + spec.table + " WHERE " + spec.where
	if spec.order != "" {
		pick += " ORDER BY " + spec.order
	}
	return "DELETE FROM " + spec.table + " WHERE ctid IN (" + pick + " LIMIT $2)"
}

// oneShotBatchSize is the batch size of deleteAllExpired.
const oneShotBatchSize = 5000

// deleteAllExpired removes every expired row of table, batch after batch
// with no pause in between, and reports how many it removed. It backs the
// Delete*Before / Purge* helpers, which are for tests and one-off tooling;
// the retention job paces its own loop (maintenance.Janitor).
func (s *Store) deleteAllExpired(ctx context.Context, table RetentionTable, cutoff time.Time) (int64, error) {
	var total int64
	for {
		n, err := s.DeleteExpiredBatch(ctx, table, cutoff, oneShotBatchSize)
		total += n
		if err != nil || n < oneShotBatchSize {
			return total, err
		}
	}
}
