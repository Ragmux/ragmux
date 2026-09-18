package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func TestUsernamesAreCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	u, err := s.CreateUser(ctx, "Alice", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUserByUsername(ctx, "ALICE")
	if err != nil || got.ID != u.ID || got.Username != "Alice" {
		t.Fatalf("lookup by a different casing: %v %+v (stored casing must be preserved)", err, got)
	}
	if _, err := s.CreateUser(ctx, "alice", "h", "viewer"); !store.IsUniqueViolation(err) {
		t.Fatalf("case-insensitive duplicate should hit the unique index: %v", err)
	}
	if _, err := s.CreateUserWithProjects(ctx, "aLiCe", "h", "viewer", nil); !store.IsUniqueViolation(err) {
		t.Fatalf("case-insensitive duplicate through CreateUserWithProjects: %v", err)
	}

	// A database that already holds colliding names refuses migration 0008
	// with the names in the error, so the operator knows what to rename.
	for _, q := range []string{
		"DROP INDEX idx_users_username_lower",
		"DELETE FROM schema_migrations WHERE version = 8",
		"INSERT INTO users (username, password_hash, role) VALUES ('alice', 'h', 'viewer'), ('Bob', 'h', 'viewer'), ('bob', 'h', 'viewer')",
	} {
		if _, err := s.DB().Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_, err = store.Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "differ only by case") || !strings.Contains(err.Error(), "alice, bob") {
		t.Fatalf("migration over colliding usernames should fail with the names: %v", err)
	}
}

func TestCreateUserWithProjectsAndStats(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "ollama", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p1, _, err := s.CreateProject(ctx, &store.Project{Name: "p1", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := s.CreateProject(ctx, &store.Project{Name: "p2", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUserWithProjects(ctx, "u", "h", "viewer", []int64{p1.ID, 424242}); !store.IsForeignKeyViolation(err) {
		t.Fatalf("unknown project should fail with a foreign key violation: %v", err)
	}
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("failed create left a user behind: %d", n)
	}
	u, err := s.CreateUserWithProjects(ctx, "u", "h", "viewer", []int64{p1.ID, p2.ID, p1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsProjectMember(ctx, p2.ID, u.ID); !ok {
		t.Error("membership not written")
	}
	other, err := s.CreateUser(ctx, "v", "h", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	for tok, ttl := range map[string]time.Duration{"live1": time.Hour, "live2": time.Hour, "dead": -time.Minute} {
		if err := s.CreateSession(ctx, u.ID, tok, ttl); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListUsersWithStats(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list with stats: %v %+v", err, list)
	}
	if list[0].ID != u.ID || list[0].ProjectCount != 2 || list[0].ActiveSessions != 2 {
		t.Errorf("stats for u: %+v", list[0])
	}
	if list[1].ID != other.ID || list[1].ProjectCount != 0 || list[1].ActiveSessions != 0 {
		t.Errorf("stats for v: %+v", list[1])
	}
	got, expires, err := s.UserAndExpiryBySession(ctx, "live1")
	if err != nil || got.ID != u.ID || expires.Before(time.Now().Add(50*time.Minute)) || expires.Location() != time.UTC {
		t.Errorf("session expiry: %v %+v %v", err, got, expires)
	}
}

func TestAuditFiltersCountAndStream(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u, err := s.CreateUser(ctx, "u", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	for i, action := range []string{"login.success", "project.create", "project.update", "user.create", "login.failure"} {
		l := &store.AuditLog{Action: action, TargetType: "x", IP: "127.0.0.1"}
		if i%2 == 0 {
			l.ActorUserID = &u.ID
			l.ActorUsername = "u"
		}
		if err := s.InsertAuditLog(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	all, more, err := s.ListAuditLogs(ctx, store.AuditFilter{})
	if err != nil || len(all) != 5 || more {
		t.Fatalf("list all: %v %d more=%v", err, len(all), more)
	}
	if all[0].Action != "login.failure" {
		t.Errorf("newest first: %v", all[0])
	}
	page, more, err := s.ListAuditLogs(ctx, store.AuditFilter{Limit: 2})
	if err != nil || len(page) != 2 || !more {
		t.Fatalf("limited page: %v %d more=%v", err, len(page), more)
	}
	if n, _ := s.CountAuditLogs(ctx, store.AuditFilter{Limit: 2}); n != 5 {
		t.Errorf("count ignores the limit: %d", n)
	}
	byActor := store.AuditFilter{ActorUserID: &u.ID, Action: "project."}
	if n, _ := s.CountAuditLogs(ctx, byActor); n != 1 {
		t.Errorf("count by actor and prefix: %d", n)
	}
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	if n, _ := s.CountAuditLogs(ctx, store.AuditFilter{Since: &past, Until: &future}); n != 5 {
		t.Errorf("since/until around now: %d", n)
	}
	if n, _ := s.CountAuditLogs(ctx, store.AuditFilter{Since: &future}); n != 0 {
		t.Errorf("since in the future: %d", n)
	}
	if n, _ := s.CountAuditLogs(ctx, store.AuditFilter{Until: &past}); n != 0 {
		t.Errorf("until in the past: %d", n)
	}
	// The before cursor pages the list but never the count.
	cursor, _ := time.Parse(time.RFC3339, all[1].CreatedAt)
	if n, _ := s.CountAuditLogs(ctx, store.AuditFilter{Before: &cursor}); n != 5 {
		t.Errorf("count with cursor: %d", n)
	}
	seen := []string{}
	err = s.StreamAuditLogs(ctx, store.AuditFilter{Action: "login."}, func(l *store.AuditLog) error {
		seen = append(seen, l.Action)
		return nil
	})
	if err != nil || strings.Join(seen, ",") != "login.failure,login.success" {
		t.Errorf("stream: %v %v", err, seen)
	}
	stop := errors.New("stop")
	if err := s.StreamAuditLogs(ctx, store.AuditFilter{}, func(*store.AuditLog) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("callback error should be returned: %v", err)
	}
}

func TestFailedLoginAggregates(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	for i := 0; i < 3; i++ {
		if err := s.RecordLoginAttempt(ctx, "a", "10.0.0.1", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordLoginAttempt(ctx, "b", "10.0.0.1", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordLoginAttempt(ctx, "a", "10.0.0.1", true); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountFailedLoginAttemptsSince(ctx, time.Now().Add(-time.Minute)); err != nil || n != 4 {
		t.Errorf("failures since: %v %d", err, n)
	}
	if n, _ := s.CountFailedLoginAttemptsSince(ctx, time.Now().Add(time.Minute)); n != 0 {
		t.Errorf("failures since the future: %d", n)
	}
	pairs, err := s.FailedLoginPairsSince(ctx, time.Now().Add(-time.Minute), 2)
	if err != nil || len(pairs) != 1 || pairs[0].Username != "a" || pairs[0].IP != "10.0.0.1" || pairs[0].Failures != 3 || pairs[0].Oldest.IsZero() {
		t.Errorf("pairs: %v %+v", err, pairs)
	}
	if pairs, _ := s.FailedLoginPairsSince(ctx, time.Now().Add(-time.Minute), 1); len(pairs) != 2 {
		t.Errorf("pairs with min 1: %+v", pairs)
	}
}
