package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/store"
)

// ---- user-owned api keys ----
//
// Everyone manages their own keys; admins see and manage everyone's. A key
// someone else owns answers 404 rather than 403, the same way a non-member
// project does, so key ids are not enumerable.

// maxKeyNameLen bounds the name a key is identified by in the dashboard.
const maxKeyNameLen = 64

type keyInput struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	// UserID mints the key for someone else; admin only. Nil means the caller.
	UserID           *int64  `json:"user_id"`
	ProjectIDs       []int64 `json:"project_ids"`
	DefaultProjectID *int64  `json:"default_project_id"`
	// ExpiresAt is an RFC 3339 timestamp; an empty string clears the expiry.
	ExpiresAt           *string `json:"expires_at"`
	RateLimitRPM        int     `json:"rate_limit_rpm"`
	RateLimitTPM        int     `json:"rate_limit_tpm"`
	BudgetDailyTokens   int64   `json:"budget_daily_tokens"`
	BudgetMonthlyTokens int64   `json:"budget_monthly_tokens"`
}

// expiry parses ExpiresAt. A nil pointer and an empty string both mean "no
// expiry", so a form that clears the field clears the column.
func (in *keyInput) expiry() (*time.Time, error) {
	if in.ExpiresAt == nil || strings.TrimSpace(*in.ExpiresAt) == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(*in.ExpiresAt))
	if err != nil {
		return nil, errors.New("expires_at must be an RFC 3339 timestamp")
	}
	return &t, nil
}

// loadKey fetches a key the caller may manage: their own, or anyone's for an
// admin. Everything else is a 404.
func (a *Admin) loadKey(w http.ResponseWriter, r *http.Request) (*store.APIKey, bool) {
	id, _ := idParam(r)
	k, err := a.Store.GetAPIKey(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return nil, false
	}
	u := auth.UserFrom(r.Context())
	if k.UserID != u.ID && !auth.Role(u.Role).AtLeast(auth.RoleAdmin) {
		writeErr(w, http.StatusNotFound, "not found")
		return nil, false
	}
	return k, true
}

// validateKey checks the shape of an input and, for gateway keys, that the
// caller may reach every project it grants.
//
// The grants are validated here, at creation time, and never again on /v1:
// a key keeps working when the owner's project membership is edited, which
// is what makes a production key safe to hand to a service.
func (a *Admin) validateKey(r *http.Request, in *keyInput, k *store.APIKey) error {
	k.Name = strings.TrimSpace(in.Name)
	if k.Name == "" || len(k.Name) > maxKeyNameLen {
		return errors.New("name is required (max 64 characters)")
	}
	exp, err := in.expiry()
	if err != nil {
		return err
	}
	k.ExpiresAt = exp
	actor := auth.UserFrom(r.Context())
	scopes, err := clipScopes(in.Scopes, k.Kind, auth.Role(actor.Role))
	if err != nil {
		return err
	}
	k.Scopes = scopes
	if k.Kind == store.KindManagement {
		// The schema refuses all three of these for a management key -- a
		// default project, limits of its own (0010) and grant rows (0014) --
		// so this check is what turns a constraint violation into a message
		// naming the field the caller sent, not what holds the invariant up.
		if len(in.ProjectIDs) > 0 || in.DefaultProjectID != nil {
			return errors.New("a management key has no projects")
		}
		if in.RateLimitRPM != 0 || in.RateLimitTPM != 0 || in.BudgetDailyTokens != 0 || in.BudgetMonthlyTokens != 0 {
			return errors.New("a management key has no rate limits or budgets")
		}
		k.ProjectIDs, k.DefaultProjectID = nil, nil
		k.RateLimitRPM, k.RateLimitTPM, k.BudgetDailyTokens, k.BudgetMonthlyTokens = 0, 0, 0, 0
		return nil
	}
	if in.RateLimitRPM < 0 || in.RateLimitTPM < 0 || in.BudgetDailyTokens < 0 || in.BudgetMonthlyTokens < 0 {
		return errors.New("rate limits and budgets must be 0 (unlimited) or positive")
	}
	k.RateLimitRPM, k.RateLimitTPM = in.RateLimitRPM, in.RateLimitTPM
	k.BudgetDailyTokens, k.BudgetMonthlyTokens = in.BudgetDailyTokens, in.BudgetMonthlyTokens
	// The key's own reach is bounded by the owner, not by the caller: an
	// admin minting a key for someone grants only what that account can see.
	owner := actor
	if k.UserID != actor.ID {
		var err error
		if owner, err = a.Store.GetUser(r.Context(), k.UserID); err != nil {
			return errors.New("user_id does not reference an existing user")
		}
	}
	for _, pid := range in.ProjectIDs {
		ok, err := auth.CanAccessProject(r.Context(), a.Store, owner, pid)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("project_ids contains a project " + owner.Username + " cannot access")
		}
	}
	k.ProjectIDs = in.ProjectIDs
	if in.DefaultProjectID != nil && *in.DefaultProjectID != 0 {
		if !containsID(in.ProjectIDs, *in.DefaultProjectID) {
			return errors.New("default_project_id must be one of project_ids")
		}
		k.DefaultProjectID = in.DefaultProjectID
	} else {
		k.DefaultProjectID = nil
	}
	return nil
}

// clipScopes validates the requested scopes for a kind and refuses any that
// the caller's own role does not cover, so a key is never a privilege
// escalation over the account that issued it.
func clipScopes(want []string, kind string, role auth.Role) ([]string, error) {
	if len(want) == 0 {
		if kind == store.KindGateway {
			return store.DefaultGatewayScopes, nil
		}
		return nil, errors.New("a management key needs at least one scope")
	}
	valid := store.ValidScopes(kind)
	out := make([]string, 0, len(want))
	for _, s := range want {
		if !containsString(valid, s) {
			return nil, errors.New("unknown scope " + strconv.Quote(s) + " for a " + kind + " key")
		}
		switch {
		case s == store.ScopeAdmin && !role.AtLeast(auth.RoleAdmin):
			return nil, errors.New(`the "admin" scope requires the admin role`)
		case (s == store.ScopeWrite || s == store.ScopeKeys) && !role.AtLeast(auth.RoleEditor) && kind == store.KindManagement:
			return nil, errors.New(`the "` + s + `" scope requires at least the editor role`)
		}
		if !containsString(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// keyFilter reads the listing query. Non-admins always see only their own
// keys, whatever ?user_id says.
func (a *Admin) keyFilter(w http.ResponseWriter, r *http.Request) (store.APIKeyFilter, bool) {
	u := auth.UserFrom(r.Context())
	f := store.APIKeyFilter{UserID: &u.ID, Kind: r.URL.Query().Get("kind")}
	if f.Kind != "" && f.Kind != store.KindGateway && f.Kind != store.KindManagement {
		writeErr(w, http.StatusBadRequest, `kind must be "gateway" or "management"`)
		return f, false
	}
	if !auth.Role(u.Role).AtLeast(auth.RoleAdmin) {
		return f, true
	}
	switch v := r.URL.Query().Get("user_id"); v {
	case "":
		f.UserID = nil // admins see every owner unless they narrow it
	default:
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad user_id")
			return f, false
		}
		f.UserID = &id
	}
	return f, true
}

func (a *Admin) listKeys(w http.ResponseWriter, r *http.Request) {
	f, ok := a.keyFilter(w, r)
	if !ok {
		return
	}
	list, err := a.Store.ListAPIKeys(r.Context(), f)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// createKey issues a key and returns the credential once. The response is the
// only place the plaintext ever exists; nothing stores or logs it.
func (a *Admin) createKey(w http.ResponseWriter, r *http.Request) {
	var in keyInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	actor := auth.UserFrom(r.Context())
	k := &store.APIKey{Kind: in.Kind, UserID: actor.ID, CreatedBy: ptr(actor.ID)}
	if k.Kind == "" {
		k.Kind = store.KindGateway
	}
	if k.Kind != store.KindGateway && k.Kind != store.KindManagement {
		writeErr(w, http.StatusBadRequest, `kind must be "gateway" or "management"`)
		return
	}
	if in.UserID != nil && *in.UserID != actor.ID {
		if !auth.Role(actor.Role).AtLeast(auth.RoleAdmin) {
			writeErr(w, http.StatusForbidden, "only an admin can create a key for another user")
			return
		}
		k.UserID = *in.UserID
	}
	if k.Kind == store.KindManagement {
		// A management key can act on the account it belongs to, so minting
		// one is exactly the step a leaked key must not be able to take.
		if !sessionOnly(w, r) {
			return
		}
		if !auth.Role(actor.Role).AtLeast(auth.RoleEditor) {
			writeErr(w, http.StatusForbidden, "a management key requires at least the editor role")
			return
		}
	}
	if err := a.validateKey(r, &in, k); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, secret, err := a.Store.CreateAPIKey(r.Context(), k)
	if err != nil {
		if store.IsForeignKeyViolation(err) {
			writeErr(w, http.StatusBadRequest, "project_ids or user_id contains an unknown id")
			return
		}
		a.fail(w, err)
		return
	}
	a.audit(r, "apikey.create", "api_key", ptr(out.ID), keyDetails(out))
	writeJSON(w, http.StatusCreated, map[string]any{"key": out, "api_key": secret})
}

func (a *Admin) getKey(w http.ResponseWriter, r *http.Request) {
	k, ok := a.loadKey(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// updateKey changes the mutable fields; the credential itself never changes,
// a key is revoked and reissued instead.
func (a *Admin) updateKey(w http.ResponseWriter, r *http.Request) {
	k, ok := a.loadKey(w, r)
	if !ok {
		return
	}
	if k.Kind == store.KindManagement {
		// Minting a management key needs a session so a leaked one cannot
		// extend its own reach. Editing one is the same step by another
		// name: scopes and expiry are writable here, so without this the
		// guard on create only costs an attacker one extra request.
		if !sessionOnly(w, r) {
			return
		}
	}
	var in keyInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if in.Kind != "" && in.Kind != k.Kind {
		writeErr(w, http.StatusBadRequest, "a key's kind cannot be changed")
		return
	}
	if err := a.validateKey(r, &in, k); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := a.Store.UpdateAPIKey(r.Context(), k)
	if err != nil {
		if store.IsForeignKeyViolation(err) {
			writeErr(w, http.StatusBadRequest, "project_ids contains an unknown project")
			return
		}
		a.fail(w, err)
		return
	}
	a.audit(r, "apikey.update", "api_key", ptr(out.ID), keyDetails(out))
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) revokeKey(w http.ResponseWriter, r *http.Request) {
	k, ok := a.loadKey(w, r)
	if !ok {
		return
	}
	out, err := a.Store.RevokeAPIKey(r.Context(), k.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "apikey.revoke", "api_key", ptr(out.ID), map[string]any{"name": out.Name, "kind": out.Kind})
	writeJSON(w, http.StatusOK, out)
}

// deleteKey removes a key outright. Revoking is almost always the right
// action, so this is admin-only and refuses while request logs still
// attribute spend to the key.
func (a *Admin) deleteKey(w http.ResponseWriter, r *http.Request) {
	k, ok := a.loadKey(w, r)
	if !ok {
		return
	}
	if err := a.Store.DeleteAPIKey(r.Context(), k.ID); err != nil {
		if errors.Is(err, store.ErrKeyInUse) {
			writeErrCode(w, http.StatusConflict, "key_in_use",
				"request logs still reference this key; revoke it instead so its usage history stays attributed")
			return
		}
		a.fail(w, err)
		return
	}
	a.audit(r, "apikey.delete", "api_key", ptr(k.ID), map[string]any{"name": k.Name, "kind": k.Kind})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *Admin) keyUsage(w http.ResponseWriter, r *http.Request) {
	k, ok := a.loadKey(w, r)
	if !ok {
		return
	}
	l := a.Usage
	if l == nil {
		l = &limits.Limiter{Store: a.Store}
	}
	u, err := l.KeyUsage(r.Context(), k)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// keyDetails is what an audit entry records about a key. The credential and
// its hash are never part of it.
func keyDetails(k *store.APIKey) map[string]any {
	d := map[string]any{"name": k.Name, "kind": k.Kind, "user_id": k.UserID,
		"scopes": k.Scopes, "key_prefix": k.KeyPrefix}
	if len(k.ProjectIDs) > 0 {
		d["project_ids"] = k.ProjectIDs
	}
	if k.ExpiresAt != nil {
		d["expires_at"] = k.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if k.RateLimitRPM != 0 || k.RateLimitTPM != 0 || k.BudgetDailyTokens != 0 || k.BudgetMonthlyTokens != 0 {
		d["limits"] = map[string]int64{"rate_limit_rpm": int64(k.RateLimitRPM), "rate_limit_tpm": int64(k.RateLimitTPM),
			"budget_daily_tokens": k.BudgetDailyTokens, "budget_monthly_tokens": k.BudgetMonthlyTokens}
	}
	return d
}
