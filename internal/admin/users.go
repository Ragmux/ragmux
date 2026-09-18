package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
)

// ---- users (admin only, except the lite listing) ----

type liteUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// listUsersLite exposes just enough for editors to pick project members.
func (a *Admin) listUsersLite(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.ListUsers(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	out := make([]liteUser, 0, len(list))
	for _, u := range list {
		if u.IsActive {
			out = append(out, liteUser{ID: u.ID, Username: u.Username, Role: u.Role})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) listUsers(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.ListUsers(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *Admin) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || len(in.Username) > 64 {
		writeErr(w, http.StatusBadRequest, "username is required (max 64 characters)")
		return
	}
	if len(in.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	if in.Role == "" {
		in.Role = string(auth.RoleViewer)
	}
	if !auth.IsValidRole(in.Role) {
		writeErr(w, http.StatusBadRequest, "role must be admin, editor or viewer")
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		a.fail(w, err)
		return
	}
	u, err := a.Store.CreateUser(r.Context(), in.Username, hash, in.Role)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "user.create", "user", ptr(u.ID), map[string]any{"username": u.Username, "role": u.Role})
	writeJSON(w, http.StatusCreated, u)
}

func (a *Admin) getUser(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	u, err := a.Store.GetUser(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// updateUser changes role and active flag. The last active admin can neither
// be demoted nor deactivated, and nobody can deactivate their own account.
func (a *Admin) updateUser(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	var in struct {
		Role     string `json:"role"`
		IsActive *bool  `json:"is_active"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	target, err := a.Store.GetUser(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	role, active := target.Role, target.IsActive
	if in.Role != "" {
		if !auth.IsValidRole(in.Role) {
			writeErr(w, http.StatusBadRequest, "role must be admin, editor or viewer")
			return
		}
		role = in.Role
	}
	if in.IsActive != nil {
		active = *in.IsActive
	}
	actor := auth.UserFrom(r.Context())
	if !active && actor.ID == target.ID {
		writeErr(w, http.StatusBadRequest, "you cannot deactivate your own account")
		return
	}
	losesAdmin := target.Role == string(auth.RoleAdmin) && target.IsActive && (role != string(auth.RoleAdmin) || !active)
	if losesAdmin {
		if ok, err := a.notLastAdmin(w, r); !ok || err != nil {
			return
		}
	}
	u, err := a.Store.UpdateUser(r.Context(), id, role, active)
	if err != nil {
		a.fail(w, err)
		return
	}
	if !active {
		// Sessions stop resolving via the is_active join anyway; drop them
		// so the table does not keep dead rows.
		_ = a.Store.DeleteUserSessions(r.Context(), id)
	}
	a.audit(r, "user.update", "user", ptr(u.ID), map[string]any{"username": u.Username, "role": u.Role, "is_active": u.IsActive})
	writeJSON(w, http.StatusOK, u)
}

// notLastAdmin writes a 409 and returns false when only one active admin is
// left. It is called before removing admin rights from an active admin.
func (a *Admin) notLastAdmin(w http.ResponseWriter, r *http.Request) (bool, error) {
	n, err := a.Store.CountActiveAdmins(r.Context())
	if err != nil {
		a.fail(w, err)
		return false, err
	}
	if n <= 1 {
		writeErr(w, http.StatusConflict, "cannot remove the last active admin")
		return false, nil
	}
	return true, nil
}

func (a *Admin) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	actor := auth.UserFrom(r.Context())
	if actor.ID == id {
		writeErr(w, http.StatusBadRequest, "you cannot delete your own account")
		return
	}
	target, err := a.Store.GetUser(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if target.Role == string(auth.RoleAdmin) && target.IsActive {
		if ok, err := a.notLastAdmin(w, r); !ok || err != nil {
			return
		}
	}
	if err := a.Store.DeleteUser(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "user.delete", "user", ptr(id), map[string]any{"username": target.Username})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) resetPassword(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	var in struct {
		New string `json:"new_password"`
	}
	if err := decode(r, &in); err != nil || len(in.New) < 8 {
		writeErr(w, http.StatusBadRequest, "new_password must be at least 8 characters")
		return
	}
	target, err := a.Store.GetUser(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	hash, err := auth.HashPassword(in.New)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.UpdateUserPassword(r.Context(), id, hash); err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.DeleteUserSessions(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "user.reset_password", "user", ptr(id), map[string]any{"username": target.Username})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) revokeSessions(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	target, err := a.Store.GetUser(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.DeleteUserSessions(r.Context(), id); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "user.revoke_sessions", "user", ptr(id), map[string]any{"username": target.Username})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- audit log ----

func (a *Admin) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AuditFilter{Action: strings.TrimSpace(q.Get("action"))}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if v := q.Get("actor_user_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad actor_user_id")
			return
		}
		f.ActorUserID = &id
	}
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before must be an RFC 3339 timestamp")
			return
		}
		f.Before = &t
	}
	list, err := a.Store.ListAuditLogs(r.Context(), f)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
