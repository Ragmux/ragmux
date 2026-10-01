// Package maintenance runs the periodic retention job that trims request
// logs, audit entries, login attempts, expired sessions and stale usage
// counters from the database.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/store"
)

// Defaults for the retention windows and the run schedule.
const (
	DefaultRequestLogDays = 90
	DefaultAuditDays      = 365
	// LoginAttemptRetention bounds how long login attempts are kept; the
	// login limiter only looks back minutes, so a day is plenty.
	LoginAttemptRetention = 24 * time.Hour
	// APIKeyRetention is how long a revoked or expired api key row is kept
	// before it is removed. Keys that request logs still attribute spend to
	// are never removed, whatever their age.
	APIKeyRetention = 30 * 24 * time.Hour
	// StartDelay is how long Run waits before its first pass so a freshly
	// started replica does not compete with its own migration and warm-up.
	StartDelay = time.Minute
	// DefaultInterval is the spacing between passes.
	DefaultInterval = time.Hour
	// DefaultBatchSize is how many rows one retention DELETE removes at
	// most. Small batches keep each statement's locks, WAL burst and
	// replication lag bounded however much has piled up.
	DefaultBatchSize = 5000
	// DefaultBatchPause is the wait between two batches of the same table,
	// so a long backlog leaves room for the traffic around it.
	DefaultBatchPause = 50 * time.Millisecond
)

// Janitor deletes rows that fell out of their retention window. Days values
// of 0 keep the corresponding table forever.
type Janitor struct {
	Store   *store.Store
	Limiter *limits.Limiter
	Log     *slog.Logger
	// Now is the clock used for cutoffs; nil means time.Now.
	Now            func() time.Time
	RequestLogDays int
	AuditDays      int
	// IngestMaxAttempts is INGEST_MAX_ATTEMPTS. Documents that reached it
	// are invisible to the ingestion claim, so the pass writes their final
	// failed status; 0 skips the step.
	IngestMaxAttempts int

	// batchSize and batchPause override DefaultBatchSize and
	// DefaultBatchPause when positive; tests set them.
	batchSize  int
	batchPause time.Duration
	// wait sleeps d between batches and reports false when ctx ended
	// first; nil uses a timer. Tests set it to cancel mid-loop.
	wait func(ctx context.Context, d time.Duration) bool
	// deleteBatch issues one bounded DELETE; nil uses
	// Store.DeleteExpiredBatch. Tests set it to cancel mid-statement.
	deleteBatch func(ctx context.Context, table store.RetentionTable, cutoff time.Time, limit int) (int64, error)
}

// Report counts the rows one pass removed.
type Report struct {
	RequestLogs   int64
	AuditLogs     int64
	LoginAttempts int64
	Sessions      int64
	// APIKeys counts retired api keys removed in the pass.
	APIKeys int64
	// UsagePurged reports whether the usage counter purge ran.
	UsagePurged bool
	// DocumentsFailed counts documents that ran out of ingestion attempts.
	DocumentsFailed int64
}

// Deleted is the number of rows the pass removed across every table.
// DocumentsFailed is an update, not a deletion, and is not counted.
func (r Report) Deleted() int64 {
	return r.RequestLogs + r.AuditLogs + r.LoginAttempts + r.Sessions + r.APIKeys
}

func (j *Janitor) now() time.Time {
	if j.Now != nil {
		return j.Now().UTC()
	}
	return time.Now().UTC()
}

func (j *Janitor) log() *slog.Logger {
	if j.Log != nil {
		return j.Log
	}
	return slog.Default()
}

// RunOnce performs a single retention pass. Unless ctx is cancelled, every
// step is attempted even when an earlier one fails; the errors are joined.
//
// Rows are deleted in batches (see deleteBatched). When ctx is cancelled the
// pass stops where it is and returns what it removed so far with a nil
// error: every step is an idempotent "delete what expired before the
// cutoff", so the next pass simply picks up the rest.
func (j *Janitor) RunOnce(ctx context.Context) (Report, error) {
	var rep Report
	var errs []error
	now := j.now()

	steps := []struct {
		table  store.RetentionTable
		label  string
		cutoff time.Time
		skip   bool
		into   *int64
	}{
		{store.RetainRequestLogs, "request logs", now.AddDate(0, 0, -j.RequestLogDays), j.RequestLogDays <= 0, &rep.RequestLogs},
		{store.RetainAuditLogs, "audit logs", now.AddDate(0, 0, -j.AuditDays), j.AuditDays <= 0, &rep.AuditLogs},
		{store.RetainLoginAttempts, "login attempts", now.Add(-LoginAttemptRetention), false, &rep.LoginAttempts},
		{store.RetainSessions, "sessions", now, false, &rep.Sessions},
		{store.RetainAPIKeys, "api keys", now.Add(-APIKeyRetention), false, &rep.APIKeys},
	}
	for _, st := range steps {
		if st.skip {
			continue
		}
		n, _, err := j.deleteBatched(ctx, st.table, st.cutoff)
		*st.into = n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", st.label, err))
		}
		if ctx.Err() != nil {
			return rep, errors.Join(errs...)
		}
	}
	if j.Limiter != nil {
		if err := j.Limiter.PurgeUsage(ctx); err != nil {
			if ctx.Err() != nil {
				return rep, errors.Join(errs...)
			}
			errs = append(errs, fmt.Errorf("usage counters: %w", err))
		} else {
			rep.UsagePurged = true
		}
	}
	if j.IngestMaxAttempts > 0 {
		n, err := j.Store.FailExhaustedDocuments(ctx, j.IngestMaxAttempts)
		if err != nil && ctx.Err() == nil {
			errs = append(errs, fmt.Errorf("exhausted documents: %w", err))
		}
		rep.DocumentsFailed = n
	}
	return rep, errors.Join(errs...)
}

// deleteBatched removes every row of table that expired before cutoff, one
// bounded DELETE at a time, pausing between batches. It stops when a batch
// comes back short. It reports the rows removed and the statements issued.
//
// A cancelled ctx is not an error: the loop returns what it removed so far
// and a nil error, and the remainder goes on the next pass.
func (j *Janitor) deleteBatched(ctx context.Context, table store.RetentionTable, cutoff time.Time) (int64, int, error) {
	size, pause := j.batching()
	var total int64
	batches := 0
	for {
		if ctx.Err() != nil {
			return total, batches, nil
		}
		n, err := j.deleteOne(ctx, table, cutoff, size)
		if err != nil {
			if ctx.Err() != nil {
				return total, batches, nil
			}
			return total, batches, err
		}
		total += n
		batches++
		if n < int64(size) {
			return total, batches, nil
		}
		if !j.sleep(ctx, pause) {
			return total, batches, nil
		}
	}
}

func (j *Janitor) deleteOne(ctx context.Context, table store.RetentionTable, cutoff time.Time, limit int) (int64, error) {
	if j.deleteBatch != nil {
		return j.deleteBatch(ctx, table, cutoff, limit)
	}
	return j.Store.DeleteExpiredBatch(ctx, table, cutoff, limit)
}

func (j *Janitor) batching() (int, time.Duration) {
	size, pause := DefaultBatchSize, DefaultBatchPause
	if j.batchSize > 0 {
		size = j.batchSize
	}
	if j.batchPause > 0 {
		pause = j.batchPause
	}
	return size, pause
}

// sleep waits d unless ctx ends first, and reports whether it waited.
func (j *Janitor) sleep(ctx context.Context, d time.Duration) bool {
	if j.wait != nil {
		return j.wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Run executes RunOnce StartDelay after it is called and then every interval
// until ctx is cancelled. A non-positive interval uses DefaultInterval.
//
// The first pass is jittered over another StartDelay so a fleet that was
// restarted together does not wake up together. That is cosmetic - only one
// replica does the work anyway - but it keeps the logs and the database
// load readable.
func (j *Janitor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	first := time.NewTimer(StartDelay + rand.N(StartDelay))
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	j.runLeader(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.runLeader(ctx)
		}
	}
}

// runLeader performs one pass unless another replica is already doing it,
// and reports whether this replica ran it.
//
// The election is pg_try_advisory_lock. The alternative, a leader_election
// table holding a lease row, needs schema, a renewal loop and a window
// where a dead leader's lease has not expired yet; a session advisory lock
// needs none of that and disappears the moment the connection does, which
// is also how the rest of the codebase serialises cluster-wide work.
//
// Every step of the pass is an idempotent, batched "DELETE ... WHERE
// created_at < cutoff", so the lock saves duplicated work rather than
// protecting correctness. That is why a connection pooler in transaction mode - where
// a session lock cannot be held and every replica sees the lock as free -
// costs a duplicated pass and nothing else.
func (j *Janitor) runLeader(ctx context.Context) bool {
	release, ok, err := j.Store.TryAdvisoryLock(ctx, store.LockJanitor)
	if err != nil {
		if ctx.Err() == nil {
			j.log().Warn("retention leader lock", "err", err)
		}
		return false
	}
	if !ok {
		j.log().Debug("retention pass skipped: another replica holds the leader lock")
		return false
	}
	defer release()
	j.runLogged(ctx)
	return true
}

func (j *Janitor) runLogged(ctx context.Context) {
	start := time.Now()
	rep, err := j.RunOnce(ctx)
	if err != nil {
		j.log().Warn("retention pass failed", "err", err)
	}
	j.log().Info("retention pass", "deleted", rep.Deleted(), "duration", time.Since(start),
		"interrupted", ctx.Err() != nil,
		"request_logs", rep.RequestLogs, "audit_logs", rep.AuditLogs,
		"login_attempts", rep.LoginAttempts, "sessions", rep.Sessions, "api_keys", rep.APIKeys,
		"usage_purged", rep.UsagePurged, "documents_failed", rep.DocumentsFailed)
}
