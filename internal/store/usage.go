package store

import (
	"context"
	"time"
)

// Usage periods stored in project_usage.
const (
	PeriodMinute = "minute"
	PeriodDay    = "day"
	PeriodMonth  = "month"
)

// UsageRow is one project_usage counter row.
type UsageRow struct {
	Period           string
	PeriodStart      time.Time
	Requests         int
	PromptTokens     int64
	CompletionTokens int64
}

// Tokens is prompt plus completion tokens.
func (u UsageRow) Tokens() int64 { return u.PromptTokens + u.CompletionTokens }

// UsageWindows are the three period starts a request falls into.
type UsageWindows struct {
	Minute, Day, Month time.Time
}

// ReserveMinuteRequest atomically increments the minute request counter and
// returns the new value, so callers can decide whether the request fits.
func (s *Store) ReserveMinuteRequest(ctx context.Context, projectID int64, minute time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `INSERT INTO project_usage (project_id, period, period_start, requests)
		VALUES ($1, 'minute', $2, 1)
		ON CONFLICT (project_id, period, period_start) DO UPDATE SET requests = project_usage.requests + 1
		RETURNING requests`, projectID, minute).Scan(&n)
	return n, err
}

// ReleaseMinuteRequest undoes a reservation that was rejected, so the
// remaining-requests figure reflects only admitted requests.
func (s *Store) ReleaseMinuteRequest(ctx context.Context, projectID int64, minute time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE project_usage SET requests = GREATEST(requests - 1, 0)
		WHERE project_id = $1 AND period = 'minute' AND period_start = $2`, projectID, minute)
	return err
}

// GetUsage reads the minute, day and month rows in one query. Missing rows
// come back zeroed with the requested period start.
func (s *Store) GetUsage(ctx context.Context, projectID int64, w UsageWindows) (minute, day, month UsageRow, err error) {
	minute = UsageRow{Period: PeriodMinute, PeriodStart: w.Minute}
	day = UsageRow{Period: PeriodDay, PeriodStart: w.Day}
	month = UsageRow{Period: PeriodMonth, PeriodStart: w.Month}
	rows, err := s.pool.Query(ctx, `SELECT period, requests, prompt_tokens, completion_tokens FROM project_usage
		WHERE project_id = $1 AND ((period = 'minute' AND period_start = $2)
			OR (period = 'day' AND period_start = $3) OR (period = 'month' AND period_start = $4))`,
		projectID, w.Minute, w.Day, w.Month)
	if err != nil {
		return minute, day, month, err
	}
	defer rows.Close()
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Period, &r.Requests, &r.PromptTokens, &r.CompletionTokens); err != nil {
			return minute, day, month, err
		}
		switch r.Period {
		case PeriodMinute:
			minute.Requests, minute.PromptTokens, minute.CompletionTokens = r.Requests, r.PromptTokens, r.CompletionTokens
		case PeriodDay:
			day.Requests, day.PromptTokens, day.CompletionTokens = r.Requests, r.PromptTokens, r.CompletionTokens
		case PeriodMonth:
			month.Requests, month.PromptTokens, month.CompletionTokens = r.Requests, r.PromptTokens, r.CompletionTokens
		}
	}
	return minute, day, month, rows.Err()
}

// RecordUsage adds tokens (and one request) to the three period rows. The
// minute request counter is only bumped when countMinuteRequest is set,
// because ReserveMinuteRequest may already have done it.
func (s *Store) RecordUsage(ctx context.Context, projectID int64, w UsageWindows, promptTokens, completionTokens int, countMinuteRequest bool) error {
	minuteReq := 0
	if countMinuteRequest {
		minuteReq = 1
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO project_usage (project_id, period, period_start, requests, prompt_tokens, completion_tokens)
		VALUES ($1, 'minute', $2, $5, $6, $7), ($1, 'day', $3, 1, $6, $7), ($1, 'month', $4, 1, $6, $7)
		ON CONFLICT (project_id, period, period_start) DO UPDATE SET
			requests = project_usage.requests + EXCLUDED.requests,
			prompt_tokens = project_usage.prompt_tokens + EXCLUDED.prompt_tokens,
			completion_tokens = project_usage.completion_tokens + EXCLUDED.completion_tokens`,
		projectID, w.Minute, w.Day, w.Month, minuteReq, promptTokens, completionTokens)
	return err
}

// DeleteUsageBefore removes rows of one period that started before cutoff.
func (s *Store) DeleteUsageBefore(ctx context.Context, period string, cutoff time.Time) (int64, error) {
	res, err := s.pool.Exec(ctx, "DELETE FROM project_usage WHERE period = $1 AND period_start < $2", period, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}
