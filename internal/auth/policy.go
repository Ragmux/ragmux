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
//
// A role bounds the *admin surface*, not gateway spend. The key routes are
// deliberately role-free -- every account manages its own API keys -- so a
// viewer can mint itself an sk-user-… key and spend through /v1. That is not
// a hole in the matrix above: spend is bounded by the project's limits and
// the key's own sub-limits, never by a role. A user carries no limit of its
// own.
//
// What stops a gateway key is revoking it, giving it a small sub-limit, or
// deactivating the owner (gateway.go answers key_owner_inactive). Two things
// that look like they would and do not: dropping the owner's project
// membership, because grants are frozen when the key is created and outlive
// membership on purpose (gateway.selectProject), and setting a limit to 0,
// because 0 means unlimited here (limits.tighter).
//
// Demotion is a management-key tool, not a gateway one. Role is intersected
// with a key's scopes on the /admin/api side only -- RequireRole below reads
// the owner's live role on every request -- so demoting someone narrows the
// sk-mgmt-… keys they hold at once. It changes nothing about an sk-user-…
// key: /v1 never consults a role, and chat and models are scopes every role
// including viewer already covers. Decided in ADR-003;
// docs/users-and-limits.md states the same split.
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

// RequireScope narrows an api-key principal to the scopes its key carries.
// It is a no-op for a dashboard session, which is already bounded by the
// user's role, and it never widens anything: RequireRole still runs on the
// same route and still consults the owner's live role, so demoting or
// deactivating a user immediately narrows every sk-mgmt-… key they hold. A
// key's effective permission on this surface is its scopes intersected with
// that role. It says nothing about /v1, which never reads a role -- see the
// note on Role above.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			se := SessionFrom(r.Context())
			if se.IsKey() && !hasScope(se.Scopes, scope) {
				insufficientScope(w, scope)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

func insufficientScope(w http.ResponseWriter, scope string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":{"message":"api key is not authorised for the ` + scope +
		` scope","type":"forbidden","code":"insufficient_scope"}}`))
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
