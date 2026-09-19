package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
)

// ---- users (admin only, except the lite listing) ----

// checkNewPassword returns the validation message for a password being set,
// or "" when it is acceptable: 8 characters at least, and at most bcrypt's
// 72-byte input limit so nothing is silently truncated.
func checkNewPassword(pw string) string {
	if len(pw) < 8 {
		return "password must be at least 8 characters"
	}
	if len(pw) > maxPasswordLen {
		return "password must be at most 72 bytes"
	}
	return ""
}

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

// errUsernameTaken answers a create that collides with an existing name;
// the comparison is case-insensitive.
const errUsernameTaken = "a user with that name already exists (usernames are case-insensitive)"

func (a *Admin) listUsers(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.ListUsersWithStats(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// createUser adds an account and, when project_ids is given, its project
// memberships in the same transaction.
func (a *Admin) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username   string  `json:"username"`
		Password   string  `json:"password"`
		Role       string  `json:"role"`
		ProjectIDs []int64 `json:"project_ids"`
	}
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" || len(in.Username) > 64 {
		writeErr(w, http.StatusBadRequest, "username is required (max 64 characters)")
		return
	}
	if msg := checkNewPassword(in.Password); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
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
	for _, id := range in.ProjectIDs {
		if id <= 0 {
			writeErr(w, http.StatusUnprocessableEntity, "project_ids contains an unknown project")
			return
		}
	}
	u, err := a.Store.CreateUserWithProjects(r.Context(), in.Username, hash, in.Role, in.ProjectIDs)
	if err != nil {
		switch {
		case store.IsUniqueViolation(err):
			writeErr(w, http.StatusConflict, errUsernameTaken)
		case store.IsForeignKeyViolation(err):
			writeErr(w, http.StatusUnprocessableEntity, "project_ids contains an unknown project")
		default:
			a.fail(w, err)
		}
		return
	}
	details := map[string]any{"username": u.Username, "role": u.Role}
	if len(in.ProjectIDs) > 0 {
		details["project_ids"] = in.ProjectIDs
	}
	a.audit(r, "user.create", "user", ptr(u.ID), details)
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
		badBody(w, err)
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
		if errors.Is(err, store.ErrKeyInUse) {
			writeErrCode(w, http.StatusConflict, "user_has_attributed_usage",
				"request logs still attribute spend to this account's api keys; deactivate it instead "+
					"so its usage history stays attributed")
			return
		}
		a.fail(w, err)
		return
	}
	a.audit(r, "user.delete", "user", ptr(id), map[string]any{"username": target.Username})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) resetPassword(w http.ResponseWriter, r *http.Request) {
	if !sessionOnly(w, r) {
		return
	}
	id, _ := idParam(r)
	var in struct {
		New string `json:"new_password"`
	}
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if msg := checkNewPassword(in.New); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
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
	// An administrator resetting someone else's password is a compromise or
	// an offboarding response, so the account's management keys go with its
	// sessions; they would otherwise keep acting on it.
	revoked, err := a.Store.RevokeUserManagementKeys(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "user.reset_password", "user", ptr(id),
		map[string]any{"username": target.Username, "revoked_management_keys": revoked})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked_management_keys": revoked})
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

// auditFilter parses the shared audit query parameters; it writes the 400
// and returns false when one of them is malformed.
func auditFilter(w http.ResponseWriter, r *http.Request) (store.AuditFilter, bool) {
	q := r.URL.Query()
	f := store.AuditFilter{Action: strings.TrimSpace(q.Get("action"))}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if v := q.Get("actor_user_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad actor_user_id")
			return f, false
		}
		f.ActorUserID = &id
	}
	for _, p := range []struct {
		name string
		dst  **time.Time
	}{{"before", &f.Before}, {"since", &f.Since}, {"until", &f.Until}} {
		v := q.Get(p.name)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, p.name+" must be an RFC 3339 timestamp")
			return f, false
		}
		*p.dst = &t
	}
	return f, true
}

// auditQuery is the filter as recorded in the audit.exported entry.
func auditQuery(f store.AuditFilter) map[string]any {
	d := map[string]any{}
	if f.Action != "" {
		d["action"] = f.Action
	}
	if f.ActorUserID != nil {
		d["actor_user_id"] = *f.ActorUserID
	}
	for _, p := range []struct {
		name string
		t    *time.Time
	}{{"before", f.Before}, {"since", f.Since}, {"until", f.Until}} {
		if p.t != nil {
			d[p.name] = p.t.UTC().Format(time.RFC3339)
		}
	}
	return d
}

func (a *Admin) listAudit(w http.ResponseWriter, r *http.Request) {
	f, ok := auditFilter(w, r)
	if !ok {
		return
	}
	entries, hasMore, err := a.Store.ListAuditLogs(r.Context(), f)
	if err != nil {
		a.fail(w, err)
		return
	}
	total, err := a.Store.CountAuditLogs(r.Context(), f)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "total": total, "has_more": hasMore})
}

// exportAudit streams the matching entries as NDJSON, newest first, at most
// store.MaxAuditExport rows. The export itself is audited before the first
// row is written so the entry exists even when the client disconnects.
func (a *Admin) exportAudit(w http.ResponseWriter, r *http.Request) {
	f, ok := auditFilter(w, r)
	if !ok {
		return
	}
	a.audit(r, "audit.exported", "audit", nil, auditQuery(f))
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="ragmux-audit-`+time.Now().UTC().Format("20060102T150405Z")+`.ndjson"`)
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	err := a.Store.StreamAuditLogs(r.Context(), f, func(l *store.AuditLog) error { return enc.Encode(l) })
	if err != nil {
		// Headers are out; the client sees a truncated body, the log the cause.
		a.Log.Error("audit export interrupted", "err", err, "req_id", w.Header().Get("X-Request-Id"))
	}
}
