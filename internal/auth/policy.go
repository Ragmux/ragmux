package auth

import (
	"context"
	"net/http"

	"github.com/ragmux/ragmux/internal/store"
)

// Role is a dashboard permission level. This file is the single source of
// truth for what each role may do:
//
//   - admin:  everything, including user management and the audit log; sees
//     every project and global metrics.
//   - editor: creates and edits model connections, RAG stores and documents;
//     creates projects (becoming a member) and manages only the projects it is
//     a member of, including their members, keys and metrics.
//   - viewer: read-only on model connections, RAG stores and documents
//     (including search and connection tests); sees only member projects and
//     their metrics.
//
// Non-member projects are reported as not found (404) rather than forbidden so
// project ids do not leak.
type Role string

// The three roles, ordered from most to least privileged.
const (
	RoleAdmin  Role = "admin"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

var roleRank = map[Role]int{RoleViewer: 1, RoleEditor: 2, RoleAdmin: 3}

// IsValidRole reports whether s names a known role.
func IsValidRole(s string) bool {
	_, ok := roleRank[Role(s)]
	return ok
}

// AtLeast reports whether r grants at least the privileges of min.
func (r Role) AtLeast(min Role) bool {
	return roleRank[r] >= roleRank[min]
}

// RequireRole rejects requests whose authenticated user is below min. It must
// run after Middleware.
func RequireRole(min Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := UserFrom(r.Context())
			if u == nil {
				unauthorized(w)
				return
			}
			if !Role(u.Role).AtLeast(min) {
				Forbidden(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Forbidden writes the standard 403 response.
func Forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":{"message":"insufficient role","type":"forbidden"}}`))
}

// CanAccessProject reports whether the user may see the project: admins see
// everything, everyone else only projects they are a member of.
func CanAccessProject(ctx context.Context, st *store.Store, u *store.User, projectID int64) (bool, error) {
	if u == nil {
		return false, nil
	}
	if Role(u.Role).AtLeast(RoleAdmin) {
		return true, nil
	}
	return st.IsProjectMember(ctx, projectID, u.ID)
}
