package store_test

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// keyFixture is a schema with one user, two projects and the helpers the key
// tests share.
type keyFixture struct {
	t     *testing.T
	st    *store.Store
	ctx   context.Context
	user  *store.User
	other *store.User
	prod  *store.Project
	stage *store.Project
}

func newKeyFixture(t *testing.T) *keyFixture {
	t.Helper()
	ctx := context.Background()
	st := testdb.Open(t)
	u, err := st.CreateUser(ctx, "owner", "h", "editor")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateUser(ctx, "other", "h", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	prod, _, err := st.CreateProject(ctx, &store.Project{Name: "prod", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	stage, _, err := st.CreateProject(ctx, &store.Project{Name: "staging", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	return &keyFixture{t: t, st: st, ctx: ctx, user: u, other: other, prod: prod, stage: stage}
}

func (f *keyFixture) gatewayKey(name string, projects ...int64) (*store.APIKey, string) {
	f.t.Helper()
	k, raw, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: store.KindGateway, Name: name, UserID: f.user.ID,
		Scopes: store.DefaultGatewayScopes(), ProjectIDs: projects})
	if err != nil {
		f.t.Fatal(err)
	}
	return k, raw
}

func TestAPIKeyCRUDAndGrants(t *testing.T) {
	f := newKeyFixture(t)
	k, raw := f.gatewayKey("ci", f.prod.ID, f.stage.ID)
	if !strings.HasPrefix(raw, store.GatewayKeyPrefix) || len(raw) != len(store.GatewayKeyPrefix)+43 {
		t.Fatalf("generated key %q", raw)
	}
	if k.KeyPrefix != raw[:15] || k.Kind != store.KindGateway || len(k.ProjectIDs) != 2 {
		t.Fatalf("created key = %+v", k)
	}
	if !k.HasScope(store.ScopeChat) || k.HasScope(store.ScopeAdmin) {
		t.Errorf("scopes = %v", k.Scopes)
	}

	// The grant set is replaced wholesale, the credential never changes.
	k.ProjectIDs = []int64{f.stage.ID}
	k.Name, k.RateLimitRPM = "ci-staging", 7
	up, err := f.st.UpdateAPIKey(f.ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if len(up.ProjectIDs) != 1 || up.ProjectIDs[0] != f.stage.ID || up.Name != "ci-staging" || up.RateLimitRPM != 7 {
		t.Fatalf("updated key = %+v", up)
	}
	gk, err := f.st.ResolveGatewayKey(f.ctx, raw)
	if err != nil || gk.Key.ID != k.ID || gk.Owner.ID != f.user.ID {
		t.Fatalf("resolve after update: %+v %v", gk, err)
	}

	list, err := f.st.ListAPIKeys(f.ctx, store.APIKeyFilter{UserID: &f.user.ID})
	if err != nil || len(list) != 1 || list[0].Username != "owner" {
		t.Fatalf("list = %+v %v", list, err)
	}
	if list, err := f.st.ListAPIKeys(f.ctx, store.APIKeyFilter{Kind: store.KindManagement}); err != nil || len(list) != 0 {
		t.Fatalf("kind filter = %+v %v", list, err)
	}
}

func TestAPIKeyConstraints(t *testing.T) {
	f := newKeyFixture(t)
	f.gatewayKey("dup")
	if _, _, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: store.KindGateway, Name: "dup", UserID: f.user.ID}); !store.IsUniqueViolation(err) {
		t.Errorf("duplicate name per user: %v", err)
	}
	// The same name under another owner is fine.
	if _, _, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: store.KindGateway, Name: "dup", UserID: f.other.ID}); err != nil {
		t.Errorf("same name, other owner: %v", err)
	}
	// A management key may carry neither a project nor limits.
	if _, _, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: store.KindManagement, Name: "m1", UserID: f.user.ID,
		DefaultProjectID: &f.prod.ID}); err == nil {
		t.Error("management key with a default project was accepted")
	}
	if _, _, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: store.KindManagement, Name: "m2", UserID: f.user.ID,
		RateLimitRPM: 10}); err == nil {
		t.Error("management key with limits was accepted")
	}
	// ...nor a project grant. The admin handler refuses this first with a
	// message naming the field; 0014 is what holds the invariant up when
	// something writes grants without going through that handler.
	if _, _, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: store.KindManagement, Name: "m4", UserID: f.user.ID,
		ProjectIDs: []int64{f.prod.ID}}); err == nil {
		t.Error("management key with a project grant was accepted")
	}
	if _, _, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: "other", Name: "m3", UserID: f.user.ID}); err == nil {
		t.Error("unknown kind was accepted")
	}
	// Deleting the owner takes the key with it.
	k, raw := f.gatewayKey("cascade", f.prod.ID)
	if err := f.st.DeleteUser(f.ctx, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.GetAPIKey(f.ctx, k.ID); err == nil {
		t.Error("key outlived its owner")
	}
	if _, err := f.st.ResolveGatewayKey(f.ctx, raw); err == nil {
		t.Error("key of a deleted owner still resolves")
	}
}

func TestResolveGatewayKeyRejectsRetiredKeys(t *testing.T) {
	f := newKeyFixture(t)

	// Revoked.
	revoked, revokedRaw := f.gatewayKey("revoked", f.prod.ID)
	if _, err := f.st.RevokeAPIKey(f.ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	// Expired.
	past := time.Now().UTC().Add(-time.Hour)
	_, expiredRaw, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: store.KindGateway, Name: "expired",
		UserID: f.user.ID, ExpiresAt: &past})
	if err != nil {
		t.Fatal(err)
	}
	// Owner deactivated.
	_, inactiveRaw := f.gatewayKey("inactive", f.prod.ID)

	for _, c := range []struct{ name, raw, want string }{
		{"revoked", revokedRaw, store.KeyStateRevoked},
		{"expired", expiredRaw, store.KeyStateExpired},
		{"unknown", store.GatewayKeyPrefix + strings.Repeat("x", 43), store.KeyStateUnknown},
	} {
		if _, err := f.st.ResolveGatewayKey(f.ctx, c.raw); err != store.ErrNotFound {
			t.Errorf("%s resolved: %v", c.name, err)
		}
		got, err := f.st.GetAPIKeyState(f.ctx, c.raw)
		if err != nil || got != c.want {
			t.Errorf("%s state = %q (%v), want %q", c.name, got, err, c.want)
		}
	}
	if _, err := f.st.UpdateUser(f.ctx, f.user.ID, "editor", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ResolveGatewayKey(f.ctx, inactiveRaw); err != store.ErrNotFound {
		t.Errorf("key of a deactivated owner resolved: %v", err)
	}
	if got, _ := f.st.GetAPIKeyState(f.ctx, inactiveRaw); got != store.KeyStateOwnerInactive {
		t.Errorf("state of an inactive owner's key = %q", got)
	}
}

func TestManagementKeyResolution(t *testing.T) {
	f := newKeyFixture(t)
	k, raw, err := f.st.CreateAPIKey(f.ctx, &store.APIKey{Kind: store.KindManagement, Name: "ops",
		UserID: f.user.ID, Scopes: []string{store.ScopeRead, store.ScopeWrite}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, store.ManagementKeyPrefix) {
		t.Fatalf("prefix of %q", raw)
	}
	got, u, err := f.st.ResolveManagementKey(f.ctx, raw)
	if err != nil || got.ID != k.ID || u.ID != f.user.ID || u.Role != "editor" {
		t.Fatalf("resolve: %+v %+v %v", got, u, err)
	}
	// The kinds do not resolve through each other's lookup.
	if _, err := f.st.ResolveGatewayKey(f.ctx, raw); err == nil {
		t.Error("a management key resolved as a gateway key")
	}
	_, gwRaw := f.gatewayKey("gw")
	if _, _, err := f.st.ResolveManagementKey(f.ctx, gwRaw); err == nil {
		t.Error("a gateway key resolved as a management key")
	}
}

func TestDeleteAPIKeyKeepsBillingHistory(t *testing.T) {
	f := newKeyFixture(t)
	free, _ := f.gatewayKey("free", f.prod.ID)
	used, _ := f.gatewayKey("used", f.prod.ID)
	if err := f.st.InsertRequestLog(f.ctx, &store.RequestLog{ProjectID: f.prod.ID, ModelName: "m", StatusCode: 200,
		APIKeyID: &used.ID, UserID: &f.user.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DeleteAPIKey(f.ctx, free.ID); err != nil {
		t.Fatalf("delete an unused key: %v", err)
	}
	if err := f.st.DeleteAPIKey(f.ctx, used.ID); err != store.ErrKeyInUse {
		t.Fatalf("delete a key with history: %v", err)
	}
	if err := f.st.DeleteAPIKey(f.ctx, 99999); err != store.ErrNotFound {
		t.Fatalf("delete an unknown key: %v", err)
	}

	// The purge leaves keys that request logs still name, and takes the rest.
	if _, err := f.st.RevokeAPIKey(f.ctx, used.ID); err != nil {
		t.Fatal(err)
	}
	gone, _ := f.gatewayKey("gone")
	if _, err := f.st.RevokeAPIKey(f.ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	n, err := f.st.PurgeRetiredAPIKeys(f.ctx, time.Now().UTC().Add(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("purge removed %d key(s): %v", n, err)
	}
	if _, err := f.st.GetAPIKey(f.ctx, used.ID); err != nil {
		t.Errorf("a key with request logs was purged: %v", err)
	}
	rows, err := f.st.RecentRequests(f.ctx, store.MetricsFilter{ProjectID: &f.prod.ID}, 10)
	if err != nil || len(rows) != 1 || rows[0].APIKeyID == nil || *rows[0].UserID != f.user.ID {
		t.Fatalf("attribution lost: %+v %v", rows, err)
	}
}

func TestTouchAPIKeyUsedThrottles(t *testing.T) {
	f := newKeyFixture(t)
	k, _ := f.gatewayKey("touch")
	if k.LastUsedAt != nil {
		t.Fatalf("a fresh key has last_used_at = %v", k.LastUsedAt)
	}
	if err := f.st.TouchAPIKeyUsed(f.ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	first, err := f.st.GetAPIKey(f.ctx, k.ID)
	if err != nil || first.LastUsedAt == nil {
		t.Fatalf("first touch: %+v %v", first, err)
	}
	// A second touch inside the minute does not move the stamp.
	if err := f.st.TouchAPIKeyUsed(f.ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	second, err := f.st.GetAPIKey(f.ctx, k.ID)
	if err != nil || !second.LastUsedAt.Equal(*first.LastUsedAt) {
		t.Errorf("second touch moved the stamp: %v -> %v", first.LastUsedAt, second.LastUsedAt)
	}
}

func TestRevokeUserAPIKeys(t *testing.T) {
	f := newKeyFixture(t)
	a, aRaw := f.gatewayKey("a")
	f.gatewayKey("b")
	if _, err := f.st.RevokeAPIKey(f.ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	// Only the key that was still live counts.
	n, err := f.st.RevokeUserAPIKeys(f.ctx, f.user.ID)
	if err != nil || n != 1 {
		t.Fatalf("revoked %d key(s): %v", n, err)
	}
	if _, err := f.st.ResolveGatewayKey(f.ctx, aRaw); err != store.ErrNotFound {
		t.Errorf("revoked key still resolves: %v", err)
	}
	list, _ := f.st.ListAPIKeys(f.ctx, store.APIKeyFilter{UserID: &f.user.ID})
	for _, k := range list {
		if k.RevokedAt == nil {
			t.Errorf("key %q survived the revoke: %+v", k.Name, k)
		}
	}
}

func TestKeyUsageCounters(t *testing.T) {
	f := newKeyFixture(t)
	k, _ := f.gatewayKey("usage")
	w := store.UsageWindows{
		Minute: time.Date(2026, 9, 18, 10, 30, 0, 0, time.UTC),
		Day:    time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC),
		Month:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	n, err := f.st.ReserveKeyMinuteRequest(f.ctx, k.ID, w.Minute)
	if err != nil || n != 1 {
		t.Fatalf("reserve = %d, %v", n, err)
	}
	if err := f.st.ReleaseKeyMinuteRequest(f.ctx, k.ID, w.Minute); err != nil {
		t.Fatal(err)
	}
	if err := f.st.RecordKeyUsage(f.ctx, k.ID, w, 10, 4, true); err != nil {
		t.Fatal(err)
	}
	minute, day, month, err := f.st.GetKeyUsage(f.ctx, k.ID, w)
	if err != nil || minute.Requests != 1 || minute.Tokens() != 14 || day.Tokens() != 14 || month.Requests != 1 {
		t.Fatalf("usage: %+v %+v %+v %v", minute, day, month, err)
	}
	if got, err := f.st.MinuteKeyTokensSince(f.ctx, k.ID, w.Minute); err != nil || got != 14 {
		t.Errorf("minute tokens = %d, %v", got, err)
	}
	if _, err := f.st.DeleteKeyUsageBefore(f.ctx, store.PeriodMinute, w.Minute.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	minute, day, _, _ = f.st.GetKeyUsage(f.ctx, k.ID, w)
	if minute.Requests != 0 || day.Requests != 1 {
		t.Errorf("after purge: minute %+v day %+v", minute, day)
	}
}

// TestAPIKeyProjectsIsAGatewayOnlyTable checks the shape migration 0014 left
// behind, not only that a management key is refused a grant. The refusal has
// to come from a composite foreign key that *replaced* 0010's single-column
// one: two foreign keys side by side would keep accepting the row through
// the wider one, and the kind column only means anything while the CHECK
// pins it to 'gateway'.
func TestAPIKeyProjectsIsAGatewayOnlyTable(t *testing.T) {
	f := newKeyFixture(t)
	defs, err := f.st.TableConstraints(f.ctx, "api_key_projects")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(defs, "\n")
	for _, want := range []string{
		"FOREIGN KEY (api_key_id, kind) REFERENCES api_keys(id, kind) ON DELETE CASCADE",
		"kind = 'gateway'",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	// 0010's inline foreign key must be gone, not merely shadowed by the
	// composite one.
	if strings.Contains(all, "FOREIGN KEY (api_key_id) REFERENCES") {
		t.Errorf("the single-column foreign key survived:\n%s", all)
	}

	// A gateway key still holds grants, and deleting it still takes them
	// with it -- the cascade moved onto the new constraint.
	k, _ := f.gatewayKey("grants", f.prod.ID, f.stage.ID)
	if len(k.ProjectIDs) != 2 {
		t.Fatalf("gateway key grants = %v", k.ProjectIDs)
	}
	if err := f.st.DeleteAPIKey(f.ctx, k.ID); err != nil {
		t.Fatalf("delete a key that holds grants: %v", err)
	}
	if _, err := f.st.GetAPIKey(f.ctx, k.ID); err == nil {
		t.Error("the key survived its own deletion")
	}
}

// TestScopeBackfillMigration runs the 0018 SQL against keys shaped like the
// ones a v0.4 database holds. A fresh test schema has no such keys by the time
// the migration runs, so the file is re-executed on seeded rows; the migration
// is idempotent by contract, which is what makes that a faithful replay.
func TestScopeBackfillMigration(t *testing.T) {
	f := newKeyFixture(t)
	db := f.st.DB()
	old := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	insert := func(kind, name string, scopes []string, extra string) int64 {
		t.Helper()
		var id int64
		err := db.QueryRow(f.ctx, `INSERT INTO api_keys (kind, name, key_hash, key_prefix, user_id, scopes, updated_at`+
			extraCols(extra)+`) VALUES ($1, $2, $3, 'sk-', $4, $5, $6`+extraVals(extra)+`) RETURNING id`,
			kind, name, "hash-"+name, f.user.ID, scopes, old).Scan(&id)
		if err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
		return id
	}
	chatOnly := insert("gateway", "chat-only", []string{"chat"}, "")
	chatModels := insert("gateway", "chat-models", []string{"chat", "models"}, "")
	partial := insert("gateway", "partial", []string{"chat", "retrieve"}, "")
	full := insert("gateway", "full", []string{"chat", "models", "embeddings", "retrieve", "rerank"}, "")
	revoked := insert("gateway", "revoked", []string{"chat"}, "revoked")
	expired := insert("gateway", "expired", []string{"chat"}, "expired")
	modelsOnly := insert("gateway", "models-only", []string{"models"}, "")
	none := insert("gateway", "no-scopes", []string{}, "")
	mgmt := insert("management", "mgmt", []string{"read", "write", "admin", "keys"}, "")

	type row struct {
		Scopes    []string
		UpdatedAt time.Time
	}
	snapshot := func() map[int64]row {
		t.Helper()
		rows, err := db.Query(f.ctx, "SELECT id, scopes, updated_at FROM api_keys")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64]row{}
		for rows.Next() {
			var id int64
			var r row
			if err := rows.Scan(&id, &r.Scopes, &r.UpdatedAt); err != nil {
				t.Fatal(err)
			}
			out[id] = r
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	body, err := os.ReadFile("migrations/0018_backfill_v05_scopes.sql")
	if err != nil {
		t.Fatal(err)
	}
	run := func() int64 {
		t.Helper()
		tag, err := db.Exec(f.ctx, string(body))
		if err != nil {
			t.Fatalf("run 0018: %v", err)
		}
		return tag.RowsAffected()
	}
	before := snapshot()
	// chat-only, chat-models, partial, revoked and expired need scopes; full,
	// models-only, no-scopes and the management key do not match.
	if n := run(); n != 5 {
		t.Errorf("first run matched %d rows, want 5", n)
	}
	after := snapshot()

	all := []string{"embeddings", "retrieve", "rerank"}
	want := map[int64][]string{
		chatOnly:   append([]string{"chat"}, all...),
		chatModels: append([]string{"chat", "models"}, all...),
		partial:    {"chat", "retrieve", "embeddings", "rerank"},
		full:       {"chat", "models", "embeddings", "retrieve", "rerank"},
		revoked:    append([]string{"chat"}, all...),
		expired:    append([]string{"chat"}, all...),
		modelsOnly: {"models"},
		none:       {},
		mgmt:       {"read", "write", "admin", "keys"},
	}
	for id, w := range want {
		if !reflect.DeepEqual(after[id].Scopes, w) {
			t.Errorf("key %d scopes = %v, want %v", id, after[id].Scopes, w)
		}
		if !after[id].UpdatedAt.Equal(old) {
			t.Errorf("key %d updated_at = %v, want it untouched (%v)", id, after[id].UpdatedAt, old)
		}
	}
	if reflect.DeepEqual(before, after) {
		t.Fatal("backfill changed nothing")
	}

	// HasScope through the real read path.
	k, err := f.st.GetAPIKey(f.ctx, chatOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{store.ScopeChat, store.ScopeEmbeddings, store.ScopeRetrieve, store.ScopeRerank} {
		if !k.HasScope(s) {
			t.Errorf("chat key lacks %q after the backfill: %v", s, k.Scopes)
		}
	}
	m, err := f.st.GetAPIKey(f.ctx, modelsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{store.ScopeEmbeddings, store.ScopeRetrieve, store.ScopeRerank} {
		if m.HasScope(s) {
			t.Errorf("models-only key gained %q", s)
		}
	}

	// A second run matches no row at all, which is stronger than leaving the
	// values equal: it fails if the migration's guard is dropped and every chat
	// key is rewritten with the same value.
	if n := run(); n != 0 {
		t.Errorf("second run matched %d rows, want 0", n)
	}
	if again := snapshot(); !reflect.DeepEqual(after, again) {
		t.Errorf("second run changed rows:\nfirst  %v\nsecond %v", after, again)
	}
}

func extraCols(extra string) string {
	switch extra {
	case "revoked":
		return ", revoked_at"
	case "expired":
		return ", expires_at"
	}
	return ""
}

func extraVals(extra string) string {
	switch extra {
	case "revoked":
		return ", now() - interval '1 day'"
	case "expired":
		return ", now() - interval '1 day'"
	}
	return ""
}

// ---- network allowlists (ADR-039) ----

func TestNormalizeCIDRs(t *testing.T) {
	got, err := store.NormalizeCIDRs([]string{" 203.0.113.7 ", "10.0.0.0/8", "::ffff:192.168.0.0/112", "192.168.0.0/16",
		"2001:db8::1", "2001:DB8::/32", "::ffff:198.51.100.9", "0.0.0.0/0"})
	want := []string{"203.0.113.7/32", "10.0.0.0/8", "192.168.0.0/16", "2001:db8::1/128", "2001:db8::/32",
		"198.51.100.9/32", "0.0.0.0/0"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("NormalizeCIDRs = %v, %v; want %v", got, err, want)
	}
	if got, err := store.NormalizeCIDRs(nil); err != nil || got == nil || len(got) != 0 {
		t.Errorf("nil list = %v, %v; want a non-nil empty list", got, err)
	}
	for _, bad := range []string{"", "nope", "10.0.0.5/24", "10.0.0.0/33", "2001:db8::/129", "::ffff:10.0.0.0/64",
		"fe80::1%eth0", "10.0.0.0/8/9", "1.2.3"} {
		if _, err := store.NormalizeCIDRs([]string{bad}); err == nil {
			t.Errorf("NormalizeCIDRs(%q) accepted it", bad)
		}
	}
	many := make([]string, store.MaxKeyNetworkEntries+1)
	for i := range many {
		many[i] = "10.0." + strconv.Itoa(i) + ".0/24"
	}
	if _, err := store.NormalizeCIDRs(many); err == nil {
		t.Error("more than the maximum was accepted")
	}
	if got, err := store.NormalizeCIDRs(many[:store.MaxKeyNetworkEntries]); err != nil || len(got) != store.MaxKeyNetworkEntries {
		t.Errorf("exactly the maximum: %d, %v", len(got), err)
	}
}

func TestNormalizeOrigins(t *testing.T) {
	got, err := store.NormalizeOrigins([]string{"HTTPS://App.Example.com/", "https://app.example.com:443", "http://localhost:3000",
		"https://*.Example.org", "http://app.example.com:80", "https://[::1]:8443", "https://app.example.com:8443"})
	want := []string{"https://app.example.com", "http://localhost:3000", "https://*.example.org", "http://app.example.com",
		"https://[::1]:8443", "https://app.example.com:8443"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("NormalizeOrigins = %v, %v; want %v", got, err, want)
	}
	for _, bad := range []string{"", "*", " * ", "app.example.com", "ftp://a.example.com", "https://", "https://a.example.com/x",
		"https://a.example.com?x=1", "https://u@a.example.com", "https://*.com", "https://*", "https://a*.example.com",
		"https://*.*.example.com", "https://a.example.com:0", "https://a.example.com:65536", "https://a.example.com:80a",
		"https://-a.example.com", "https://a..example.com", "https://bücher.example", "https://a b.example.com",
		"https://[::1", "https://[zz]", "null"} {
		if _, err := store.NormalizeOrigins([]string{bad}); err == nil {
			t.Errorf("NormalizeOrigins(%q) accepted it", bad)
		}
	}
	many := make([]string, store.MaxKeyNetworkEntries+1)
	for i := range many {
		many[i] = "https://a" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".example.com"
	}
	if _, err := store.NormalizeOrigins(many); err == nil {
		t.Error("more than the maximum was accepted")
	}
}

func TestCheckKeyNetwork(t *testing.T) {
	cidrs := []string{"203.0.113.0/24", "2001:db8::/32", "198.51.100.7/32"}
	ipCases := []struct {
		remote string
		want   string
	}{
		{"203.0.113.9:5000", ""}, {"203.0.113.9", ""}, {"198.51.100.7", ""}, {"198.51.100.8", store.DenyIPNotAllowed},
		{"[2001:db8::5]:443", ""}, {"2001:db8:1::5", ""}, {"2001:dead::1", store.DenyIPNotAllowed},
		{"::ffff:203.0.113.9", ""}, {"[::ffff:198.51.100.8]:1", store.DenyIPNotAllowed},
		{"fe80::1%eth0", store.DenyIPNotAllowed}, {"", store.DenyIPNotAllowed}, {"not-an-address", store.DenyIPNotAllowed},
	}
	for _, c := range ipCases {
		if got := store.CheckKeyNetwork(cidrs, nil, c.remote, nil); got != c.want {
			t.Errorf("ip %q = %q, want %q", c.remote, got, c.want)
		}
	}
	// A list of IPv4 networks is matched by an IPv4-mapped client.
	if got := store.CheckKeyNetwork([]string{"10.0.0.0/8"}, nil, "[::ffff:10.1.2.3]:80", nil); got != "" {
		t.Errorf("IPv4-mapped client = %q, want it admitted", got)
	}
	// An empty list restricts nothing, whatever the address or Origin.
	if got := store.CheckKeyNetwork(nil, nil, "garbage", []string{"https://evil.example", "https://x"}); got != "" {
		t.Errorf("empty lists = %q", got)
	}

	origins := []string{"https://app.example.com", "https://*.example.org", "http://localhost:3000"}
	originCases := []struct {
		origin []string
		want   string
	}{
		{[]string{"https://app.example.com"}, ""}, {[]string{"HTTPS://APP.EXAMPLE.COM"}, ""},
		{[]string{"https://app.example.com:443"}, ""}, {[]string{"https://app.example.com:8443"}, store.DenyOriginNotAllowed},
		{[]string{"http://app.example.com"}, store.DenyOriginNotAllowed}, {[]string{"https://www.example.com"}, store.DenyOriginNotAllowed},
		{[]string{"https://a.example.org"}, ""}, {[]string{"https://a.b.example.org"}, ""},
		{[]string{"https://example.org"}, store.DenyOriginNotAllowed}, {[]string{"https://evilexample.org"}, store.DenyOriginNotAllowed},
		{[]string{"https://a.example.org.evil.com"}, store.DenyOriginNotAllowed}, {[]string{"http://a.example.org"}, store.DenyOriginNotAllowed},
		{[]string{"http://localhost:3000"}, ""}, {[]string{"http://localhost:3001"}, store.DenyOriginNotAllowed},
		{nil, store.DenyOriginNotAllowed}, {[]string{}, store.DenyOriginNotAllowed},
		{[]string{"null"}, store.DenyOriginNotAllowed}, {[]string{""}, store.DenyOriginNotAllowed},
		{[]string{"https://*.example.org"}, store.DenyOriginNotAllowed},
		{[]string{"https://app.example.com", "https://app.example.com"}, store.DenyOriginNotAllowed},
		{[]string{"https://app.example.com/path"}, store.DenyOriginNotAllowed},
	}
	for _, c := range originCases {
		if got := store.CheckKeyNetwork(nil, origins, "203.0.113.9", c.origin); got != c.want {
			t.Errorf("origin %q = %q, want %q", c.origin, got, c.want)
		}
	}
	// Both lists: the address is checked first.
	if got := store.CheckKeyNetwork(cidrs, origins, "192.0.2.1", []string{"https://evil.example"}); got != store.DenyIPNotAllowed {
		t.Errorf("both wrong = %q", got)
	}
	if got := store.CheckKeyNetwork(cidrs, origins, "203.0.113.9", []string{"https://evil.example"}); got != store.DenyOriginNotAllowed {
		t.Errorf("origin wrong = %q", got)
	}
	if got := store.CheckKeyNetwork(cidrs, origins, "203.0.113.9", []string{"https://app.example.com"}); got != "" {
		t.Errorf("both right = %q", got)
	}
}

func TestKeyNetworkColumnsRoundTrip(t *testing.T) {
	f := newKeyFixture(t)
	ctx := f.ctx
	k, raw, err := f.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: "net", UserID: f.user.ID,
		Scopes: store.DefaultGatewayScopes(), ProjectIDs: []int64{f.prod.ID},
		AllowedCIDRs: []string{"203.0.113.0/24", "2001:db8::/32", "198.51.100.7/32"}, AllowedOrigins: []string{"https://app.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(k.AllowedCIDRs, []string{"203.0.113.0/24", "2001:db8::/32", "198.51.100.7/32"}) ||
		!reflect.DeepEqual(k.AllowedOrigins, []string{"https://app.example.com"}) {
		t.Fatalf("created key = %v / %v", k.AllowedCIDRs, k.AllowedOrigins)
	}
	// The hot-path lookup carries the lists, so the gateway needs no second query.
	gk, err := f.st.ResolveGatewayKey(ctx, raw)
	if err != nil || len(gk.Key.AllowedCIDRs) != 3 || len(gk.Key.AllowedOrigins) != 1 {
		t.Fatalf("ResolveGatewayKey: %+v, %v", gk, err)
	}
	// A key written without lists reads [] and not nil.
	plain, _, err := f.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: "plain", UserID: f.user.ID,
		Scopes: store.DefaultGatewayScopes(), ProjectIDs: []int64{f.prod.ID}})
	if err != nil || plain.AllowedCIDRs == nil || len(plain.AllowedCIDRs) != 0 || plain.AllowedOrigins == nil || len(plain.AllowedOrigins) != 0 {
		t.Fatalf("plain key = %+v, %v", plain, err)
	}
	// Update replaces both lists; the database refuses a malformed network.
	k.AllowedCIDRs, k.AllowedOrigins = []string{"10.0.0.0/8"}, nil
	up, err := f.st.UpdateAPIKey(ctx, k)
	if err != nil || !reflect.DeepEqual(up.AllowedCIDRs, []string{"10.0.0.0/8"}) || len(up.AllowedOrigins) != 0 {
		t.Fatalf("update = %+v, %v", up, err)
	}
	k.AllowedCIDRs = []string{"10.0.0.5/24"}
	if _, err := f.st.UpdateAPIKey(ctx, k); err == nil {
		t.Error("the CIDR column accepted a network with host bits")
	}

	// The project key has its own two columns; an update that leaves them nil keeps them.
	p, _, err := f.st.CreateProject(ctx, &store.Project{Name: "netproj", ModelConnectionID: f.prod.ModelConnectionID,
		KeyAllowedCIDRs: []string{"192.0.2.0/24"}, KeyAllowedOrigins: []string{"https://site.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.KeyAllowedCIDRs, []string{"192.0.2.0/24"}) || !reflect.DeepEqual(p.KeyAllowedOrigins, []string{"https://site.example.com"}) {
		t.Fatalf("created project = %v / %v", p.KeyAllowedCIDRs, p.KeyAllowedOrigins)
	}
	p.KeyAllowedCIDRs, p.KeyAllowedOrigins = nil, nil
	p.SystemPrompt = "changed"
	if p, err = f.st.UpdateProject(ctx, p); err != nil || len(p.KeyAllowedCIDRs) != 1 || len(p.KeyAllowedOrigins) != 1 {
		t.Fatalf("a nil update must keep the lists: %+v, %v", p, err)
	}
	p.KeyAllowedCIDRs = []string{}
	if p, err = f.st.UpdateProject(ctx, p); err != nil || len(p.KeyAllowedCIDRs) != 0 || len(p.KeyAllowedOrigins) != 1 {
		t.Fatalf("an empty update must clear one list: %+v, %v", p, err)
	}
}

func TestCountCIDRRestricted(t *testing.T) {
	f := newKeyFixture(t)
	ctx := f.ctx
	if k, p, err := f.st.CountCIDRRestricted(ctx); err != nil || k != 0 || p != 0 {
		t.Fatalf("empty: %d %d %v", k, p, err)
	}
	mk := func(name string, cidrs []string) *store.APIKey {
		k, _, err := f.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: name, UserID: f.user.ID,
			Scopes: store.DefaultGatewayScopes(), ProjectIDs: []int64{f.prod.ID}, AllowedCIDRs: cidrs})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	live := mk("live", []string{"10.0.0.0/8"})
	revoked := mk("revoked", []string{"10.0.0.0/8"})
	mk("origins-only", nil)
	if _, err := f.st.RevokeAPIKey(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.st.CreateProject(ctx, &store.Project{Name: "withlist", ModelConnectionID: f.prod.ModelConnectionID,
		KeyAllowedCIDRs: []string{"192.0.2.0/24"}}); err != nil {
		t.Fatal(err)
	}
	if k, p, err := f.st.CountCIDRRestricted(ctx); err != nil || k != 1 || p != 1 {
		t.Errorf("counts = %d keys, %d projects, %v; want 1, 1", k, p, err)
	}
	_ = live
}
