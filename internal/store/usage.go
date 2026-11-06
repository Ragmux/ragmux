package store

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
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
//
// costMicros is the request's estimated cost in USD millionths and unpriced
// says its model had no price (the cost is then 0). Both go on the day and
// month rows only: the minute rows are the rate limiter's and carry no money
// (ADR-036). A negative cost is stored as 0.
func (s *Store) RecordUsage(ctx context.Context, projectID int64, w UsageWindows, promptTokens, completionTokens int, costMicros int64, unpriced, countMinuteRequest bool) error {
	minuteReq := 0
	if countMinuteRequest {
		minuteReq = 1
	}
	unpricedReq := 0
	if unpriced {
		unpricedReq = 1
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO project_usage
			(project_id, period, period_start, requests, prompt_tokens, completion_tokens, cost_micros, unpriced_requests)
		VALUES ($1, 'minute', $2, $5, $6, $7, 0, 0), ($1, 'day', $3, 1, $6, $7, $8, $9), ($1, 'month', $4, 1, $6, $7, $8, $9)
		ON CONFLICT (project_id, period, period_start) DO UPDATE SET
			requests = project_usage.requests + EXCLUDED.requests,
			prompt_tokens = project_usage.prompt_tokens + EXCLUDED.prompt_tokens,
			completion_tokens = project_usage.completion_tokens + EXCLUDED.completion_tokens,
			cost_micros = project_usage.cost_micros + EXCLUDED.cost_micros,
			unpriced_requests = project_usage.unpriced_requests + EXCLUDED.unpriced_requests`,
		projectID, w.Minute, w.Day, w.Month, minuteReq, promptTokens, completionTokens, max(costMicros, 0), unpricedReq)
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

// MinuteTokensSince sums the tokens of a project's minute rows starting at or
// after since; the budget forecast projects from it.
func (s *Store) MinuteTokensSince(ctx context.Context, projectID int64, since time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0) FROM project_usage
		WHERE project_id = $1 AND period = 'minute' AND period_start >= $2`, projectID, since.UTC()).Scan(&n)
	return n, err
}

// BudgetUsage is one subject's token budgets next to what it has used in the
// current UTC day and month: a project (KeyID 0) or an API key (KeyID set,
// ProjectID the project it is granted). A limit of 0 means that budget is not
// set. MonthlyCostMicros and MonthlyUnpriced are the project's estimated spend
// this month and the requests that had no price; they are 0 on a key row.
type BudgetUsage struct {
	ProjectID, KeyID                   int64
	DailyUsed, DailyLimit              int64
	MonthlyUsed, MonthlyLimit          int64
	MonthlyCostMicros, MonthlyUnpriced int64
	// UpdatedAt is when the project or key row was last edited, so when its
	// budgets last may have changed. Requests do not touch it.
	UpdatedAt time.Time
}

// Ratio is the larger of the daily and the monthly share of the budget used,
// counting only the budgets that are set; ok is false when neither is, because
// the ratio is then undefined, not zero. It is the one definition behind both
// ragmux_budget_used_ratio and the limit alerts.
func (u BudgetUsage) Ratio() (ratio float64, ok bool) {
	if u.DailyLimit > 0 {
		ratio, ok = float64(u.DailyUsed)/float64(u.DailyLimit), true
	}
	if u.MonthlyLimit > 0 {
		if r := float64(u.MonthlyUsed) / float64(u.MonthlyLimit); !ok || r > ratio {
			ratio, ok = r, true
		}
	}
	return ratio, ok
}

const (
	budgetProjectSelect = `SELECT p.id, 0::bigint,
			COALESCE(d.prompt_tokens + d.completion_tokens, 0), p.budget_daily_tokens,
			COALESCE(m.prompt_tokens + m.completion_tokens, 0), p.budget_monthly_tokens,
			COALESCE(m.cost_micros, 0), COALESCE(m.unpriced_requests, 0), p.updated_at
		FROM projects p
		LEFT JOIN project_usage d ON d.project_id = p.id AND d.period = 'day' AND d.period_start = $1
		LEFT JOIN project_usage m ON m.project_id = p.id AND m.period = 'month' AND m.period_start = $2`

	// A key has one budget and any number of project grants; its row is
	// reported once for every granted project in the filter, so each project's
	// alert endpoint hears about the keys it lets in.
	budgetKeySelect = `SELECT g.project_id, k.id,
			COALESCE(d.prompt_tokens + d.completion_tokens, 0), k.budget_daily_tokens,
			COALESCE(m.prompt_tokens + m.completion_tokens, 0), k.budget_monthly_tokens,
			0::bigint, 0::bigint, k.updated_at
		FROM api_key_projects g
		JOIN api_keys k ON k.id = g.api_key_id AND k.kind = 'gateway'
			AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > $3)
		LEFT JOIN key_usage d ON d.api_key_id = k.id AND d.period = 'day' AND d.period_start = $1
		LEFT JOIN key_usage m ON m.api_key_id = k.id AND m.period = 'month' AND m.period_start = $2
		WHERE g.project_id = ANY($4) AND (k.budget_daily_tokens > 0 OR k.budget_monthly_tokens > 0)`
)

func scanBudgetUsages(rows pgx.Rows) ([]BudgetUsage, error) {
	defer rows.Close()
	var out []BudgetUsage
	for rows.Next() {
		var u BudgetUsage
		if err := rows.Scan(&u.ProjectID, &u.KeyID, &u.DailyUsed, &u.DailyLimit, &u.MonthlyUsed, &u.MonthlyLimit,
			&u.MonthlyCostMicros, &u.MonthlyUnpriced, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// BudgetUsedRatios reports, for every project with a daily or monthly token
// budget, the larger of day tokens over the daily budget and month tokens
// over the monthly budget in the windows w. The key is the project id in
// decimal, which is what ragmux_budget_used_ratio{project} carries. A
// project without any budget is left out: its ratio is undefined, not zero.
// One query, whatever the number of projects.
func (s *Store) BudgetUsedRatios(ctx context.Context, w UsageWindows) (map[string]float64, error) {
	rows, err := s.pool.Query(ctx, budgetProjectSelect+`
		WHERE p.budget_daily_tokens > 0 OR p.budget_monthly_tokens > 0`, w.Day, w.Month)
	if err != nil {
		return nil, err
	}
	usages, err := scanBudgetUsages(rows)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, u := range usages {
		if ratio, ok := u.Ratio(); ok {
			out[strconv.FormatInt(u.ProjectID, 10)] = ratio
		}
	}
	return out, nil
}

// BudgetUsages reads the budgets and the usage of the given projects and of
// the gateway keys granted to them, in one query. The project rows come back
// whether or not the project sets a token budget, because the money budget
// needs its spend; a key row only when the key sets one and is neither revoked
// nor expired at w.Minute. An empty list reads nothing.
func (s *Store) BudgetUsages(ctx context.Context, w UsageWindows, projectIDs []int64) ([]BudgetUsage, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, budgetProjectSelect+` WHERE p.id = ANY($4)
		UNION ALL `+budgetKeySelect, w.Day, w.Month, w.Minute, projectIDs)
	if err != nil {
		return nil, err
	}
	return scanBudgetUsages(rows)
}

// The end-user counters (ADR-035). end_user_usage has the shape of
// project_usage with the end user in the key, so the limiter reads a tier the
// same way it reads the other two. Only a request that names an end user, to a
// project that sets an end-user ceiling, reads or writes it.

// ReserveEndUserMinuteRequest atomically increments an end user's minute
// request counter and returns the new value.
func (s *Store) ReserveEndUserMinuteRequest(ctx context.Context, projectID int64, endUser string, minute time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `INSERT INTO end_user_usage (project_id, end_user, period, period_start, requests)
		VALUES ($1, $2, 'minute', $3, 1)
		ON CONFLICT (project_id, end_user, period, period_start) DO UPDATE SET requests = end_user_usage.requests + 1
		RETURNING requests`, projectID, clampEndUser(endUser), minute).Scan(&n)
	return n, err
}

// ReleaseEndUserMinuteRequest undoes a reservation that was rejected.
func (s *Store) ReleaseEndUserMinuteRequest(ctx context.Context, projectID int64, endUser string, minute time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE end_user_usage SET requests = GREATEST(requests - 1, 0)
		WHERE project_id = $1 AND end_user = $2 AND period = 'minute' AND period_start = $3`,
		projectID, clampEndUser(endUser), minute)
	return err
}

// GetEndUserUsage reads an end user's minute, day and month rows in one
// query. Missing rows come back zeroed with the requested period start.
func (s *Store) GetEndUserUsage(ctx context.Context, projectID int64, endUser string, w UsageWindows) (minute, day, month UsageRow, err error) {
	minute = UsageRow{Period: PeriodMinute, PeriodStart: w.Minute}
	day = UsageRow{Period: PeriodDay, PeriodStart: w.Day}
	month = UsageRow{Period: PeriodMonth, PeriodStart: w.Month}
	rows, err := s.pool.Query(ctx, `SELECT period, requests, prompt_tokens, completion_tokens FROM end_user_usage
		WHERE project_id = $1 AND end_user = $2 AND ((period = 'minute' AND period_start = $3)
			OR (period = 'day' AND period_start = $4) OR (period = 'month' AND period_start = $5))`,
		projectID, clampEndUser(endUser), w.Minute, w.Day, w.Month)
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

// RecordEndUserUsage adds a finished request's tokens to an end user's three
// period rows; the minute request counter is only bumped when the slot was not
// already reserved.
func (s *Store) RecordEndUserUsage(ctx context.Context, projectID int64, endUser string, w UsageWindows, promptTokens, completionTokens int64, countMinuteRequest bool) error {
	minuteReq := 0
	if countMinuteRequest {
		minuteReq = 1
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO end_user_usage (project_id, end_user, period, period_start, requests, prompt_tokens, completion_tokens)
		VALUES ($1, $2, 'minute', $3, $6, $7, $8), ($1, $2, 'day', $4, 1, $7, $8), ($1, $2, 'month', $5, 1, $7, $8)
		ON CONFLICT (project_id, end_user, period, period_start) DO UPDATE SET
			requests = end_user_usage.requests + EXCLUDED.requests,
			prompt_tokens = end_user_usage.prompt_tokens + EXCLUDED.prompt_tokens,
			completion_tokens = end_user_usage.completion_tokens + EXCLUDED.completion_tokens`,
		projectID, clampEndUser(endUser), w.Minute, w.Day, w.Month, minuteReq,
		max(promptTokens, 0), max(completionTokens, 0))
	return err
}

// DeleteEndUserUsageBefore removes rows of one period that started before cutoff.
func (s *Store) DeleteEndUserUsageBefore(ctx context.Context, period string, cutoff time.Time) (int64, error) {
	res, err := s.pool.Exec(ctx, "DELETE FROM end_user_usage WHERE period = $1 AND period_start < $2", period, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}
