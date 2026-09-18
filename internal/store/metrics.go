package store

import (
	"context"
	"math"
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
	Error            string `json:"error"`
	CreatedAt        string `json:"created_at"`
}

// InsertRequestLog persists a metric row.
func (s *Store) InsertRequestLog(ctx context.Context, l *RequestLog) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO request_logs
		(project_id, model_name, status_code, prompt_tokens, completion_tokens, estimated, latency_ms, streamed, rag_used, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		l.ProjectID, l.ModelName, l.StatusCode, l.PromptTokens, l.CompletionTokens, l.Estimated,
		l.LatencyMs, l.Streamed, l.RAGUsed, l.Error)
	return err
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
}

// Summarize computes totals for a project (or all projects when projectID is
// nil) since the given time.
func (s *Store) Summarize(ctx context.Context, projectID *int64, since time.Time) (*MetricsSummary, error) {
	q := `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		COALESCE(AVG(latency_ms), 0)::float8,
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::float8,
		COALESCE(SUM(CASE WHEN rag_used THEN 1 ELSE 0 END), 0)
		FROM request_logs WHERE created_at >= $1`
	args := []any{since.UTC()}
	if projectID != nil {
		q += " AND project_id = $2"
		args = append(args, *projectID)
	}
	m := &MetricsSummary{ProjectID: projectID}
	var p95 float64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&m.Requests, &m.Errors, &m.PromptTokens,
		&m.CompletionTokens, &m.AvgLatencyMs, &p95, &m.RAGRequests); err != nil {
		return nil, err
	}
	m.P95LatencyMs = int64(math.Round(p95))
	return m, nil
}

// RecentRequests returns the newest request logs for a project.
func (s *Store) RecentRequests(ctx context.Context, projectID *int64, limit int) ([]*RequestLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT id, project_id, model_name, status_code, prompt_tokens, completion_tokens, estimated,
		latency_ms, streamed, rag_used, error, created_at FROM request_logs`
	args := []any{}
	if projectID != nil {
		q += " WHERE project_id = $1"
		args = append(args, *projectID)
	}
	if projectID != nil {
		q += " ORDER BY id DESC LIMIT $2"
	} else {
		q += " ORDER BY id DESC LIMIT $1"
	}
	args = append(args, limit)
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
			&l.Estimated, &l.LatencyMs, &l.Streamed, &l.RAGUsed, &l.Error, &created); err != nil {
			return nil, err
		}
		l.CreatedAt = ts(created)
		out = append(out, l)
	}
	return out, rows.Err()
}

// DailyBucket is request volume for one UTC day.
type DailyBucket struct {
	Day              string `json:"day"`
	Requests         int    `json:"requests"`
	Errors           int    `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

// DailySeries returns per-day totals for the last n days.
func (s *Store) DailySeries(ctx context.Context, projectID *int64, days int) ([]DailyBucket, error) {
	if days <= 0 {
		days = 14
	}
	since := time.Now().UTC().AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	q := `SELECT date_trunc('day', created_at AT TIME ZONE 'UTC') AS day, COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		FROM request_logs WHERE created_at >= $1`
	args := []any{since}
	if projectID != nil {
		q += " AND project_id = $2"
		args = append(args, *projectID)
	}
	q += " GROUP BY day ORDER BY day"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DailyBucket{}
	for rows.Next() {
		var b DailyBucket
		var day time.Time
		if err := rows.Scan(&day, &b.Requests, &b.Errors, &b.PromptTokens, &b.CompletionTokens); err != nil {
			return nil, err
		}
		b.Day = day.Format("2006-01-02")
		out = append(out, b)
	}
	return out, rows.Err()
}
