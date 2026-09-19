package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// RequestLog is one proxied chat completion.
type RequestLog struct {
	ID               int64  `json:"id"`
	ProjectID        int64  `json:"project_id"`
	ModelName        string `json:"model_name"`
	StatusCode       int    `json:"status_code"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Estimated        bool   `json:"estimated"`
	LatencyMs        int64  `json:"latency_ms"`
	Streamed         bool   `json:"streamed"`
	RAGUsed          bool   `json:"rag_used"`
	// RAGHits is the number of retrieved chunks injected into the prompt.
	RAGHits int `json:"rag_hits"`
	// CachedPromptTokens and CacheWriteTokens are the parts of the prompt a
	// provider cache served and wrote; both are included in PromptTokens.
	CachedPromptTokens int `json:"cached_prompt_tokens"`
	CacheWriteTokens   int `json:"cache_write_tokens"`
	// CostMicros is the estimated cost in USD millionths (exact in BIGINT,
	// which is why it is summed rather than a float); CostUSD is the same
	// figure in dollars, computed in Go. CostSource is "builtin", "user" or
	// "none" when no price matched the model.
	CostMicros int64   `json:"cost_micros"`
	CostUSD    float64 `json:"cost_usd"`
	CostSource string  `json:"cost_source"`
	Error      string  `json:"error"`
	// APIKeyID and UserID attribute the request to the user-owned key that
	// made it; both are nil for a project's default key. UserID is
	// denormalised so deleting the key (ON DELETE SET NULL) never loses who
	// spent the tokens.
	APIKeyID  *int64 `json:"api_key_id"`
	UserID    *int64 `json:"user_id"`
	CreatedAt string `json:"created_at"`
}

// CostSourceNone marks a request whose model matched no price row.
const CostSourceNone = "none"

// usd converts a micro-dollar total to dollars for the JSON API.
func usd(micros int64) float64 { return float64(micros) / 1e6 }

// InsertRequestLog persists a metric row.
func (s *Store) InsertRequestLog(ctx context.Context, l *RequestLog) error {
	source := l.CostSource
	if source == "" {
		source = CostSourceNone
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO request_logs
		(project_id, model_name, status_code, prompt_tokens, completion_tokens, estimated, latency_ms, streamed,
		 rag_used, rag_hits, error, cached_prompt_tokens, cache_write_tokens, cost_micros, cost_source,
		 api_key_id, user_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		l.ProjectID, l.ModelName, l.StatusCode, l.PromptTokens, l.CompletionTokens, l.Estimated,
		l.LatencyMs, l.Streamed, l.RAGUsed, l.RAGHits, l.Error,
		l.CachedPromptTokens, l.CacheWriteTokens, l.CostMicros, source, l.APIKeyID, l.UserID)
	return err
}

// DeleteRequestLogsBefore removes request logs older than t and reports how
// many rows were removed.
func (s *Store) DeleteRequestLogsBefore(ctx context.Context, t time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM request_logs WHERE created_at < $1", t.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MetricsSummary aggregates request logs over a window.
type MetricsSummary struct {
	ProjectID        *int64  `json:"project_id,omitempty"`
	Requests         int     `json:"requests"`
	Errors           int     `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	P95LatencyMs     int64   `json:"p95_latency_ms"`
	RAGRequests      int     `json:"rag_requests"`
	// RateLimited counts requests answered 429 by the gateway's limiter.
	RateLimited int `json:"rate_limited"`
	// CostMicros/CostUSD are the estimated spend of the window; cost is
	// informational and never enforced.
	CostMicros int64   `json:"cost_micros"`
	CostUSD    float64 `json:"cost_usd"`
}

// MetricsFilter narrows metric queries. A nil ProjectID means every project;
// a non-nil UserID restricts to projects the user is a member of.
type MetricsFilter struct {
	ProjectID *int64
	UserID    *int64
}

// where renders the filter as SQL predicates; args are appended after base.
func (f MetricsFilter) where(base []any) (string, []any) {
	return f.whereCol("project_id", base)
}

// whereCol is where with the request_logs project column spelled as col, for
// queries that join request_logs under an alias.
func (f MetricsFilter) whereCol(col string, base []any) (string, []any) {
	var sb strings.Builder
	if f.ProjectID != nil {
		base = append(base, *f.ProjectID)
		fmt.Fprintf(&sb, " AND %s = $%d", col, len(base))
	}
	if f.UserID != nil {
		base = append(base, *f.UserID)
		fmt.Fprintf(&sb, " AND %s IN (SELECT project_id FROM project_members WHERE user_id = $%d)", col, len(base))
	}
	return sb.String(), base
}

// Token and cost columns are summed through GREATEST(…, 0) wherever a total
// is reported. The gateway clamps the counts it writes, but a row put there
// by an older build, a restored dump or anything other than the gateway can
// still be negative, and a single such row must not subtract from a figure
// the dashboard presents as usage or spend.

// Summarize computes totals matching the filter since the given time.
func (s *Store) Summarize(ctx context.Context, f MetricsFilter, since time.Time) (*MetricsSummary, error) {
	return s.SummarizeBetween(ctx, f, since, time.Time{})
}

// SummarizeBetween is Summarize bounded above: rows at or after until are
// excluded, which is how the previous comparison window is computed. A zero
// until means no upper bound.
func (s *Store) SummarizeBetween(ctx context.Context, f MetricsFilter, since, until time.Time) (*MetricsSummary, error) {
	q := `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(GREATEST(prompt_tokens, 0)), 0), COALESCE(SUM(GREATEST(completion_tokens, 0)), 0),
		COALESCE(AVG(latency_ms), 0)::float8,
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::float8,
		COALESCE(SUM(CASE WHEN rag_used THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code = 429 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(GREATEST(cost_micros, 0)), 0)
		FROM request_logs WHERE created_at >= $1`
	base := []any{since.UTC()}
	if !until.IsZero() {
		base = append(base, until.UTC())
		q += " AND created_at < $2"
	}
	cond, args := f.where(base)
	q += cond
	m := &MetricsSummary{ProjectID: f.ProjectID}
	var p95 float64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&m.Requests, &m.Errors, &m.PromptTokens,
		&m.CompletionTokens, &m.AvgLatencyMs, &p95, &m.RAGRequests, &m.RateLimited, &m.CostMicros); err != nil {
		return nil, err
	}
	m.P95LatencyMs = int64(math.Round(p95))
	m.CostUSD = usd(m.CostMicros)
	return m, nil
}

// RecentRequests returns the newest request logs matching the filter.
func (s *Store) RecentRequests(ctx context.Context, f MetricsFilter, limit int) ([]*RequestLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT id, project_id, model_name, status_code, prompt_tokens, completion_tokens, estimated,
		latency_ms, streamed, rag_used, rag_hits, error, cached_prompt_tokens, cache_write_tokens,
		cost_micros, cost_source, api_key_id, user_id, created_at FROM request_logs WHERE true`
	cond, args := f.where(nil)
	args = append(args, limit)
	q += cond + fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RequestLog{}
	for rows.Next() {
		l := &RequestLog{}
		var created time.Time
		if err := rows.Scan(&l.ID, &l.ProjectID, &l.ModelName, &l.StatusCode, &l.PromptTokens, &l.CompletionTokens,
			&l.Estimated, &l.LatencyMs, &l.Streamed, &l.RAGUsed, &l.RAGHits, &l.Error,
			&l.CachedPromptTokens, &l.CacheWriteTokens, &l.CostMicros, &l.CostSource,
			&l.APIKeyID, &l.UserID, &created); err != nil {
			return nil, err
		}
		l.CreatedAt = ts(created)
		l.CostUSD = usd(l.CostMicros)
		out = append(out, l)
	}
	return out, rows.Err()
}

// DailyBucket is request volume for one UTC day.
type DailyBucket struct {
	Day              string  `json:"day"`
	Requests         int     `json:"requests"`
	Errors           int     `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostMicros       int64   `json:"cost_micros"`
	CostUSD          float64 `json:"cost_usd"`
}

// MaxSeriesDays caps DailySeries; longer ranges belong in the CSV export.
const MaxSeriesDays = 90

// DailySeries returns per-day totals for the last n days (default 14,
// capped at MaxSeriesDays).
func (s *Store) DailySeries(ctx context.Context, f MetricsFilter, days int) ([]DailyBucket, error) {
	if days <= 0 {
		days = 14
	}
	if days > MaxSeriesDays {
		days = MaxSeriesDays
	}
	since := time.Now().UTC().AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	q := `SELECT date_trunc('day', created_at AT TIME ZONE 'UTC') AS day, COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(GREATEST(prompt_tokens, 0)), 0), COALESCE(SUM(GREATEST(completion_tokens, 0)), 0),
		COALESCE(SUM(GREATEST(cost_micros, 0)), 0)
		FROM request_logs WHERE created_at >= $1`
	cond, args := f.where([]any{since})
	q += cond + " GROUP BY day ORDER BY day"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DailyBucket{}
	for rows.Next() {
		var b DailyBucket
		var day time.Time
		if err := rows.Scan(&day, &b.Requests, &b.Errors, &b.PromptTokens, &b.CompletionTokens, &b.CostMicros); err != nil {
			return nil, err
		}
		b.Day = day.Format("2006-01-02")
		b.CostUSD = usd(b.CostMicros)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ProjectMetrics is one project's totals inside a window.
type ProjectMetrics struct {
	ProjectID        int64   `json:"project_id"`
	Name             string  `json:"name"`
	Requests         int     `json:"requests"`
	Errors           int     `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	RateLimited      int     `json:"rate_limited"`
	RAGRequests      int     `json:"rag_requests"`
	CostMicros       int64   `json:"cost_micros"`
	CostUSD          float64 `json:"cost_usd"`
}

// SummarizeByProject breaks the window down per project, busiest first.
// Projects without requests in the window are not listed.
func (s *Store) SummarizeByProject(ctx context.Context, f MetricsFilter, since time.Time) ([]ProjectMetrics, error) {
	q := `SELECT l.project_id, COALESCE(p.name, ''), COUNT(*),
		COALESCE(SUM(CASE WHEN l.status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(GREATEST(l.prompt_tokens, 0)), 0), COALESCE(SUM(GREATEST(l.completion_tokens, 0)), 0),
		COALESCE(SUM(CASE WHEN l.status_code = 429 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN l.rag_used THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(GREATEST(l.cost_micros, 0)), 0)
		FROM request_logs l LEFT JOIN projects p ON p.id = l.project_id WHERE l.created_at >= $1`
	cond, args := f.whereCol("l.project_id", []any{since.UTC()})
	q += cond + " GROUP BY l.project_id, p.name ORDER BY COUNT(*) DESC, l.project_id"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectMetrics{}
	for rows.Next() {
		var m ProjectMetrics
		if err := rows.Scan(&m.ProjectID, &m.Name, &m.Requests, &m.Errors, &m.PromptTokens, &m.CompletionTokens,
			&m.RateLimited, &m.RAGRequests, &m.CostMicros); err != nil {
			return nil, err
		}
		m.CostUSD = usd(m.CostMicros)
		out = append(out, m)
	}
	return out, rows.Err()
}

// RequestExportRow is one request log with its project's name, for CSV export.
type RequestExportRow struct {
	RequestLog
	ProjectName string
}

// MaxExportRows bounds one CSV export.
const MaxExportRows = 50000

// ExportRequests streams request logs since the given time, oldest first,
// to fn until MaxExportRows have been delivered or fn returns an error.
func (s *Store) ExportRequests(ctx context.Context, f MetricsFilter, since time.Time, fn func(*RequestExportRow) error) error {
	q := `SELECT l.id, l.project_id, COALESCE(p.name, ''), l.model_name, l.status_code, l.prompt_tokens, l.completion_tokens,
		l.estimated, l.latency_ms, l.streamed, l.rag_used, l.rag_hits, l.error,
		l.cached_prompt_tokens, l.cache_write_tokens, l.cost_micros, l.cost_source,
		l.api_key_id, l.user_id, l.created_at
		FROM request_logs l LEFT JOIN projects p ON p.id = l.project_id WHERE l.created_at >= $1`
	cond, args := f.whereCol("l.project_id", []any{since.UTC()})
	args = append(args, MaxExportRows)
	q += cond + fmt.Sprintf(" ORDER BY l.id LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r RequestExportRow
		var created time.Time
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ProjectName, &r.ModelName, &r.StatusCode, &r.PromptTokens, &r.CompletionTokens,
			&r.Estimated, &r.LatencyMs, &r.Streamed, &r.RAGUsed, &r.RAGHits, &r.Error,
			&r.CachedPromptTokens, &r.CacheWriteTokens, &r.CostMicros, &r.CostSource,
			&r.APIKeyID, &r.UserID, &created); err != nil {
			return err
		}
		r.CreatedAt = ts(created)
		r.CostUSD = usd(r.CostMicros)
		if err := fn(&r); err != nil {
			return err
		}
	}
	return rows.Err()
}
