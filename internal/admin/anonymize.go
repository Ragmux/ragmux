package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
)

// KVKK anonymisation (ADR-040). Both endpoints are admin-only and
// session-only: a management key, even one with the admin scope, gets
// 403 session_required, because the operation cannot be undone and a leaked
// key must not be able to do it.

// anonymizeUser makes one account's personal fields irreversible. The body is
// {"confirm": "<the account's current username>"}.
func (a *Admin) anonymizeUser(w http.ResponseWriter, r *http.Request) {
	if !sessionOnly(w, r) {
		return
	}
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	// Like deactivating or deleting one's own account, which are refused too.
	if actor := auth.UserFrom(r.Context()); actor != nil && actor.ID == id {
		writeErr(w, http.StatusBadRequest, "you cannot anonymize your own account")
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	// The password becomes a bcrypt hash of a secret nobody ever sees, so no
	// login or reset can ever match it.
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		a.fail(w, err)
		return
	}
	hash, err := auth.HashPassword(hex.EncodeToString(secret[:]))
	if err != nil {
		a.fail(w, err)
		return
	}
	res, err := a.Store.AnonymizeUser(r.Context(), id, in.Confirm, hash)
	switch {
	case errors.Is(err, store.ErrConfirmMismatch):
		writeErr(w, http.StatusBadRequest, "confirm must be the account's current username, exactly as stored")
		return
	case anonymizedRefusal(w, err):
		return
	case err != nil:
		a.fail(w, err)
		return
	}
	a.audit(r, "user.anonymize", "user", ptr(id), map[string]any{
		"username": res.Pseudonym, "revoked_keys": res.RevokedKeys})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pseudonym": res.Pseudonym, "revoked_keys": res.RevokedKeys})
}

// anonymizeEndUser replaces an end user's identifier with a pseudonym.
// project_id limits it to one project; without it every project is covered.
func (a *Admin) anonymizeEndUser(w http.ResponseWriter, r *http.Request) {
	if !sessionOnly(w, r) {
		return
	}
	var in struct {
		EndUser   string `json:"end_user"`
		ProjectID *int64 `json:"project_id"`
	}
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if in.EndUser == "" || len(in.EndUser) > store.MaxEndUserBytes {
		writeErr(w, http.StatusBadRequest, "end_user is required (at most 128 bytes)")
		return
	}
	// A long rewrite must not stop halfway because the client went away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Minute)
	defer cancel()
	res, err := a.Store.AnonymizeEndUser(ctx, in.EndUser, in.ProjectID)
	if err != nil {
		a.fail(w, err)
		return
	}
	// The original identifier is not written anywhere.
	details := map[string]any{
		"pseudonym": res.Pseudonym, "project_id": in.ProjectID, "request_logs": res.RequestLogs,
		"end_user_usage": res.EndUserUsage, "alert_events": res.AlertEvents, "webhook_deliveries": res.WebhookDeliveries,
	}
	a.audit(r, "end_user.anonymize", "end_user", nil, details)
	writeJSON(w, http.StatusOK, res)
}

// anonymizedRefusal answers 409 already_anonymized when err is the store's
// refusal to touch an anonymized account, and reports whether it did.
func anonymizedRefusal(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, store.ErrAlreadyAnonymized) {
		return false
	}
	writeErrCode(w, http.StatusConflict, "already_anonymized", "this account has been anonymized and stays closed")
	return true
}
