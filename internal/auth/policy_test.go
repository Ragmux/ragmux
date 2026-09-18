package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func TestRoleMatrix(t *testing.T) {
	cases := []struct {
		role Role
		min  Role
		want bool
	}{
		{RoleAdmin, RoleAdmin, true}, {RoleAdmin, RoleEditor, true}, {RoleAdmin, RoleViewer, true},
		{RoleEditor, RoleAdmin, false}, {RoleEditor, RoleEditor, true}, {RoleEditor, RoleViewer, true},
		{RoleViewer, RoleAdmin, false}, {RoleViewer, RoleEditor, false}, {RoleViewer, RoleViewer, true},
		{Role(""), RoleViewer, false}, {Role("root"), RoleViewer, false},
	}
	for _, c := range cases {
		if got := c.role.AtLeast(c.min); got != c.want {
			t.Errorf("%q.AtLeast(%q) = %v, want %v", c.role, c.min, got, c.want)
		}
	}
	for _, r := range []string{"admin", "editor", "viewer"} {
		if !IsValidRole(r) {
			t.Errorf("%q should be valid", r)
		}
	}
	if IsValidRole("owner") || IsValidRole("") {
		t.Error("unknown roles must be invalid")
	}
}

func TestRequireRole(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	cases := []struct {
		user   *store.User
		min    Role
		status int
	}{
		{nil, RoleViewer, http.StatusUnauthorized},
		{&store.User{Role: "viewer"}, RoleViewer, http.StatusNoContent},
		{&store.User{Role: "viewer"}, RoleEditor, http.StatusForbidden},
		{&store.User{Role: "editor"}, RoleEditor, http.StatusNoContent},
		{&store.User{Role: "editor"}, RoleAdmin, http.StatusForbidden},
		{&store.User{Role: "admin"}, RoleAdmin, http.StatusNoContent},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.user != nil {
			req = req.WithContext(ContextWithUser(req.Context(), c.user))
		}
		rec := httptest.NewRecorder()
		RequireRole(c.min)(ok).ServeHTTP(rec, req)
		if rec.Code != c.status {
			t.Errorf("user %+v min %q: status %d, want %d (body %s)", c.user, c.min, rec.Code, c.status, rec.Body)
		}
		if rec.Code == http.StatusForbidden && rec.Body.String() != `{"error":{"message":"insufficient role","type":"forbidden"}}` {
			t.Errorf("unexpected forbidden body %s", rec.Body)
		}
	}
}

func TestCanAccessProject(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	admin, _ := s.CreateUser(ctx, "admin", "h", "admin")
	editor, _ := s.CreateUser(ctx, "editor", "h", "editor")
	viewer, _ := s.CreateUser(ctx, "viewer", "h", "viewer")
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "ollama", ModelName: "x"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddProjectMember(ctx, p.ID, editor.ID); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		user *store.User
		want bool
	}{
		{"admin", admin, true}, {"member editor", editor, true}, {"non-member viewer", viewer, false}, {"anonymous", nil, false},
	}
	for _, c := range cases {
		got, err := CanAccessProject(ctx, s, c.user, p.ID)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v %v, want %v", c.name, got, err, c.want)
		}
	}
	if got, _ := CanAccessProject(ctx, s, editor, p.ID+1000); got {
		t.Error("unknown project must not be accessible to non-admins")
	}
}
