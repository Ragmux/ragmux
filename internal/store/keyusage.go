package store

import (
	"context"
	"time"
)

// key_usage mirrors project_usage row for row, so the two limit tiers are
// counted by the same code shape and purged by the same retention windows.

// ReserveKeyMinuteRequest atomically increments a key's minute request
// counter and returns the new value.
func (s *Store) ReserveKeyMinuteRequest(ctx context.Context, keyID int64, minute time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `INSERT INTO key_usage (api_key_id, period, period_start, requests)
		VALUES ($1, 'minute', $2, 1)
		ON CONFLICT (api_key_id, period, period_start) DO UPDATE SET requests = key_usage.requests + 1
		RETURNING requests`, keyID, minute).Scan(&n)
	return n, err
}

// ReleaseKeyMinuteRequest undoes a reservation that was rejected.
func (s *Store) ReleaseKeyMinuteRequest(ctx context.Context, keyID int64, minute time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE key_usage SET requests = GREATEST(requests - 1, 0)
		WHERE api_key_id = $1 AND period = 'minute' AND period_start = $2`, keyID, minute)
	return err
}

// GetKeyUsage reads a key's minute, day and month rows in one query.
func (s *Store) GetKeyUsage(ctx context.Context, keyID int64, w UsageWindows) (minute, day, month UsageRow, err error) {
	minute = UsageRow{Period: PeriodMinute, PeriodStart: w.Minute}
	day = UsageRow{Period: PeriodDay, PeriodStart: w.Day}
	month = UsageRow{Period: PeriodMonth, PeriodStart: w.Month}
	rows, err := s.pool.Query(ctx, `SELECT period, requests, prompt_tokens, completion_tokens FROM key_usage
		WHERE api_key_id = $1 AND ((period = 'minute' AND period_start = $2)
			OR (period = 'day' AND period_start = $3) OR (period = 'month' AND period_start = $4))`,
		keyID, w.Minute, w.Day, w.Month)
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

// RecordKeyUsage adds a finished request's tokens to a key's three period
// rows; the minute request counter is only bumped when the slot was not
// already reserved.
func (s *Store) RecordKeyUsage(ctx context.Context, keyID int64, w UsageWindows, promptTokens, completionTokens int, countMinuteRequest bool) error {
	minuteReq := 0
	if countMinuteRequest {
		minuteReq = 1
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO key_usage (api_key_id, period, period_start, requests, prompt_tokens, completion_tokens)
		VALUES ($1, 'minute', $2, $5, $6, $7), ($1, 'day', $3, 1, $6, $7), ($1, 'month', $4, 1, $6, $7)
		ON CONFLICT (api_key_id, period, period_start) DO UPDATE SET
			requests = key_usage.requests + EXCLUDED.requests,
			prompt_tokens = key_usage.prompt_tokens + EXCLUDED.prompt_tokens,
			completion_tokens = key_usage.completion_tokens + EXCLUDED.completion_tokens`,
		keyID, w.Minute, w.Day, w.Month, minuteReq, promptTokens, completionTokens)
	return err
}

// DeleteKeyUsageBefore removes rows of one period that started before cutoff.
func (s *Store) DeleteKeyUsageBefore(ctx context.Context, period string, cutoff time.Time) (int64, error) {
	res, err := s.pool.Exec(ctx, "DELETE FROM key_usage WHERE period = $1 AND period_start < $2", period, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// MinuteKeyTokensSince sums a key's minute rows starting at or after since;
// the budget forecast projects from it.
func (s *Store) MinuteKeyTokensSince(ctx context.Context, keyID int64, since time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0) FROM key_usage
		WHERE api_key_id = $1 AND period = 'minute' AND period_start >= $2`, keyID, since.UTC()).Scan(&n)
	return n, err
}
