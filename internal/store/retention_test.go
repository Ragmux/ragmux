package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// DeleteExpiredBatch removes at most limit expired rows per call and never a
// row inside the retention window.
func TestDeleteExpiredBatchHonoursLimit(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	cutoff := now.Add(-24 * time.Hour)
	insert := func(n int, at time.Time) {
		t.Helper()
		if _, err := s.DB().Exec(ctx, `INSERT INTO request_logs (project_id, model_name, status_code, created_at)
			SELECT $1, 'm', 200, $2 FROM generate_series(1, $3)`, p.ID, at, n); err != nil {
			t.Fatal(err)
		}
	}
	insert(25, now.Add(-48*time.Hour))
	insert(7, now)

	for _, want := range []int64{10, 10, 5, 0} {
		n, err := s.DeleteExpiredBatch(ctx, store.RetainRequestLogs, cutoff, 10)
		if err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("batch removed %d rows, want %d", n, want)
		}
	}
	var left int
	if err := s.DB().QueryRow(ctx, "SELECT COUNT(*) FROM request_logs").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 7 {
		t.Errorf("request_logs left = %d, want the 7 inside the window", left)
	}

	// There is no unbounded spelling: a non-positive limit is refused and
	// deletes nothing.
	insert(30, now.Add(-48*time.Hour))
	for _, limit := range []int{0, -1} {
		if n, err := s.DeleteExpiredBatch(ctx, store.RetainRequestLogs, cutoff, limit); !errors.Is(err, store.ErrBatchLimit) || n != 0 {
			t.Errorf("limit %d = (%d, %v), want (0, ErrBatchLimit)", limit, n, err)
		}
	}
	// The one-shot helper loops over bounded batches until nothing is left.
	n, err := s.DeleteRequestLogsBefore(ctx, cutoff)
	if err != nil || n != 30 {
		t.Errorf("DeleteRequestLogsBefore = (%d, %v), want (30, nil)", n, err)
	}
}

func TestDeleteExpiredBatchRejectsUnknownTable(t *testing.T) {
	s := testdb.Open(t)
	if _, err := s.DeleteExpiredBatch(context.Background(), store.RetentionTable(99), time.Now(), 10); err == nil {
		t.Error("an unknown retention table must be refused")
	}
	if got := store.RetentionTable(99).String(); got != "RetentionTable(99)" {
		t.Errorf("String() = %q", got)
	}
	if got := store.RetainSessions.String(); got != "sessions" {
		t.Errorf("String() = %q, want sessions", got)
	}
}
