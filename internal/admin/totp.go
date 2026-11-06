package admin

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
)

// TOTP second factor for the dashboard login (ADR-037). The endpoints here
// verify a code through store.VerifySecondFactor and nowhere else, so the
// account has one lock however a guess arrives.

// Field limits, checked before any database or bcrypt work.
const (
	maxChallengeTokenLen = 128
	maxTOTPCodeLen       = 16
	maxRecoveryCodeLen   = 32
)

// mfaMethods are what the first login step offers.
var mfaMethods = []string{"totp", "recovery_code"}

// mfaProof is the code part of a request body.
type mfaProof struct {
	Code         string
	RecoveryCode string
}

// secondFactor turns the body's code fields into the gate's proof. It writes
// the 400 and returns false when there is not exactly one well-sized proof.
// A recovery code that is not shaped like one is still handed to the gate (its
// hash cannot match), so a typo costs an attempt like any wrong code.
func (p mfaProof) secondFactor(w http.ResponseWriter, f *store.SecondFactor) bool {
	code, recovery := strings.TrimSpace(p.Code), strings.TrimSpace(p.RecoveryCode)
	switch {
	case (code == "") == (recovery == ""):
		writeErr(w, http.StatusBadRequest, "send exactly one of code and recovery_code")
		return false
	case len(code) > maxTOTPCodeLen || len(recovery) > maxRecoveryCodeLen:
		writeErr(w, http.StatusBadRequest, "code is too long")
		return false
	case recovery != "":
		norm, ok := auth.NormalizeRecoveryCode(recovery)
		if !ok {
			norm = strings.ToUpper(recovery)
		}
		f.RecoveryHash = auth.HashRecoveryCode(norm)
	default:
		f.Code, f.Check = code, auth.CheckTOTP
	}
	return true
}

// mfaThrottle answers 429 for a gate that is busy or locked.
func mfaThrottle(w http.ResponseWriter, retryAfter time.Duration, locked bool) {
	code, msg := "mfa_busy", "another verification of this account is in progress, try again"
	if locked {
		code, msg = "mfa_locked", "too many wrong codes, try again later"
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"message": msg, "type": "rate_limited", "code": code, "locked": locked}})
}

// auditMFA writes the audit entry of a second-factor event: the user is the
// target, the details are the closed set the design allows (no username).
func (a *Admin) auditMFA(r *http.Request, actor *store.User, action string, userID int64, details map[string]any) {
	a.auditAs(r, actor, action, "user", ptr(userID), details)
}

// mfaOutcome maps the gate's refusal onto the response. actor is the signed-in
// user for the session endpoints and nil on login/mfa; wrong is the status of a
// wrong code (401 on login, 403 where the caller has a session). It always
// answers: the caller returns after it.
func (a *Admin) mfaOutcome(w http.ResponseWriter, r *http.Request, actor *store.User, userID int64,
	via string, wrong int, res store.SecondFactorResult, err error) {
	details := map[string]any{"via": via}
	var locked *store.MFALockedError
	switch {
	case errors.Is(err, store.ErrMFABusy):
		mfaThrottle(w, time.Second, false)
	case errors.As(err, &locked):
		a.auditMFALocked(r, actor, userID, via, locked.RetryAfter)
		mfaThrottle(w, locked.RetryAfter, true)
	case errors.Is(err, store.ErrMFAWrongCode):
		a.auditMFA(r, actor, "login.mfa_failure", userID, details)
		body := map[string]any{"message": "invalid code", "type": http.StatusText(wrong), "code": "mfa_invalid_code"}
		if via == store.ViaLogin {
			body["attempts_left"] = res.AttemptsLeft
		}
		writeJSON(w, wrong, map[string]any{"error": body})
	case errors.Is(err, store.ErrMFAInvalidChallenge):
		writeErrCode(w, http.StatusUnauthorized, "mfa_challenge_invalid", "the sign-in step expired or was already used; sign in again")
	case errors.Is(err, store.ErrTOTPAlreadyEnabled):
		writeErrCode(w, http.StatusConflict, "totp_already_enabled", "two-factor authentication is already enabled")
	case errors.Is(err, store.ErrTOTPNotSetUp):
		writeErrCode(w, http.StatusConflict, "totp_not_set_up", "call setup first")
	case errors.Is(err, store.ErrTOTPNotEnabled):
		writeErrCode(w, http.StatusConflict, "totp_not_enabled", "two-factor authentication is not enabled")
	default:
		a.fail(w, err)
	}
}

// auditMFALocked writes login.mfa_locked once per lock episode instead of once
// per refused request: a locked gate is cheap to knock on (no bcrypt behind a
// challenge), and every knock would otherwise add an audit row. An episode
// starts with the oldest failure still in the window, which RetryAfter gives.
func (a *Admin) auditMFALocked(r *http.Request, actor *store.User, userID int64, via string, retryAfter time.Duration) {
	since := time.Now().Add(retryAfter - store.MFALockWindow)
	if seen, err := a.Store.HasAuditSince(r.Context(), "login.mfa_locked", userID, since); err == nil && seen {
		return
	}
	a.auditMFA(r, actor, "login.mfa_locked", userID, map[string]any{"via": via})
}

// recoveryUsed audits a spent recovery code with how many are left.
func (a *Admin) recoveryUsed(r *http.Request, actor *store.User, userID int64, via string, res store.SecondFactorResult) {
	if res.UsedRecovery {
		a.auditMFA(r, actor, "login.recovery_code_used", userID, map[string]any{"via": via, "remaining": res.RecoveryRemaining})
	}
}

// ---- login, second step ----

// startMFA answers the first login step for an account with a second factor:
// a challenge instead of a session. The password was right. A locked account
// gets 429 here instead of a challenge it could not answer.
func (a *Admin) startMFA(w http.ResponseWriter, r *http.Request, u *store.User, bearer bool, ip string) {
	ctx := r.Context()
	locked, err := a.Store.MFALocked(ctx, u.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	if locked != nil {
		a.auditMFALocked(r, nil, u.ID, store.ViaLogin, locked.RetryAfter)
		mfaThrottle(w, locked.RetryAfter, true)
		return
	}
	tok, err := store.GenerateSessionToken()
	if err != nil {
		a.fail(w, err)
		return
	}
	ch, err := a.Store.CreateLoginChallenge(ctx, u.ID, tok, bearer, ip, store.LoginChallengeTTL)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mfa_required":    true,
		"challenge_token": tok,
		"expires_at":      ch.ExpiresAt.Format(time.RFC3339),
		"methods":         mfaMethods,
	})
}

// finishLogin issues the session and answers a login that is complete. With
// oneStep the account was seen without a second factor: the session is written
// only if that is still true, and otherwise the login turns into a challenge
// (TOTP was switched on a moment ago). Otherwise the second factor has been
// proved already.
func (a *Admin) finishLogin(w http.ResponseWriter, r *http.Request, u *store.User, bearer, oneStep bool) {
	ctx := r.Context()
	var tok string
	var err error
	if oneStep {
		var ok bool
		if tok, ok, err = a.Auth.NewSessionIfNoTOTP(ctx, u.ID); err == nil && !ok {
			a.startMFA(w, r, u, bearer, clientIP(r))
			return
		}
	} else {
		tok, err = a.Auth.NewSession(ctx, u.ID)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.TouchLastLogin(ctx, u.ID); err != nil {
		a.Log.Error("touch last login", "err", err)
	}
	a.auditAs(r, u, "login.success", "user", &u.ID, nil)
	a.Auth.SetCookie(w, r, tok)
	out := map[string]any{"user": u}
	if bearer {
		out["token"] = tok
	}
	writeJSON(w, http.StatusOK, out)
}

// loginMFA is POST /login/mfa: the challenge from the first step and a TOTP
// code or a recovery code. A right one spends the challenge and opens the
// session; the check, its lock and its counters are the gate's.
func (a *Admin) loginMFA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChallengeToken string `json:"challenge_token"`
		Code           string `json:"code"`
		RecoveryCode   string `json:"recovery_code"`
	}
	if err := decodeLogin(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if in.ChallengeToken == "" || len(in.ChallengeToken) > maxChallengeTokenLen {
		writeErr(w, http.StatusBadRequest, "challenge_token is required")
		return
	}
	req := store.SecondFactor{Via: store.ViaLogin, IP: clientIP(r)}
	if !(mfaProof{in.Code, in.RecoveryCode}).secondFactor(w, &req) {
		return
	}
	ctx := r.Context()
	ch, err := a.Store.GetLoginChallenge(ctx, in.ChallengeToken)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.mfaOutcome(w, r, nil, 0, store.ViaLogin, http.StatusUnauthorized, store.SecondFactorResult{}, store.ErrMFAInvalidChallenge)
			return
		}
		a.fail(w, err)
		return
	}
	req.UserID, req.ChallengeID = ch.UserID, ch.ID
	res, err := a.Store.VerifySecondFactor(ctx, req, nil)
	if err != nil {
		a.mfaOutcome(w, r, nil, ch.UserID, store.ViaLogin, http.StatusUnauthorized, res, err)
		return
	}
	u, err := a.Store.GetUser(ctx, ch.UserID)
	if err != nil {
		a.fail(w, err)
		return
	}
	// The challenge is spent and the code with it. A failure from here on costs
	// the user a code, not the account's safety.
	a.recoveryUsed(r, u, u.ID, store.ViaLogin, res)
	a.finishLogin(w, r, u, ch.Bearer, false)
}

// ---- password for the sensitive actions (T10) ----

// checkSessionPassword verifies the password a signed-in user types again for a
// sensitive action. A session may try SessionPasswordBudget wrong passwords: the
// try is counted before bcrypt runs, in one statement, so parallel requests
// cannot get past the limit between them; the session ends on the last wrong
// one. A right password gives the budget back. It answers, and returns false,
// when the password is not accepted.
func (a *Admin) checkSessionPassword(w http.ResponseWriter, r *http.Request, u *store.User, password, wrongMsg string) bool {
	ctx := r.Context()
	hash := store.HashToken(auth.TokenFromRequest(r))
	tries, ok, err := a.Store.ClaimSessionPasswordAttempt(ctx, hash)
	if err != nil {
		a.fail(w, err)
		return false
	}
	if !ok {
		writeErrCode(w, http.StatusUnauthorized, "password_attempts_exhausted",
			"too many wrong passwords on this session; sign in again")
		return false
	}
	if len(password) > maxLoginPasswordLen || !auth.CheckPassword(u.PasswordHash, password) {
		if tries >= store.SessionPasswordBudget {
			if err := a.Store.DeleteSessionByHash(ctx, hash); err != nil {
				a.Log.Error("end session after wrong passwords", "err", err)
			}
		}
		writeErrCode(w, http.StatusForbidden, "wrong_password", wrongMsg)
		return false
	}
	if err := a.Store.ResetSessionPasswordAttempts(ctx, hash); err != nil {
		a.Log.Error("reset session password attempts", "err", err)
	}
	return true
}

// ---- /me/totp ----

// totpBody is the body of the four session endpoints. Each reads only the
// fields it documents.
type totpBody struct {
	Password     string `json:"password"`
	Code         string `json:"code"`
	RecoveryCode string `json:"recovery_code"`
}

// totpSetup is POST /me/totp/setup: a new secret for an account that has not
// switched TOTP on. The password is asked for first, so a stolen session cannot
// plant its own secret; TOTP being on already is then a 409 and the secret in
// place stays.
func (a *Admin) totpSetup(w http.ResponseWriter, r *http.Request) {
	if !sessionOnly(w, r) {
		return
	}
	var in totpBody
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	ctx := r.Context()
	u := auth.UserFrom(ctx)
	if !a.checkSessionPassword(w, r, u, in.Password, "password is wrong") {
		return
	}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		a.fail(w, err)
		return
	}
	// Conditional in the statement, not in a read before it: with TOTP on, the
	// secret in place is never replaced, and the caller gets the 409.
	set, err := a.Store.SetTOTPSecret(ctx, u.ID, secret)
	if err != nil {
		a.fail(w, err)
		return
	}
	if !set {
		a.mfaOutcome(w, r, u, u.ID, "", 0, store.SecondFactorResult{}, store.ErrTOTPAlreadyEnabled)
		return
	}
	a.audit(r, "totp.setup", "user", ptr(u.ID), nil)
	writeJSON(w, http.StatusOK, map[string]any{"secret": secret, "otpauth_uri": auth.TOTPURI(u.Username, secret)})
}

// sessionGate runs the checks the three code-taking session endpoints share, in
// the order the design fixes: the state (409), the password (403, outside any
// transaction), then the gate. It returns the gate's result and true when the
// gate passed and onSuccess ran.
func (a *Admin) sessionGate(w http.ResponseWriter, r *http.Request, in totpBody, via string,
	stateOK func(store.TOTPStatus) error, onSuccess func(pgx.Tx) error) (store.SecondFactorResult, *store.User, bool) {
	ctx := r.Context()
	u := auth.UserFrom(ctx)
	st, err := a.Store.GetTOTPStatus(ctx, u.ID)
	if err != nil {
		a.fail(w, err)
		return store.SecondFactorResult{}, nil, false
	}
	if err := stateOK(st); err != nil {
		a.mfaOutcome(w, r, u, u.ID, via, http.StatusForbidden, store.SecondFactorResult{}, err)
		return store.SecondFactorResult{}, nil, false
	}
	if !a.checkSessionPassword(w, r, u, in.Password, "password is wrong") {
		return store.SecondFactorResult{}, nil, false
	}
	req := store.SecondFactor{UserID: u.ID, Via: via, IP: clientIP(r)}
	if !(mfaProof{in.Code, in.RecoveryCode}).secondFactor(w, &req) {
		return store.SecondFactorResult{}, nil, false
	}
	res, err := a.Store.VerifySecondFactor(ctx, req, onSuccess)
	if err != nil {
		a.mfaOutcome(w, r, u, u.ID, via, http.StatusForbidden, res, err)
		return res, nil, false
	}
	return res, u, true
}

// totpEnable is POST /me/totp/enable: the first code from the authenticator
// switches TOTP on, shows the ten recovery codes once and ends the account's
// other sessions, all in the gate's transaction.
func (a *Admin) totpEnable(w http.ResponseWriter, r *http.Request) {
	if !sessionOnly(w, r) {
		return
	}
	var in totpBody
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	in.RecoveryCode = "" // enable takes a code from the app, nothing else
	codes, hashes, err := auth.GenerateRecoveryCodes(auth.RecoveryCodeCount)
	if err != nil {
		a.fail(w, err)
		return
	}
	keep := store.HashToken(auth.TokenFromRequest(r))
	ctx := r.Context()
	userID := auth.UserFrom(ctx).ID
	_, u, ok := a.sessionGate(w, r, in, store.ViaEnable, func(st store.TOTPStatus) error {
		switch {
		case st.Enabled:
			return store.ErrTOTPAlreadyEnabled
		case !st.HasSecret:
			return store.ErrTOTPNotSetUp
		}
		return nil
	}, func(tx pgx.Tx) error {
		if err := store.EnableTOTPTx(ctx, tx, userID); err != nil {
			return err
		}
		if err := store.ReplaceRecoveryCodesTx(ctx, tx, userID, hashes); err != nil {
			return err
		}
		return store.DeleteUserSessionsExceptTx(ctx, tx, userID, keep)
	})
	if !ok {
		return
	}
	a.audit(r, "totp.enable", "user", ptr(u.ID), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recovery_codes": codes})
}

// totpDisable is POST /me/totp/disable: password and a code (or a recovery
// code) remove the secret and the recovery codes.
func (a *Admin) totpDisable(w http.ResponseWriter, r *http.Request) {
	if !sessionOnly(w, r) {
		return
	}
	var in totpBody
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	ctx := r.Context()
	userID := auth.UserFrom(ctx).ID
	res, u, ok := a.sessionGate(w, r, in, store.ViaDisable, notEnabledErr, func(tx pgx.Tx) error {
		return store.ClearTOTPTx(ctx, tx, userID)
	})
	if !ok {
		return
	}
	a.recoveryUsed(r, u, u.ID, store.ViaDisable, res)
	a.audit(r, "totp.disable", "user", ptr(u.ID), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// totpRecoveryCodes is POST /me/totp/recovery-codes: a new set of ten codes,
// shown once; the old ones, spent or not, stop working.
func (a *Admin) totpRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	if !sessionOnly(w, r) {
		return
	}
	var in totpBody
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	in.RecoveryCode = "" // a spent set is not renewed with one of its own codes
	codes, hashes, err := auth.GenerateRecoveryCodes(auth.RecoveryCodeCount)
	if err != nil {
		a.fail(w, err)
		return
	}
	ctx := r.Context()
	userID := auth.UserFrom(ctx).ID
	_, u, ok := a.sessionGate(w, r, in, store.ViaRecoveryCodes, notEnabledErr, func(tx pgx.Tx) error {
		return store.ReplaceRecoveryCodesTx(ctx, tx, userID, hashes)
	})
	if !ok {
		return
	}
	a.audit(r, "totp.recovery_regenerate", "user", ptr(u.ID), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recovery_codes": codes})
}

func notEnabledErr(st store.TOTPStatus) error {
	if !st.Enabled {
		return store.ErrTOTPNotEnabled
	}
	return nil
}

// ---- admin reset (T6) ----

// resetUserTOTP is POST /users/{id}/totp/reset: an admin removes another
// account's second factor, its failed-code rows and its sessions. Never one's
// own: that would walk around the password-and-code that disable asks for.
func (a *Admin) resetUserTOTP(w http.ResponseWriter, r *http.Request) {
	if !sessionOnly(w, r) {
		return
	}
	id, err := idParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid user id")
		return
	}
	ctx := r.Context()
	if auth.UserFrom(ctx).ID == id {
		writeErr(w, http.StatusBadRequest, "you cannot reset your own two-factor authentication; use disable")
		return
	}
	if _, err := a.Store.GetUser(ctx, id); err != nil {
		a.fail(w, err)
		return
	}
	had, err := a.Store.DisableUserTOTP(ctx, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.DeleteUserSessions(ctx, id); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "user.totp_reset", "user", ptr(id), map[string]any{"had_totp": had})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
