package store_test

import (
	"context"
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
		Scopes: store.DefaultGatewayScopes, ProjectIDs: projects})
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
