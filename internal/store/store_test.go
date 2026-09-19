package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// cosineDistance is the reference metric (1 - cosine similarity) that the
// pgvector <=> operator is expected to reproduce.
func cosineDistance(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

func TestOpenReportsDatabaseInfo(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	if s.ServerVersion == "" {
		t.Error("server version not captured")
	}
	if s.SecretKeySource != "env" {
		t.Errorf("secret key source = %q, want env", s.SecretKeySource)
	}
	info, err := s.DatabaseInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.PgvectorVersion == "" || info.MigrationsVersion < 1 || info.SizeBytes <= 0 {
		t.Errorf("unexpected database info: %+v", info)
	}
	if err := s.DB().Ping(ctx); err != nil {
		t.Errorf("pool ping: %v", err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	cfg := testdb.Config(t)
	s1 := testdb.OpenWith(t, cfg)
	s1.Close()
	// Migrations are recorded, so a second open on the same schema is a no-op.
	s2 := testdb.OpenWith(t, cfg)
	info, err := s2.DatabaseInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.MigrationsVersion != 13 {
		t.Errorf("migrations version = %d, want 13", info.MigrationsVersion)
	}
}

func TestSecretKeyFileFallback(t *testing.T) {
	cfg := testdb.Config(t)
	cfg.SecretKeyHex = ""
	s := testdb.OpenWith(t, cfg)
	if s.SecretKeySource != "file" {
		t.Errorf("secret key source = %q, want file", s.SecretKeySource)
	}
	ctx := context.Background()
	c, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "o", ProviderType: "openai", APIKey: "sk-file-key-1234", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// The same DataDir yields the same key file, so the credential still decrypts.
	s2 := testdb.OpenWith(t, cfg)
	got, err := s2.GetConnection(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-file-key-1234" {
		t.Errorf("api key after reopen = %q", got.APIKey)
	}
}

func TestCredentialsRoundTripAcrossReopen(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	c, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "o", ProviderType: "openai", APIKey: "sk-secret-123456", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKeyMasked != "sk-...3456" {
		t.Errorf("mask = %q", c.APIKeyMasked)
	}
	if _, err := time.Parse(time.RFC3339, c.CreatedAt); err != nil {
		t.Errorf("created_at %q is not RFC3339: %v", c.CreatedAt, err)
	}
	s.Close()

	s2 := testdb.OpenWith(t, cfg)
	got, err := s2.GetConnection(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "sk-secret-123456" {
		t.Errorf("api key after reopen = %q", got.APIKey)
	}

	// A different SECRET_KEY no longer gets as far as reading a
	// credential: the canary stops the store from opening at all.
	other := cfg
	other.SecretKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	_, err = store.Open(ctx, other, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, store.ErrSecretKeyMismatch) {
		t.Errorf("opening with a different key should fail the canary, got %v", err)
	}
}

// TestRotateKeyReSealsCanary is the hard dependency between rotate-key and
// the boot check: a rotation that left the canary sealed with the old key
// would make every later start fail.
func TestRotateKeyReSealsCanary(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	if _, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "o", ProviderType: "openai", APIKey: "sk-secret-123456", ModelName: "m"}); err != nil {
		t.Fatal(err)
	}
	newKey := "aabbccddeeff00112233445566778899aabbccddeeff001122334455667788990"[:64]
	if _, err := s.ReencryptConnections(ctx, newKey); err != nil {
		t.Fatal(err)
	}
	s.Close()

	old := cfg
	cfg.SecretKeyHex = newKey
	s2 := testdb.OpenWith(t, cfg) // fatals here if the canary was not re-sealed
	if list, err := s2.ListConnections(ctx); err != nil || len(list) != 1 {
		t.Fatalf("connections after rotation: %v %d", err, len(list))
	}
	// And the key the database moved away from is refused.
	if _, err := store.Open(ctx, old, slog.New(slog.NewTextHandler(io.Discard, nil))); !errors.Is(err, store.ErrSecretKeyMismatch) {
		t.Errorf("the pre-rotation key should be refused, got %v", err)
	}
}

func TestVectorSearchOrderingAndDistances(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb", ProviderType: "openai", ModelName: "text-embedding-3-small"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "docs", EmbeddingConnectionID: conn.ID, ChunkSize: 100, ChunkOverlap: 10, TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: "a.txt"}, []byte("x y z"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.DocumentContent(ctx, doc.ID); string(got) != "x y z" {
		t.Errorf("document content = %q", got)
	}
	// Searching before any chunks exist is an empty result, not an error.
	if hits, err := s.SearchTopK(ctx, r.ID, []float32{1, 0, 0, 0}, 2); err != nil || len(hits) != 0 {
		t.Fatalf("search on empty store: %v %+v", err, hits)
	}

	chunks := []*store.Chunk{
		{Index: 0, Content: "x axis", Embedding: []float32{1, 0, 0, 0}},
		{Index: 1, Content: "y axis", Embedding: []float32{0, 1, 0, 0}},
		{Index: 2, Content: "near x", Embedding: []float32{0.9, 0.1, 0, 0}},
		{Index: 3, Content: "z axis", Embedding: []float32{0, 0, 1, 0}},
	}
	if err := s.ReplaceDocumentChunks(ctx, doc, chunks, ""); err != nil {
		t.Fatalf("replace chunks: %v", err)
	}
	rr, err := s.GetRAGStore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Dimensions != 4 || rr.ChunkCount != 4 || rr.DocumentCount != 1 {
		t.Errorf("store after ingest: %+v", rr)
	}
	d, _ := s.GetDocument(ctx, doc.ID)
	if d.Status != store.DocReady || d.ChunkCount != 4 {
		t.Errorf("document after ingest: %+v", d)
	}

	q := []float32{1, 0, 0, 0}
	hits, err := s.SearchTopK(ctx, r.ID, q, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Content != "x axis" || hits[1].Content != "near x" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	for i, h := range hits {
		want := cosineDistance(q, chunks[h.Index].Embedding)
		if math.Abs(h.Distance-want) > 1e-5 {
			t.Errorf("hit %d (%s): distance %f, want %f", i, h.Content, h.Distance, want)
		}
		if h.Filename != "a.txt" || h.DocumentID != doc.ID {
			t.Errorf("hit %d carries wrong document info: %+v", i, h)
		}
	}

	// Re-ingesting replaces rather than duplicates.
	if err := s.ReplaceDocumentChunks(ctx, doc, chunks[:2], ""); err != nil {
		t.Fatal(err)
	}
	if rr, _ = s.GetRAGStore(ctx, r.ID); rr.ChunkCount != 2 {
		t.Errorf("chunk count after re-ingest = %d, want 2", rr.ChunkCount)
	}
	if _, err := s.SearchTopK(ctx, r.ID, []float32{1, 0}, 1); err == nil {
		t.Error("expected dimension mismatch error")
	}

	// Deleting the document cascades to chunks and embeddings.
	if err := s.DeleteDocument(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	if rr, _ = s.GetRAGStore(ctx, r.ID); rr.ChunkCount != 0 || rr.DocumentCount != 0 {
		t.Errorf("counts after delete: %+v", rr)
	}
	if _, err := s.DocumentContent(ctx, doc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("content after delete: %v", err)
	}
	if err := s.DeleteRAGStore(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRAGStore(ctx, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("store after delete: %v", err)
	}
}

func TestProjectKeyLookup(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, key, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != len("sk-proj-")+43 || key[:8] != "sk-proj-" {
		t.Errorf("bad key %q", key)
	}
	if p.APIKeyPrefix != key[:15] {
		t.Errorf("prefix %q vs key %q", p.APIKeyPrefix, key)
	}
	got, err := s.GetProjectByKey(ctx, key)
	if err != nil || got.ID != p.ID {
		t.Fatalf("lookup by key: %v %+v", err, got)
	}
	if _, err := s.GetProjectByKey(ctx, "sk-proj-wrong"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
	nk, err := s.RotateProjectKey(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProjectByKey(ctx, key); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("old key should be revoked, got %v", err)
	}
	if _, err := s.GetProjectByKey(ctx, nk); err != nil {
		t.Errorf("new key lookup: %v", err)
	}
	// The connection is referenced by the project and cannot be deleted.
	if err := s.DeleteConnection(ctx, conn.ID); err == nil {
		t.Error("expected foreign key violation deleting a connection in use")
	}
}

func TestMetrics(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range []store.RequestLog{
		{StatusCode: 200, PromptTokens: 10, CompletionTokens: 5, LatencyMs: 100, RAGUsed: true, RAGHits: 3, Streamed: true},
		{StatusCode: 200, PromptTokens: 20, CompletionTokens: 5, LatencyMs: 200, Estimated: true},
		{StatusCode: 502, LatencyMs: 300, Error: "upstream"},
	} {
		l.ProjectID = p.ID
		if err := s.InsertRequestLog(ctx, &l); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	m, err := s.Summarize(ctx, store.MetricsFilter{ProjectID: &p.ID}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if m.Requests != 3 || m.Errors != 1 || m.PromptTokens != 30 || m.CompletionTokens != 10 || m.RAGRequests != 1 {
		t.Errorf("summary: %+v", m)
	}
	if m.AvgLatencyMs != 200 || m.P95LatencyMs != 290 {
		t.Errorf("latency: avg %v p95 %v", m.AvgLatencyMs, m.P95LatencyMs)
	}
	if m, _ = s.Summarize(ctx, store.MetricsFilter{}, time.Now().Add(time.Hour)); m.Requests != 0 || m.P95LatencyMs != 0 {
		t.Errorf("empty window summary: %+v", m)
	}
	recent, err := s.RecentRequests(ctx, store.MetricsFilter{ProjectID: &p.ID}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0].StatusCode != 502 || !recent[1].Estimated || recent[1].Streamed {
		t.Errorf("recent: %+v %+v", recent[0], recent[1])
	}
	all, _ := s.RecentRequests(ctx, store.MetricsFilter{}, 0)
	if len(all) != 3 || !all[2].RAGUsed || !all[2].Streamed || all[2].RAGHits != 3 || all[1].RAGHits != 0 {
		t.Errorf("all recent: %d rows %+v", len(all), all)
	}
	daily, err := s.DailySeries(ctx, store.MetricsFilter{ProjectID: &p.ID}, 7)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if len(daily) != 1 || daily[0].Day != today || daily[0].Requests != 3 || daily[0].Errors != 1 || daily[0].PromptTokens != 30 {
		t.Errorf("daily: %+v", daily)
	}
}

func TestMetricsCompareByProjectAndSeriesClamp(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	pa, _, err := s.CreateProject(ctx, &store.Project{Name: "alpha", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	pb, _, err := s.CreateProject(ctx, &store.Project{Name: "beta", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.CreateUser(ctx, "viewer", "x", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectMembers(ctx, pb.ID, []int64{member.ID}); err != nil {
		t.Fatal(err)
	}
	insert := func(l store.RequestLog) {
		t.Helper()
		if err := s.InsertRequestLog(ctx, &l); err != nil {
			t.Fatal(err)
		}
	}
	// Current window: two on alpha (one 429, one with RAG), one on beta.
	insert(store.RequestLog{ProjectID: pa.ID, StatusCode: 200, PromptTokens: 10, CompletionTokens: 2, RAGUsed: true, RAGHits: 2})
	insert(store.RequestLog{ProjectID: pa.ID, StatusCode: 429, Error: "rate_limit_rpm"})
	insert(store.RequestLog{ProjectID: pb.ID, StatusCode: 200, PromptTokens: 7, CompletionTokens: 1})
	// Previous window: one row on beta, 90 minutes back.
	insert(store.RequestLog{ProjectID: pb.ID, StatusCode: 502, Error: "upstream"})
	all, _ := s.RecentRequests(ctx, store.MetricsFilter{}, 0)
	if err := s.BackdateRequestLog(ctx, all[0].ID, time.Now().Add(-90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cur, err := s.SummarizeBetween(ctx, store.MetricsFilter{}, now.Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	prev, err := s.SummarizeBetween(ctx, store.MetricsFilter{}, now.Add(-2*time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if cur.Requests != 3 || cur.Errors != 1 || cur.RateLimited != 1 || prev.Requests != 1 || prev.Errors != 1 || prev.RateLimited != 0 {
		t.Errorf("current %+v previous %+v", cur, prev)
	}

	by, err := s.SummarizeByProject(ctx, store.MetricsFilter{}, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(by) != 2 || by[0].ProjectID != pa.ID || by[0].Name != "alpha" || by[0].Requests != 2 || by[0].Errors != 1 ||
		by[0].RateLimited != 1 || by[0].RAGRequests != 1 || by[0].PromptTokens != 10 || by[0].CompletionTokens != 2 ||
		by[1].Name != "beta" || by[1].Requests != 1 {
		t.Errorf("by project: %+v", by)
	}
	// A member of beta only sees beta; project filter selects one row.
	by, err = s.SummarizeByProject(ctx, store.MetricsFilter{UserID: &member.ID}, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(by) != 1 || by[0].ProjectID != pb.ID || by[0].Requests != 1 {
		t.Errorf("member by project: %+v", by)
	}
	if by, _ = s.SummarizeByProject(ctx, store.MetricsFilter{ProjectID: &pa.ID}, now.Add(-time.Hour)); len(by) != 1 || by[0].Name != "alpha" {
		t.Errorf("project filter: %+v", by)
	}
	if by, _ = s.SummarizeByProject(ctx, store.MetricsFilter{}, now.Add(time.Hour)); len(by) != 0 {
		t.Errorf("empty window lists projects: %+v", by)
	}

	// The series is capped at MaxSeriesDays; a shorter range excludes the
	// backdated row only when it falls before the range start.
	if daily, err := s.DailySeries(ctx, store.MetricsFilter{}, 10000); err != nil || len(daily) == 0 {
		t.Errorf("clamped series: %v %+v", err, daily)
	}
	if daily, _ := s.DailySeries(ctx, store.MetricsFilter{ProjectID: &pa.ID}, 1); len(daily) != 1 || daily[0].Requests != 2 {
		t.Errorf("one-day series: %+v", daily)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u, err := s.CreateUser(ctx, "admin", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Errorf("count users = %d", n)
	}
	if err := s.CreateSession(ctx, u.ID, "tok", time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserBySession(ctx, "tok"); err != nil || got.ID != u.ID {
		t.Fatalf("user by session: %v %+v", err, got)
	}
	if err := s.CreateSession(ctx, u.ID, "old", -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserBySession(ctx, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired session should not resolve: %v", err)
	}
	if _, err := s.PurgeExpiredSessions(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, "tok"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserBySession(ctx, "tok"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted session should not resolve: %v", err)
	}
	if err := s.UpdateUserPassword(ctx, u.ID, "hash2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUserByUsername(ctx, "admin"); got.PasswordHash != "hash2" {
		t.Errorf("password hash not updated: %+v", got)
	}
}

func TestBackupInfoCountsVectorTablesAndDocumentBytes(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	empty, err := s.BackupInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Tables != 0 || empty.DocumentsBytes != 0 || empty.LastMigrationAt == nil {
		t.Errorf("backup info on fresh schema: %+v", empty)
	}

	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb", ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "docs", EmbeddingConnectionID: conn.ID, ChunkSize: 100, ChunkOverlap: 10, TopK: 3})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: "a.txt"}, []byte("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceDocumentChunks(ctx, doc, []*store.Chunk{{Index: 0, Content: "hello", Embedding: []float32{1, 0, 0}}}, ""); err != nil {
		t.Fatal(err)
	}
	info, err := s.BackupInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Tables != 1 || info.DocumentsBytes != int64(len("hello world")) {
		t.Errorf("backup info after ingest: %+v", info)
	}
}

func TestLastAdminGuardIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	a, err := s.CreateUser(ctx, "a", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateUser(ctx, "b", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	// Two concurrent demotions: exactly one must succeed.
	results := make(chan error, 2)
	for _, id := range []int64{a.ID, b.ID} {
		go func(id int64) {
			_, err := s.UpdateUser(ctx, id, "editor", true)
			results <- err
		}(id)
	}
	var failed, ok int
	for i := 0; i < 2; i++ {
		switch err := <-results; {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrLastAdmin):
			failed++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || failed != 1 {
		t.Errorf("ok=%d lastadmin=%d, want one of each", ok, failed)
	}
	if n, _ := s.CountActiveAdmins(ctx); n != 1 {
		t.Errorf("active admins = %d", n)
	}
	// Deactivating or deleting the survivor fails too; a non-admin change passes.
	for _, u := range []*store.User{a, b} {
		if _, err := s.UpdateUser(ctx, u.ID, "admin", false); errors.Is(err, store.ErrLastAdmin) {
			if err := s.DeleteUser(ctx, u.ID); !errors.Is(err, store.ErrLastAdmin) {
				t.Errorf("deleting the last admin: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		} else if _, err := s.UpdateUser(ctx, u.ID, "viewer", true); err != nil {
			t.Errorf("changing a non-admin: %v", err)
		}
	}
	if _, err := s.UpdateUser(ctx, 999999, "admin", true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
	if err := s.DeleteUser(ctx, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown user: %v", err)
	}
}

func TestCreateProjectWithMembersIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u, err := s.CreateUser(ctx, "u", "h", "editor")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "ollama", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p := &store.Project{Name: "p", ModelConnectionID: conn.ID}
	if _, _, err := s.CreateProjectWithMembers(ctx, p, []int64{u.ID, 424242}); !store.IsForeignKeyViolation(err) {
		t.Fatalf("unknown member should fail with a foreign key violation: %v", err)
	}
	if list, _ := s.ListProjects(ctx); len(list) != 0 {
		t.Errorf("failed create left a project behind: %v", list)
	}
	out, key, err := s.CreateProjectWithMembers(ctx, p, []int64{u.ID, u.ID})
	if err != nil || key == "" || len(out.MemberIDs) != 1 || out.MemberIDs[0] != u.ID {
		t.Fatalf("create with members: %v %+v", err, out)
	}
}

func TestDeleteUserSessionsExcept(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u, err := s.CreateUser(ctx, "u", "h", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"keep", "drop1", "drop2"} {
		if err := s.CreateSession(ctx, u.ID, tok, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteUserSessionsExcept(ctx, u.ID, store.HashToken("keep")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserBySession(ctx, "keep"); err != nil {
		t.Errorf("current session dropped: %v", err)
	}
	for _, tok := range []string{"drop1", "drop2"} {
		if _, err := s.UserBySession(ctx, tok); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s should be gone: %v", tok, err)
		}
	}
}

func TestReencryptConnections(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	for _, k := range []string{"sk-one", "", "sk-three"} {
		if _, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c" + k, ProviderType: "openai", ModelName: "m", APIKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ReencryptConnections(ctx, "not-hex"); err == nil {
		t.Error("bad key should be rejected")
	}
	newKey := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	n, err := s.ReencryptConnections(ctx, newKey)
	if err != nil || n != 3 {
		t.Fatalf("reencrypt: n=%d err=%v", n, err)
	}
	// The old key can no longer read the rows; the new one can.
	if _, err := s.ListConnections(ctx); err == nil {
		t.Error("old key should fail to decrypt after rotation")
	}
	cfg.SecretKeyHex = newKey
	s2 := testdb.OpenWith(t, cfg)
	list, err := s2.ListConnections(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list with new key: %v %d", err, len(list))
	}
	got := map[string]string{}
	for _, c := range list {
		got[c.Name] = c.APIKey
	}
	if got["csk-one"] != "sk-one" || got["c"] != "" || got["csk-three"] != "sk-three" {
		t.Errorf("keys after rotation: %v", got)
	}
}

func TestStoreUsageAndQuotaFields(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "emb", ProviderType: "openai", ModelName: "e"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRAGStore(ctx, &store.RAGStore{Name: "docs", EmbeddingConnectionID: conn.ID, ChunkSize: 100, ChunkOverlap: 10, TopK: 3,
		MaxDocuments: 5, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxDocuments != 5 || r.MaxBytes != 1<<20 || r.DocumentCount != 0 || r.BytesUsed != 0 {
		t.Fatalf("fresh store: %+v", r)
	}
	if docs, bytes, err := s.StoreUsage(ctx, r.ID); err != nil || docs != 0 || bytes != 0 {
		t.Fatalf("usage of empty store = %d, %d, %v", docs, bytes, err)
	}
	for i, body := range []string{"hello", "hello world"} {
		if _, err := s.CreateDocument(ctx, &store.Document{RAGStoreID: r.ID, Filename: "a.txt", SizeBytes: int64(len(body))}, []byte(body)); err != nil {
			t.Fatalf("document %d: %v", i, err)
		}
	}
	docs, bytes, err := s.StoreUsage(ctx, r.ID)
	if err != nil || docs != 2 || bytes != 16 {
		t.Fatalf("usage = %d docs, %d bytes, %v; want 2, 16", docs, bytes, err)
	}
	got, err := s.GetRAGStore(ctx, r.ID)
	if err != nil || got.DocumentCount != 2 || got.BytesUsed != 16 {
		t.Fatalf("store after uploads: %+v, %v", got, err)
	}
	list, err := s.ListRAGStores(ctx)
	if err != nil || len(list) != 1 || list[0].BytesUsed != 16 || list[0].MaxDocuments != 5 {
		t.Fatalf("list: %+v, %v", list, err)
	}
	r.MaxDocuments, r.MaxBytes = 0, 0
	if upd, err := s.UpdateRAGStore(ctx, r); err != nil || upd.MaxDocuments != 0 || upd.MaxBytes != 0 {
		t.Fatalf("update to unlimited: %+v, %v", upd, err)
	}
	// The CHECK constraints refuse negative quotas even when the API layer is bypassed.
	r.MaxBytes = -1
	if _, err := s.UpdateRAGStore(ctx, r); err == nil {
		t.Error("negative max_bytes should violate the check constraint")
	}
}

// A database written before instance_settings existed has credentials but no
// canary. Opening it once with the wrong key must not record that key as the
// database's: doing so inverts the check for good, because the correct key is
// then refused and rotate-key opens the store too, so it is refused with it.
func TestCanaryIsNotSealedByAKeyThatCannotOpenExistingCredentials(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	if _, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "o", ProviderType: "openai",
		APIKey: "sk-secret-123456", ModelName: "m"}); err != nil {
		t.Fatal(err)
	}
	// Roll the database back to what an upgrade finds: credentials, no canary.
	if _, err := s.DB().Exec(ctx, "DELETE FROM instance_settings WHERE key = 'secret_key_canary'"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	wrong := cfg
	wrong.SecretKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := store.Open(ctx, wrong, quiet); !errors.Is(err, store.ErrSecretKeyMismatch) {
		t.Fatalf("the wrong key should be refused, got %v", err)
	}

	// The refusal must have left nothing behind: the real key still opens it.
	s2, err := store.Open(ctx, cfg, quiet)
	if err != nil {
		t.Fatalf("the original key was locked out by the refused attempt: %v", err)
	}
	defer s2.Close()
	list, err := s2.ListConnections(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("connections: %v %d", err, len(list))
	}
	// ... and it is the one now recorded, so the wrong key is refused from here on.
	if _, err := store.Open(ctx, wrong, quiet); !errors.Is(err, store.ErrSecretKeyMismatch) {
		t.Errorf("after a good open the wrong key should still be refused, got %v", err)
	}
}

// A database with nothing encrypted yet cannot contradict any key, so the
// first one to arrive is adopted -- that is what makes a fresh install work
// without SECRET_KEY set.
func TestCanaryAdoptsTheFirstKeyOnAnEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	if _, err := s.DB().Exec(ctx, "DELETE FROM instance_settings WHERE key = 'secret_key_canary'"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	other := cfg
	other.SecretKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s2, err := store.Open(ctx, other, quiet)
	if err != nil {
		t.Fatalf("an empty database should adopt the key it is opened with: %v", err)
	}
	s2.Close()
	if _, err := store.Open(ctx, cfg, quiet); !errors.Is(err, store.ErrSecretKeyMismatch) {
		t.Errorf("once adopted, a different key must be refused, got %v", err)
	}
}
