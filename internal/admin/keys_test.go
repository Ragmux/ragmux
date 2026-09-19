package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// An HTTP-level harness for /admin/api. The routing × role × scope ×
// ownership matrix is where a bug turns into a privilege escalation, so
// these tests drive the real router with real sessions rather than calling
// the handlers directly.

type adminEnv struct {
	t    *testing.T
	st   *store.Store
	srv  *httptest.Server
	adm  *Admin
	conn *store.ModelConnection
	proj *store.Project
	// other is a project the non-admin users are not members of.
	other *store.Project
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	ctx := context.Background()
	st := testdb.Open(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	adm := &Admin{Store: st, Auth: &auth.Service{Store: st, TTL: time.Hour}, Log: log}
	r := chi.NewRouter()
	r.Route("/admin", adm.Routes)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	proj, _, err := st.CreateProject(ctx, &store.Project{Name: "prod", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := st.CreateProject(ctx, &store.Project{Name: "secret", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	return &adminEnv{t: t, st: st, srv: srv, adm: adm, conn: conn, proj: proj, other: other}
}

// user creates an account, makes it a member of every named project and
// returns it with a live session token.
type actor struct {
	*store.User
	token string
}

func (e *adminEnv) user(name, role string, projects ...int64) *actor {
	e.t.Helper()
	ctx := context.Background()
	u, err := e.st.CreateUser(ctx, name, "h", role)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, pid := range projects {
		if err := e.st.AddProjectMember(ctx, pid, u.ID); err != nil {
			e.t.Fatal(err)
		}
	}
	tok, err := e.adm.Auth.NewSession(ctx, u.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return &actor{User: u, token: tok}
}

// do sends an authenticated management request and decodes the JSON body.
func (e *adminEnv) do(method, path string, body any, bearer string) (int, map[string]any) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+"/admin"+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

// doList is do for endpoints that answer a JSON array.
func (e *adminEnv) doList(path, bearer string) (int, []any) {
	e.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, e.srv.URL+"/admin"+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errCode(out map[string]any) any {
	e, _ := out["error"].(map[string]any)
	if e == nil {
		return nil
	}
	return e["code"]
}

func errMessage(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	if e == nil {
		return ""
	}
	s, _ := e["message"].(string)
	return s
}

func keyID(t *testing.T, out map[string]any) int64 {
	t.Helper()
	k, _ := out["key"].(map[string]any)
	if k == nil {
		t.Fatalf("no key in %v", out)
	}
	id, _ := k["id"].(float64)
	return int64(id)
}

// A viewer can mint a gateway key for a project it belongs to; that is the
// intended flow (ADR-003). What makes it safe is this gate, not a role: the
// grants are held to what the owner could reach at creation time, which is
// what the refusals below cover. The role is not a second bound here -- /v1
// never reads one -- so what limits the spend afterwards is the project's own
// rate limits and budgets and the key's sub-limits under them.
func TestCreateGatewayKeyMembershipGate(t *testing.T) {
	e := newAdminEnv(t)
	viewer := e.user("viewer", "viewer", e.proj.ID)

	status, out := e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "ci", "project_ids": []int64{e.proj.ID}}, viewer.token)
	if status != http.StatusCreated {
		t.Fatalf("viewer create: %d %v", status, out)
	}
	secret, _ := out["api_key"].(string)
	if len(secret) != len(store.GatewayKeyPrefix)+43 {
		t.Fatalf("api_key = %q", secret)
	}
	k, _ := out["key"].(map[string]any)
	if scopes, _ := k["scopes"].([]any); len(scopes) != 2 {
		t.Errorf("default scopes = %v", k["scopes"])
	}

	// A project the owner is not a member of is refused at creation time.
	status, out = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "sneaky", "project_ids": []int64{e.other.ID}}, viewer.token)
	if status != http.StatusBadRequest || errMessage(out) == "" {
		t.Errorf("non-member project: %d %v", status, out)
	}
	// An unknown project is refused too.
	status, _ = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "ghost", "project_ids": []int64{99999}}, viewer.token)
	if status != http.StatusBadRequest {
		t.Errorf("unknown project: %d", status)
	}
	// default_project_id must be one of the grants.
	status, _ = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "bad-default", "project_ids": []int64{e.proj.ID},
		"default_project_id": e.other.ID}, viewer.token)
	if status != http.StatusBadRequest {
		t.Errorf("default outside the grants: %d", status)
	}
	// Names are unique per owner.
	status, _ = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "ci", "project_ids": []int64{e.proj.ID}}, viewer.token)
	if status != http.StatusConflict {
		t.Errorf("duplicate name: %d", status)
	}
}

func TestCreateManagementKeyGates(t *testing.T) {
	e := newAdminEnv(t)
	viewer := e.user("viewer", "viewer", e.proj.ID)
	editor := e.user("editor", "editor", e.proj.ID)
	admin := e.user("admin", "admin")

	// A viewer may not mint a management key at all.
	status, _ := e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "management", "name": "ops", "scopes": []string{store.ScopeRead}}, viewer.token)
	if status != http.StatusForbidden {
		t.Errorf("viewer management key: %d", status)
	}
	// An editor may, but not with the admin scope.
	status, out := e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "management", "name": "ops", "scopes": []string{store.ScopeAdmin}}, editor.token)
	if status != http.StatusBadRequest || errMessage(out) == "" {
		t.Errorf("editor asking for the admin scope: %d %v", status, out)
	}
	status, out = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "management", "name": "ops", "scopes": []string{store.ScopeRead, store.ScopeWrite}}, editor.token)
	if status != http.StatusCreated {
		t.Fatalf("editor management key: %d %v", status, out)
	}
	// Projects and limits have no meaning on a management key.
	status, _ = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "management", "name": "limited", "scopes": []string{store.ScopeRead},
		"rate_limit_rpm": 10}, editor.token)
	if status != http.StatusBadRequest {
		t.Errorf("management key with limits: %d", status)
	}
	status, _ = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "management", "name": "scoped", "scopes": []string{}}, editor.token)
	if status != http.StatusBadRequest {
		t.Errorf("management key without scopes: %d", status)
	}
	// Only an admin may mint a key owned by someone else.
	status, _ = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "for-other", "user_id": viewer.ID}, editor.token)
	if status != http.StatusForbidden {
		t.Errorf("editor minting for another user: %d", status)
	}
	status, out = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "for-viewer", "user_id": viewer.ID,
		"project_ids": []int64{e.proj.ID}}, admin.token)
	if status != http.StatusCreated {
		t.Fatalf("admin minting for a viewer: %d %v", status, out)
	}
	// ... but the grant is still bounded by the owner, not by the admin.
	status, out = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "overreach", "user_id": viewer.ID,
		"project_ids": []int64{e.other.ID}}, admin.token)
	if status != http.StatusBadRequest {
		t.Errorf("admin granting a project the owner cannot see: %d %v", status, out)
	}
}

func TestKeyOwnershipIsolation(t *testing.T) {
	e := newAdminEnv(t)
	alice := e.user("alice", "editor", e.proj.ID)
	bob := e.user("bob", "editor", e.proj.ID)
	admin := e.user("admin", "admin")

	_, out := e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "alice-key", "project_ids": []int64{e.proj.ID}}, alice.token)
	id := keyID(t, out)

	// Someone else's key is a 404 on every route, not a 403: ids stay
	// unenumerable.
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/keys/"},
		{http.MethodPut, "/api/keys/"},
		{http.MethodPost, "/api/keys/%s/revoke"},
		{http.MethodGet, "/api/keys/%s/usage"},
	} {
		path := "/api/keys/" + itoa(id)
		if c.path != "/api/keys/" {
			path = "/api/keys/" + itoa(id) + c.path[len("/api/keys/%s"):]
		}
		body := any(nil)
		if c.method == http.MethodPut {
			body = map[string]any{"name": "stolen"}
		}
		if status, _ := e.do(c.method, path, body, bob.token); status != http.StatusNotFound {
			t.Errorf("%s %s as another user: %d", c.method, path, status)
		}
	}
	// An admin reaches it.
	if status, _ := e.do(http.MethodGet, "/api/keys/"+itoa(id), nil, admin.token); status != http.StatusOK {
		t.Errorf("admin reading someone's key: %d", status)
	}

	// The listing shows only your own keys; admins see every owner and can
	// narrow with ?user_id.
	_, _ = e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "bob-key", "project_ids": []int64{e.proj.ID}}, bob.token)
	if status, list := e.doList("/api/keys", bob.token); status != 200 || len(list) != 1 {
		t.Errorf("bob's listing: %d %v", status, list)
	}
	// ?user_id is ignored for a non-admin.
	if status, list := e.doList("/api/keys?user_id="+itoa(alice.ID), bob.token); status != 200 || len(list) != 1 {
		t.Errorf("bob asking for alice's keys: %d %v", status, list)
	}
	if status, list := e.doList("/api/keys", admin.token); status != 200 || len(list) != 2 {
		t.Errorf("admin listing: %d %v", status, list)
	}
	if status, list := e.doList("/api/keys?user_id="+itoa(alice.ID), admin.token); status != 200 || len(list) != 1 {
		t.Errorf("admin narrowing: %d %v", status, list)
	}
	if status, list := e.doList("/api/keys?kind=management", admin.token); status != 200 || len(list) != 0 {
		t.Errorf("kind filter: %d %v", status, list)
	}
	if status, _ := e.do(http.MethodGet, "/api/keys?kind=bogus", nil, admin.token); status != http.StatusBadRequest {
		t.Errorf("bad kind: %d", status)
	}
}

func TestRevokeUpdateAndDelete(t *testing.T) {
	e := newAdminEnv(t)
	alice := e.user("alice", "editor", e.proj.ID)
	admin := e.user("admin", "admin")
	_, out := e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "k", "project_ids": []int64{e.proj.ID}}, alice.token)
	id := keyID(t, out)
	secret, _ := out["api_key"].(string)

	status, out := e.do(http.MethodPut, "/api/keys/"+itoa(id), map[string]any{
		"name": "renamed", "project_ids": []int64{e.proj.ID}, "rate_limit_rpm": 5,
		"expires_at": time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)}, alice.token)
	if status != http.StatusOK || out["name"] != "renamed" || out["rate_limit_rpm"] != float64(5) || out["expires_at"] == nil {
		t.Fatalf("update: %d %v", status, out)
	}
	// The kind is immutable and a malformed expiry is a 400.
	if status, _ := e.do(http.MethodPut, "/api/keys/"+itoa(id), map[string]any{
		"kind": "management", "name": "renamed"}, alice.token); status != http.StatusBadRequest {
		t.Errorf("changing the kind: %d", status)
	}
	if status, _ := e.do(http.MethodPut, "/api/keys/"+itoa(id), map[string]any{
		"name": "renamed", "expires_at": "not-a-date"}, alice.token); status != http.StatusBadRequest {
		t.Errorf("bad expires_at: %d", status)
	}

	// Deleting is admin-only; the owner revokes instead.
	if status, _ := e.do(http.MethodDelete, "/api/keys/"+itoa(id), nil, alice.token); status != http.StatusForbidden {
		t.Errorf("owner deleting: %d", status)
	}
	status, out = e.do(http.MethodPost, "/api/keys/"+itoa(id)+"/revoke", nil, alice.token)
	if status != http.StatusOK || out["revoked_at"] == nil {
		t.Fatalf("revoke: %d %v", status, out)
	}
	// Request logs pin the key: delete refuses with 409 until they are gone.
	if err := e.st.InsertRequestLog(context.Background(), &store.RequestLog{ProjectID: e.proj.ID, ModelName: "m",
		StatusCode: 200, APIKeyID: &id, UserID: &alice.ID}); err != nil {
		t.Fatal(err)
	}
	status, out = e.do(http.MethodDelete, "/api/keys/"+itoa(id), nil, admin.token)
	if status != http.StatusConflict || errCode(out) != "key_in_use" {
		t.Fatalf("delete with history: %d %v", status, out)
	}
	if _, err := e.st.DeleteRequestLogsBefore(context.Background(), time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.do(http.MethodDelete, "/api/keys/"+itoa(id), nil, admin.token); status != http.StatusOK {
		t.Errorf("delete without history: %d", status)
	}

	// Every mutation is audited, and no entry carries the credential.
	entries, _, err := e.st.ListAuditLogs(context.Background(), store.AuditFilter{Action: "apikey."})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range entries {
		seen[a.Action] = true
		raw, _ := json.Marshal(a.Details)
		// The 15-character display prefix is deliberate; the credential and
		// its hash must never appear.
		if (secret != "" && bytes.Contains(raw, []byte(secret))) || bytes.Contains(raw, []byte("key_hash")) {
			t.Errorf("audit entry leaks the key: %s", raw)
		}
	}
	for _, want := range []string{"apikey.create", "apikey.update", "apikey.revoke", "apikey.delete"} {
		if !seen[want] {
			t.Errorf("no %s audit entry: %v", want, seen)
		}
	}
}

func TestKeyUsageEndpoint(t *testing.T) {
	e := newAdminEnv(t)
	alice := e.user("alice", "editor", e.proj.ID)
	_, out := e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "k", "project_ids": []int64{e.proj.ID},
		"budget_daily_tokens": 100}, alice.token)
	id := keyID(t, out)
	status, out := e.do(http.MethodGet, "/api/keys/"+itoa(id)+"/usage", nil, alice.token)
	if status != http.StatusOK || out["api_key_id"] != float64(id) || out["project_id"] != nil {
		t.Fatalf("usage: %d %v", status, out)
	}
	day, _ := out["day"].(map[string]any)
	if day == nil || day["token_limit"] != float64(100) {
		t.Errorf("day window: %v", out["day"])
	}
}

// A management key is bounded by its scopes and by its owner's live role.
func TestManagementKeyScopeAndRoleMatrix(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	editor := e.user("editor", "editor", e.proj.ID)
	admin := e.user("admin", "admin")
	e.user("second-admin", "admin") // so the demotion below is allowed

	mint := func(owner *actor, name string, scopes ...string) string {
		e.t.Helper()
		_, raw, err := e.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindManagement, Name: name,
			UserID: owner.ID, Scopes: scopes})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	readKey := mint(editor, "read", store.ScopeRead)
	writeKey := mint(editor, "write", store.ScopeRead, store.ScopeWrite)
	// An admin's key with only the write scope: the role allows the admin
	// routes, the scopes do not.
	adminWriteKey := mint(admin, "write", store.ScopeRead, store.ScopeWrite)
	adminAllKey := mint(admin, "all", store.ScopeRead, store.ScopeWrite, store.ScopeAdmin)

	body := map[string]any{"name": "new", "provider_type": "openai", "model_name": "gpt-4o"}
	cases := []struct {
		name, method, path string
		body               any
		bearer             string
		want               int
	}{
		{"read scope reads", http.MethodGet, "/api/projects", nil, readKey, 200},
		{"read scope cannot write", http.MethodPost, "/api/models", body, readKey, 403},
		{"write scope writes", http.MethodPost, "/api/models", body, writeKey, 201},
		{"write scope cannot reach admin routes", http.MethodGet, "/api/users", nil, writeKey, 403},
		{"an admin's key without the admin scope cannot either", http.MethodGet, "/api/users", nil, adminWriteKey, 403},
		{"admin scope plus the admin role can", http.MethodGet, "/api/users", nil, adminAllKey, 200},
		{"no keys scope, no key listing", http.MethodGet, "/api/keys", nil, adminAllKey, 403},
		{"identity needs no scope", http.MethodGet, "/api/me", nil, readKey, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if status, out := e.do(c.method, c.path, c.body, c.bearer); status != c.want {
				t.Errorf("status %d, want %d (%v)", status, c.want, out)
			}
		})
	}

	// Demoting the owner narrows every key it holds on the very next request.
	if _, err := e.st.UpdateUser(ctx, admin.ID, "viewer", true); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.do(http.MethodGet, "/api/users", nil, adminAllKey); status != http.StatusForbidden {
		t.Errorf("key of a demoted admin still reaches /api/users: %d", status)
	}
	// Deactivating stops it resolving at all.
	if _, err := e.st.UpdateUser(ctx, editor.ID, "editor", false); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.do(http.MethodGet, "/api/projects", nil, readKey); status != http.StatusUnauthorized {
		t.Errorf("key of a deactivated owner: %d", status)
	}
}

// A leaked management key must not be able to take over the account it
// belongs to, nor mint a second key of its own kind.
func TestSessionOnlyActions(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	admin := e.user("admin", "admin")
	_, raw, err := e.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindManagement, Name: "everything",
		UserID: admin.ID, Scopes: store.ManagementScopes()})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, method, path string
		body               any
	}{
		{"own password", http.MethodPost, "/api/me/password", map[string]any{
			"current_password": "h", "new_password": "a-new-password"}},
		{"another account's password", http.MethodPost, "/api/users/" + itoa(admin.ID) + "/reset-password",
			map[string]any{"new_password": "a-new-password"}},
		{"a second management key", http.MethodPost, "/api/keys", map[string]any{
			"kind": "management", "name": "child", "scopes": []string{store.ScopeRead}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, out := e.do(c.method, c.path, c.body, raw)
			if status != http.StatusForbidden || errCode(out) != "session_required" {
				t.Errorf("status %d body %v", status, out)
			}
		})
	}
	// A gateway key is still fine to mint from a management key: it cannot
	// touch the management API at all.
	if status, out := e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "gateway", "name": "child-gw", "project_ids": []int64{e.proj.ID}}, raw); status != http.StatusCreated {
		t.Errorf("gateway key from a management key: %d %v", status, out)
	}
	// The same actions work from the session the key belongs to.
	if status, out := e.do(http.MethodPost, "/api/keys", map[string]any{
		"kind": "management", "name": "child", "scopes": []string{store.ScopeRead}}, admin.token); status != http.StatusCreated {
		t.Errorf("management key from a session: %d %v", status, out)
	}
}

func TestMeReportsHowTheRequestAuthenticated(t *testing.T) {
	e := newAdminEnv(t)
	admin := e.user("admin", "admin")
	status, out := e.do(http.MethodGet, "/api/me", nil, admin.token)
	if status != 200 || out["session_kind"] != "session" || out["session_expires_at"] == "" || out["scopes"] != nil {
		t.Fatalf("session: %d %v", status, out)
	}
	_, raw, err := e.st.CreateAPIKey(context.Background(), &store.APIKey{Kind: store.KindManagement,
		Name: "k", UserID: admin.ID, Scopes: []string{store.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	status, out = e.do(http.MethodGet, "/api/me", nil, raw)
	scopes, _ := out["scopes"].([]any)
	if status != 200 || out["session_kind"] != "api_key" || len(scopes) != 1 || out["username"] != "admin" {
		t.Fatalf("api key: %d %v", status, out)
	}
	// A key without an expiry reports none rather than the zero time.
	if out["session_expires_at"] != "" {
		t.Errorf("session_expires_at = %v", out["session_expires_at"])
	}
}

func TestSetupStatusReportsWizardProgress(t *testing.T) {
	e := newAdminEnv(t)
	status, out := e.do(http.MethodGet, "/api/setup", nil, "")
	if status != 200 || out["has_connections"] != true || out["has_projects"] != true || out["needs_setup"] != true {
		t.Fatalf("with data: %d %v", status, out)
	}
	ctx := context.Background()
	if err := e.st.DeleteProject(ctx, e.proj.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteProject(ctx, e.other.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteConnection(ctx, e.conn.ID); err != nil {
		t.Fatal(err)
	}
	_, out = e.do(http.MethodGet, "/api/setup", nil, "")
	if out["has_connections"] != false || out["has_projects"] != false {
		t.Errorf("empty: %v", out)
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// A narrow management key must not be able to widen itself. Minting a
// management key requires a session precisely so a leaked one cannot extend
// its own reach; updating one has to hold the same line, or the guard on
// create is decorative.
func TestManagementKeyCannotWidenItself(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	admin := e.user("admin", "admin")
	k, raw, err := e.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindManagement, Name: "ci",
		UserID: admin.ID, Scopes: []string{store.ScopeKeys}})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"name": "ci", "scopes": store.ManagementScopes(),
		"expires_at": "2099-01-01T00:00:00Z"}
	status, _ := e.do(http.MethodPut, "/api/keys/"+itoa(k.ID), body, raw)
	if status != http.StatusForbidden {
		t.Fatalf("widening its own scopes: %d, want 403", status)
	}
	after, err := e.st.GetAPIKey(ctx, k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Scopes) != 1 || after.Scopes[0] != store.ScopeKeys {
		t.Errorf("scopes after the refused update = %v", after.Scopes)
	}
	if after.ExpiresAt != nil {
		t.Errorf("expiry was extended to %v", after.ExpiresAt)
	}
	// The owner, at a keyboard, can still do it.
	if status, _ := e.do(http.MethodPut, "/api/keys/"+itoa(k.ID), body, admin.token); status != http.StatusOK {
		t.Errorf("session update: %d, want 200", status)
	}
}

// Changing a password is how someone reacts to a suspected leak. A
// management key minted from the leaked session outlives every session, so
// it has to die with them; a gateway key, which cannot touch the management
// API, keeps running.
func TestPasswordChangeRevokesManagementKeys(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	admin := e.user("admin", "admin")
	// e.user stores a placeholder hash; the change has to verify the current
	// password, so give the account a real one.
	pw := "the-current-password"
	hash, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpdateUserPassword(ctx, admin.ID, hash); err != nil {
		t.Fatal(err)
	}
	mgmt, mgmtRaw, err := e.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindManagement,
		Name: "minted-by-the-attacker", UserID: admin.ID, Scopes: store.ManagementScopes()})
	if err != nil {
		t.Fatal(err)
	}
	gw, _, err := e.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: "app",
		UserID: admin.ID, Scopes: store.DefaultGatewayScopes(), ProjectIDs: []int64{e.proj.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := e.do(http.MethodGet, "/api/projects", nil, mgmtRaw); status != http.StatusOK {
		t.Fatalf("the management key should work before the change")
	}
	status, out := e.do(http.MethodPost, "/api/me/password",
		map[string]any{"current_password": pw, "new_password": "a-new-password"}, admin.token)
	if status != http.StatusOK {
		t.Fatalf("password change: %d %v", status, out)
	}
	if n, _ := out["revoked_management_keys"].(float64); n != 1 {
		t.Errorf("revoked_management_keys = %v, want 1", out["revoked_management_keys"])
	}
	if status, _ := e.do(http.MethodGet, "/api/projects", nil, mgmtRaw); status != http.StatusUnauthorized {
		t.Errorf("the management key still works after the password change: %d", status)
	}
	after, err := e.st.GetAPIKey(ctx, mgmt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RevokedAt == nil {
		t.Error("management key not revoked")
	}
	stillLive, err := e.st.GetAPIKey(ctx, gw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillLive.RevokedAt != nil {
		t.Error("a gateway key was revoked; it cannot reach the management API and should keep running")
	}
}

// Deleting a user cascades to its api keys, and that would null the
// attribution on every request log those keys made -- the same loss
// DeleteAPIKey refuses one key at a time, arriving in bulk through a path
// that never mentions keys.
func TestDeletingAUserWithAttributedSpendIsRefused(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	admin := e.user("admin", "admin")
	victim := e.user("app-owner", "editor", e.proj.ID)
	k, _, err := e.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: "app",
		UserID: victim.ID, Scopes: store.DefaultGatewayScopes(), ProjectIDs: []int64{e.proj.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertRequestLog(ctx, &store.RequestLog{ProjectID: e.proj.ID, ModelName: "m",
		StatusCode: 200, PromptTokens: 10, CompletionTokens: 2, APIKeyID: &k.ID, UserID: &victim.ID,
		CostSource: store.CostSourceNone}); err != nil {
		t.Fatal(err)
	}
	status, out := e.do(http.MethodDelete, "/api/users/"+itoa(victim.ID), nil, admin.token)
	if status != http.StatusConflict || errCode(out) != "user_has_attributed_usage" {
		t.Fatalf("delete: %d %v", status, out)
	}
	if _, err := e.st.GetUser(ctx, victim.ID); err != nil {
		t.Errorf("the account was removed anyway: %v", err)
	}
	// Without spend history the account deletes as before.
	spare := e.user("no-spend", "viewer")
	if status, out := e.do(http.MethodDelete, "/api/users/"+itoa(spare.ID), nil, admin.token); status != http.StatusOK {
		t.Errorf("deleting an account with no attributed usage: %d %v", status, out)
	}
}
