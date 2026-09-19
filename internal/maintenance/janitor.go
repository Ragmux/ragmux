// Package maintenance runs the periodic retention job that trims request
// logs, audit entries, login attempts, expired sessions and stale usage
// counters from the database.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// RunOnce performs a single retention pass. Every step is attempted even
// when an earlier one fails; the errors are joined.
func (j *Janitor) RunOnce(ctx context.Context) (Report, error) {
	var rep Report
	var errs []error
	now := j.now()

	if j.RequestLogDays > 0 {
		n, err := j.Store.DeleteRequestLogsBefore(ctx, now.AddDate(0, 0, -j.RequestLogDays))
		if err != nil {
			errs = append(errs, fmt.Errorf("request logs: %w", err))
		}
		rep.RequestLogs = n
	}
	if j.AuditDays > 0 {
		n, err := j.Store.DeleteAuditLogsBefore(ctx, now.AddDate(0, 0, -j.AuditDays))
		if err != nil {
			errs = append(errs, fmt.Errorf("audit logs: %w", err))
		}
		rep.AuditLogs = n
	}
	n, err := j.Store.DeleteLoginAttemptsBefore(ctx, now.Add(-LoginAttemptRetention))
	if err != nil {
		errs = append(errs, fmt.Errorf("login attempts: %w", err))
	}
	rep.LoginAttempts = n
	n, err = j.Store.PurgeExpiredSessions(ctx, now)
	if err != nil {
		errs = append(errs, fmt.Errorf("sessions: %w", err))
	}
	rep.Sessions = n
	n, err = j.Store.PurgeRetiredAPIKeys(ctx, now.Add(-APIKeyRetention))
	if err != nil {
		errs = append(errs, fmt.Errorf("api keys: %w", err))
	}
	rep.APIKeys = n
	if j.Limiter != nil {
		if err := j.Limiter.PurgeUsage(ctx); err != nil {
			errs = append(errs, fmt.Errorf("usage counters: %w", err))
		} else {
			rep.UsagePurged = true
		}
	}
	return rep, errors.Join(errs...)
}

// Run executes RunOnce StartDelay after it is called and then every interval
// until ctx is cancelled. A non-positive interval uses DefaultInterval.
func (j *Janitor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	first := time.NewTimer(StartDelay)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	j.runLogged(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.runLogged(ctx)
		}
	}
}

func (j *Janitor) runLogged(ctx context.Context) {
	rep, err := j.RunOnce(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		j.log().Warn("retention pass failed", "err", err)
	}
	j.log().Info("retention pass", "request_logs", rep.RequestLogs, "audit_logs", rep.AuditLogs,
		"login_attempts", rep.LoginAttempts, "sessions", rep.Sessions, "api_keys", rep.APIKeys,
		"usage_purged", rep.UsagePurged)
}
