package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
)

// HTTP-level tests of the TOTP second factor (ADR-037). They drive the real
// router with real sessions, and the real login limiter, because the lock lives
// where the password counters, the challenge and the session endpoints meet.

const totpTestPassword = "correct-horse-battery"

type totpEnv struct{ *adminEnv }

func newTOTPEnv(t *testing.T) *totpEnv {
	t.Helper()
	e := newAdminEnv(t)
	e.adm.Limiter = auth.DefaultLoginLimiter(e.st)
	return &totpEnv{e}
}

// call is adminEnv.do with the response headers, and no Authorization header
// when token is empty.
func (e *totpEnv) call(method, path string, body any, token string) (int, map[string]any, http.Header) {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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
	return resp.StatusCode, out, resp.Header
}

// account creates a user whose password is totpTestPassword, with a session.
func (e *totpEnv) account(name, role string) *actor {
	e.t.Helper()
	a := e.user(name, role)
	h, err := auth.HashPassword(totpTestPassword)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.UpdateUserPassword(context.Background(), a.ID, h); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *totpEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.st.DB().Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *totpEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.DB().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *totpEnv) mfaRows(username string) int {
	return e.count("SELECT COUNT(*) FROM login_attempts WHERE stage = 'mfa' AND NOT success AND username = $1", username)
}

// code is what the authenticator shows now. Every success moves the account's
// last accepted step to the current one, so a test that needs another success
// within the same 30 seconds calls forget first.
func (e *totpEnv) code(secret string) string {
	e.t.Helper()
	c, err := auth.TOTPCodeAt(secret, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *totpEnv) forget(u *actor) {
	e.exec("UPDATE users SET totp_last_step = 0 WHERE id = $1", u.ID)
}

// enabled sets TOTP up for u through the endpoints and returns the secret and
// the recovery codes. u's own session survives; its others are ended.
func (e *totpEnv) enabled(u *actor) (secret string, recovery []string) {
	e.t.Helper()
	st, out, _ := e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": totpTestPassword}, u.token)
	if st != 200 {
		e.t.Fatalf("setup: %d %v", st, out)
	}
	secret, _ = out["secret"].(string)
	st, out, _ = e.call(http.MethodPost, "/api/me/totp/enable",
		map[string]any{"password": totpTestPassword, "code": e.code(secret)}, u.token)
	if st != 200 {
		e.t.Fatalf("enable: %d %v", st, out)
	}
	for _, c := range out["recovery_codes"].([]any) {
		recovery = append(recovery, c.(string))
	}
	e.forget(u)
	return secret, recovery
}

// login runs the first step with the right password and returns the answer.
func (e *totpEnv) login(name string, bearer bool) (int, map[string]any, http.Header) {
	e.t.Helper()
	return e.call(http.MethodPost, "/api/login", map[string]any{"username": name, "password": totpTestPassword, "bearer": bearer}, "")
}

// challenge opens a challenge and returns its token.
func (e *totpEnv) challenge(name string) string {
	e.t.Helper()
	st, out, _ := e.login(name, false)
	tok, _ := out["challenge_token"].(string)
	if st != 200 || tok == "" {
		e.t.Fatalf("login of %s: %d %v", name, st, out)
	}
	return tok
}

func (e *totpEnv) mfa(token string, proof map[string]any) (int, map[string]any, http.Header) {
	e.t.Helper()
	body := map[string]any{"challenge_token": token}
	for k, v := range proof {
		body[k] = v
	}
	return e.call(http.MethodPost, "/api/login/mfa", body, "")
}

func code(out map[string]any) string {
	s, _ := errCode(out).(string)
	return s
}

func TestTOTPSetupEnableAndTwoStepLogin(t *testing.T) {
	e := newTOTPEnv(t)
	ali := e.account("ali", "viewer")
	second, err := e.adm.Auth.NewSession(context.Background(), ali.ID)
	if err != nil {
		t.Fatal(err)
	}

	// setup: the password is asked for.
	for _, pw := range []string{"", "wrong-password-1"} {
		if st, out, _ := e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": pw}, ali.token); st != http.StatusForbidden || code(out) != "wrong_password" {
			t.Errorf("setup with password %q: %d %v", pw, st, out)
		}
	}
	st, out, _ := e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": totpTestPassword}, ali.token)
	secret, _ := out["secret"].(string)
	uri, _ := out["otpauth_uri"].(string)
	if st != 200 || len(secret) != 32 || uri != "otpauth://totp/Ragmux:ali?secret="+secret+"&issuer=Ragmux" {
		t.Fatalf("setup: %d %v", st, out)
	}
	// Setup again before enabling renews the pending secret.
	st, out, _ = e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": totpTestPassword}, ali.token)
	if again, _ := out["secret"].(string); st != 200 || again == secret {
		t.Fatalf("a second setup: %d, same secret = %v", st, again == secret)
	}
	secret, _ = out["secret"].(string)

	// The sealed secret is not in the row in the clear.
	var raw []byte
	if err := e.st.DB().QueryRow(context.Background(), "SELECT totp_secret_enc FROM users WHERE id = $1", ali.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || bytes.Contains(raw, []byte(secret)) {
		t.Errorf("totp_secret_enc: %d bytes, plaintext inside: %v", len(raw), bytes.Contains(raw, []byte(secret)))
	}

	// An abandoned setup is not a second factor: the login is still one step.
	if st, out, hdr := e.login("ali", false); st != 200 || out["user"] == nil || out["mfa_required"] != nil || !strings.Contains(hdr.Get("Set-Cookie"), "ragmux_session") {
		t.Fatalf("login with a pending secret: %d %v", st, out)
	}

	// enable: password first, then the code.
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/enable", map[string]any{"code": e.code(secret)}, ali.token); st != http.StatusForbidden || code(out) != "wrong_password" {
		t.Errorf("enable without the password: %d %v", st, out)
	}
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/enable", map[string]any{"password": totpTestPassword, "code": "000000"}, ali.token); st != http.StatusForbidden || code(out) != "mfa_invalid_code" {
		t.Errorf("enable with a wrong code: %d %v", st, out)
	}
	if n := e.count("SELECT COUNT(*) FROM users WHERE totp_enabled_at IS NOT NULL"); n != 0 {
		t.Fatalf("a wrong code switched TOTP on")
	}
	// The very code enable takes is kept: regenerating it later could give the
	// next step's code when the clock crosses a 30 second boundary in between,
	// and that one is valid.
	enableCode := e.code(secret)
	st, out, _ = e.call(http.MethodPost, "/api/me/totp/enable", map[string]any{"password": totpTestPassword, "code": enableCode}, ali.token)
	codes, _ := out["recovery_codes"].([]any)
	if st != 200 || len(codes) != 10 {
		t.Fatalf("enable: %d %v", st, out)
	}
	seen := map[string]bool{}
	for _, c := range codes {
		s := c.(string)
		if len(s) != 11 || s[5] != '-' || seen[s] {
			t.Errorf("recovery code %q", s)
		}
		seen[s] = true
		if n := e.count("SELECT COUNT(*) FROM user_recovery_codes WHERE code_hash = $1", s); n != 0 {
			t.Errorf("recovery code %q is stored in the clear", s)
		}
	}
	if n := e.count("SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = $1 AND used_at IS NULL AND length(code_hash) = 64", ali.ID); n != 10 {
		t.Errorf("%d hashed codes stored, want 10", n)
	}
	// The other sessions of the account ended; this one did not.
	if st, _, _ := e.call(http.MethodGet, "/api/me", nil, second); st != http.StatusUnauthorized {
		t.Errorf("the other session survived enable: %d", st)
	}
	if st, _, _ := e.call(http.MethodGet, "/api/me", nil, ali.token); st != 200 {
		t.Errorf("the session that enabled TOTP ended: %d", st)
	}
	// Enabled: setup is a 409 and the secret in place stays.
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": totpTestPassword}, ali.token); st != http.StatusConflict || code(out) != "totp_already_enabled" {
		t.Errorf("setup when enabled: %d %v", st, out)
	}
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/enable", map[string]any{"password": totpTestPassword, "code": e.code(secret)}, ali.token); st != http.StatusConflict || code(out) != "totp_already_enabled" {
		t.Errorf("enable when enabled: %d %v", st, out)
	}
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": "wrong-password-1"}, ali.token); st != http.StatusForbidden {
		t.Errorf("setup when enabled without the password: %d %v", st, out)
	}

	// Login is now two steps: a challenge, no session, no cookie, no token.
	st, out, hdr := e.login("ali", true)
	tok, _ := out["challenge_token"].(string)
	if st != 200 || out["mfa_required"] != true || tok == "" || out["token"] != nil || out["user"] != nil || hdr.Get("Set-Cookie") != "" {
		t.Fatalf("login with TOTP on: %d %v cookie=%q", st, out, hdr.Get("Set-Cookie"))
	}
	if m, _ := out["methods"].([]any); len(m) != 2 || m[0] != "totp" || m[1] != "recovery_code" {
		t.Errorf("methods = %v", out["methods"])
	}
	if exp, err := time.Parse(time.RFC3339, out["expires_at"].(string)); err != nil || time.Until(exp) < 4*time.Minute || time.Until(exp) > 6*time.Minute {
		t.Errorf("expires_at = %v (%v)", out["expires_at"], err)
	}
	// The code that enabled TOTP was spent; a wrong one and a malformed body fail.
	if st, out, _ := e.mfa(tok, map[string]any{"code": enableCode}); st != http.StatusUnauthorized || code(out) != "mfa_invalid_code" {
		t.Errorf("the code enable spent was taken again: %d %v", st, out)
	}
	if st, _, _ := e.mfa(tok, map[string]any{}); st != http.StatusBadRequest {
		t.Errorf("no code at all: %d", st)
	}
	if st, _, _ := e.mfa(tok, map[string]any{"code": "123456", "recovery_code": "abcde-fghjk"}); st != http.StatusBadRequest {
		t.Errorf("two proofs: %d", st)
	}
	if st, _, _ := e.mfa("", map[string]any{"code": "123456"}); st != http.StatusBadRequest {
		t.Errorf("no challenge token: %d", st)
	}
	if st, out, _ := e.mfa("not-a-challenge", map[string]any{"code": "123456"}); st != http.StatusUnauthorized || code(out) != "mfa_challenge_invalid" {
		t.Errorf("an unknown challenge: %d %v", st, out)
	}
	// The next step's code is fine, and the dashboard path sets the cookie.
	next, _ := auth.TOTPCodeAt(secret, time.Now().Add(30*time.Second))
	st, out, hdr = e.mfa(tok, map[string]any{"code": next})
	if st != 200 || out["user"] == nil || out["token"] == nil || !strings.Contains(hdr.Get("Set-Cookie"), "ragmux_session") {
		t.Fatalf("login/mfa: %d %v (bearer was asked for in the first step)", st, out)
	}
	if st, _, _ := e.call(http.MethodGet, "/api/me", nil, out["token"].(string)); st != 200 {
		t.Errorf("the token of login/mfa does not open a session: %d", st)
	}
	// Single use, and the code only once.
	if st, out, _ := e.mfa(tok, map[string]any{"code": e.code(secret)}); st != http.StatusUnauthorized || code(out) != "mfa_challenge_invalid" {
		t.Errorf("a spent challenge answered again: %d %v", st, out)
	}
	tok2 := e.challenge("ali")
	if st, _, _ := e.mfa(tok2, map[string]any{"code": next}); st != http.StatusUnauthorized {
		t.Errorf("the same code twice: %d", st)
	}
	// Without bearer the token stays out of the body.
	e.forget(ali)
	st, out, _ = e.mfa(tok2, map[string]any{"code": e.code(secret)})
	if st != 200 || out["token"] != nil || out["user"] == nil {
		t.Errorf("a dashboard login/mfa: %d %v", st, out)
	}
	// Last login moved, and the audit trail carries login.success for it.
	if n := e.count("SELECT COUNT(*) FROM audit_logs WHERE action = 'login.success' AND target_id = $1", ali.ID); n != 3 {
		t.Errorf("login.success entries = %d, want 3 (the one-step login and two mfa logins)", n)
	}
}

func TestTOTPChallengeExpiresAndClosesOnTheFifthWrongCode(t *testing.T) {
	e := newTOTPEnv(t)
	ali := e.account("ali", "viewer")
	secret, _ := e.enabled(ali)

	tok := e.challenge("ali")
	e.exec("UPDATE login_challenges SET expires_at = now() - interval '1 second'")
	if st, out, _ := e.mfa(tok, map[string]any{"code": e.code(secret)}); st != http.StatusUnauthorized || code(out) != "mfa_challenge_invalid" {
		t.Errorf("an expired challenge: %d %v", st, out)
	}

	tok = e.challenge("ali")
	for i := 1; i <= 5; i++ {
		st, out, _ := e.mfa(tok, map[string]any{"code": "000000"})
		if st != http.StatusUnauthorized || code(out) != "mfa_invalid_code" {
			t.Fatalf("wrong code %d: %d %v", i, st, out)
		}
		if left := out["error"].(map[string]any)["attempts_left"]; left != float64(5-i) {
			t.Errorf("attempts_left after %d = %v", i, left)
		}
	}
	if n := e.count("SELECT COUNT(*) FROM login_challenges WHERE token_hash = $1", store.HashToken(tok)); n != 0 {
		t.Errorf("the challenge survived its fifth wrong code")
	}
	if st, out, _ := e.mfa(tok, map[string]any{"code": e.code(secret)}); st != http.StatusUnauthorized || code(out) != "mfa_challenge_invalid" {
		t.Errorf("the closed challenge answered a right code: %d %v", st, out)
	}
}

func TestTOTPRecoveryCodeLogin(t *testing.T) {
	e := newTOTPEnv(t)
	ali := e.account("ali", "viewer")
	_, recovery := e.enabled(ali)

	tok := e.challenge("ali")
	// Typed the way people do: lower case, no dash.
	typed := strings.ToLower(strings.ReplaceAll(recovery[0], "-", ""))
	if st, out, _ := e.mfa(tok, map[string]any{"recovery_code": typed}); st != 200 {
		t.Fatalf("recovery code login: %d %v", st, out)
	}
	if st, out, _ := e.mfa(e.challenge("ali"), map[string]any{"recovery_code": recovery[0]}); st != http.StatusUnauthorized || code(out) != "mfa_invalid_code" {
		t.Errorf("a recovery code twice: %d %v", st, out)
	}
	if st, out, _ := e.mfa(e.challenge("ali"), map[string]any{"recovery_code": "!!not a code!!"}); st != http.StatusUnauthorized || code(out) != "mfa_invalid_code" {
		t.Errorf("a malformed recovery code: %d %v", st, out)
	}
	var details string
	if err := e.st.DB().QueryRow(context.Background(),
		"SELECT details::text FROM audit_logs WHERE action = 'login.recovery_code_used'").Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"remaining": 9`) || !strings.Contains(details, `"via": "login"`) {
		t.Errorf("login.recovery_code_used details = %s", details)
	}
	// A bearer login keeps its preference through the second step.
	st, out, _ := e.mfa(func() string {
		_, o, _ := e.login("ali", true)
		return o["challenge_token"].(string)
	}(), map[string]any{"recovery_code": recovery[1]})
	if st != 200 || out["token"] == nil {
		t.Errorf("bearer login with a recovery code: %d %v", st, out)
	}
}

// T5, sequential: every door counts into one counter, a locked account refuses
// a right code on every door and the first login step, the password stage
// stays separate, and the account's casing makes no difference.
func TestTOTPLockSpansEveryDoor(t *testing.T) {
	e := newTOTPEnv(t)
	ali := e.account("Ali", "viewer")
	secret, recovery := e.enabled(ali)
	with := func(extra map[string]any) map[string]any {
		b := map[string]any{"password": totpTestPassword}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}

	// A challenge opened before the lock, typed as "ALI": it must not help once locked.
	held := e.challenge2(t, "ALI")

	// Two wrong codes at login/mfa (typed as "aLI"), one at recovery-codes, two at disable.
	for i := 0; i < 2; i++ {
		_, out, _ := e.call(http.MethodPost, "/api/login", map[string]any{"username": "aLI", "password": totpTestPassword}, "")
		if st, out, _ := e.mfa(out["challenge_token"].(string), map[string]any{"code": "000000"}); st != http.StatusUnauthorized {
			t.Fatalf("wrong code at login/mfa: %d %v", st, out)
		}
	}
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/recovery-codes", with(map[string]any{"code": "000000"}), ali.token); st != http.StatusForbidden || code(out) != "mfa_invalid_code" {
		t.Fatalf("wrong code at recovery-codes: %d %v", st, out)
	}
	for i := 0; i < 2; i++ {
		if st, out, _ := e.call(http.MethodPost, "/api/me/totp/disable", with(map[string]any{"code": "000000"}), ali.token); st != http.StatusForbidden || code(out) != "mfa_invalid_code" {
			t.Fatalf("wrong code at disable: %d %v", st, out)
		}
	}
	if n := e.mfaRows("ali"); n != 5 {
		t.Fatalf("mfa rows for 'ali' = %d, want 5 from the four doors", n)
	}
	if n := e.count("SELECT COUNT(*) FROM login_attempts WHERE username = 'Ali'"); n != 0 {
		t.Errorf("%d rows kept the stored casing", n)
	}

	// A wrong password at a session door does not count.
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/disable", map[string]any{"password": "wrong-password-1", "code": e.code(secret)}, ali.token); st != http.StatusForbidden || code(out) != "wrong_password" {
		t.Errorf("wrong password at disable: %d %v", st, out)
	}
	if n := e.mfaRows("ali"); n != 5 {
		t.Errorf("a wrong password changed the mfa count to %d", n)
	}

	// Locked: the right code and a right recovery code are 429 everywhere.
	tok := held
	for name, c := range map[string]struct {
		path string
		body map[string]any
		tok  string
	}{
		"login/mfa code":     {"/api/login/mfa", map[string]any{"challenge_token": tok, "code": e.code(secret)}, ""},
		"login/mfa recovery": {"/api/login/mfa", map[string]any{"challenge_token": tok, "recovery_code": recovery[0]}, ""},
		"recovery-codes":     {"/api/me/totp/recovery-codes", with(map[string]any{"code": e.code(secret)}), ali.token},
		"disable code":       {"/api/me/totp/disable", with(map[string]any{"code": e.code(secret)}), ali.token},
		"disable recovery":   {"/api/me/totp/disable", with(map[string]any{"recovery_code": recovery[1]}), ali.token},
	} {
		st, out, hdr := e.call(http.MethodPost, c.path, c.body, c.tok)
		if st != http.StatusTooManyRequests || code(out) != "mfa_locked" || hdr.Get("Retry-After") == "" {
			t.Errorf("%s while locked: %d %v retry-after=%q", name, st, out, hdr.Get("Retry-After"))
		}
	}
	if n := e.mfaRows("ali"); n != 5 {
		t.Errorf("refused attempts wrote rows: %d", n)
	}

	// The first login step: a right password gets 429, no challenge; a wrong
	// password stays 401 and its budget is untouched by the mfa failures.
	before := e.count("SELECT COUNT(*) FROM login_challenges")
	st, out, hdr := e.login("ali", false)
	if st != http.StatusTooManyRequests || code(out) != "mfa_locked" || hdr.Get("Retry-After") == "" || out["challenge_token"] != nil {
		t.Errorf("login of a locked account: %d %v", st, out)
	}
	if after := e.count("SELECT COUNT(*) FROM login_challenges"); after != before {
		t.Errorf("a challenge was opened for a locked account")
	}
	st, out, _ = e.call(http.MethodPost, "/api/login", map[string]any{"username": "ali", "password": "wrong-password-1"}, "")
	if rem := out["error"].(map[string]any)["attempts_remaining"]; st != http.StatusUnauthorized || rem != float64(4) {
		t.Errorf("a wrong password: %d %v, want 401 with 4 attempts remaining (the mfa failures must not be counted)", st, out)
	}

	// The lock lifts when the oldest failure leaves the window.
	e.exec("UPDATE login_attempts SET created_at = now() - interval '16 minutes' WHERE stage = 'mfa' AND id = (SELECT min(id) FROM login_attempts WHERE stage = 'mfa')")
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/disable", with(map[string]any{"code": e.code(secret)}), ali.token); st != 200 {
		t.Errorf("disable after the lock lifted: %d %v", st, out)
	}

	// Audit: the user is the target, never a username in the details.
	for _, action := range []string{"login.mfa_failure", "login.mfa_locked", "totp.setup", "totp.enable", "totp.disable"} {
		if n := e.count("SELECT COUNT(*) FROM audit_logs WHERE action = $1 AND target_id = $2", action, ali.ID); n == 0 {
			t.Errorf("no %s entry with target_id = the user", action)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_logs WHERE (action LIKE 'login.mfa%' OR action LIKE 'totp.%' OR action = 'login.recovery_code_used' OR action = 'user.totp_reset')
		AND details ? 'username'`); n != 0 {
		t.Errorf("%d mfa audit entries carry details.username", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_logs WHERE action LIKE 'login.mfa%' AND NOT (details->>'via' IN ('login', 'enable', 'disable', 'recovery_codes'))`); n != 0 {
		t.Errorf("%d entries with a via outside the closed set", n)
	}
}

// challenge2 is challenge for a name typed in another casing.
func (e *totpEnv) challenge2(t *testing.T, typed string) string {
	t.Helper()
	st, out, _ := e.call(http.MethodPost, "/api/login", map[string]any{"username": typed, "password": totpTestPassword}, "")
	tok, _ := out["challenge_token"].(string)
	if st != 200 || tok == "" {
		t.Fatalf("login: %d %v", st, out)
	}
	return tok
}

// The pending secret counts into the same counter, and the lock does not touch
// the login of an account whose TOTP is not on.
func TestTOTPEnableCountsAndDoesNotLockLogin(t *testing.T) {
	e := newTOTPEnv(t)
	bob := e.account("bob", "viewer")
	st, out, _ := e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": totpTestPassword}, bob.token)
	secret, _ := out["secret"].(string)
	if st != 200 {
		t.Fatal(st, out)
	}
	for i := 0; i < 5; i++ {
		if st, out, _ := e.call(http.MethodPost, "/api/me/totp/enable", map[string]any{"password": totpTestPassword, "code": "000000"}, bob.token); st != http.StatusForbidden || code(out) != "mfa_invalid_code" {
			t.Fatalf("wrong enable %d: %d %v", i+1, st, out)
		}
	}
	if n := e.mfaRows("bob"); n != 5 {
		t.Fatalf("mfa rows = %d, want 5", n)
	}
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/enable", map[string]any{"password": totpTestPassword, "code": e.code(secret)}, bob.token); st != http.StatusTooManyRequests || code(out) != "mfa_locked" {
		t.Errorf("the right code while locked: %d %v", st, out)
	}
	if st, out, _ := e.login("bob", false); st != 200 || out["user"] == nil {
		t.Errorf("the enable lock reached the one-step login: %d %v", st, out)
	}
}

func TestTOTPStateConflicts(t *testing.T) {
	e := newTOTPEnv(t)
	cy := e.account("cy", "viewer")
	body := map[string]any{"password": totpTestPassword, "code": "123456"}
	for _, path := range []string{"/api/me/totp/disable", "/api/me/totp/recovery-codes"} {
		if st, out, _ := e.call(http.MethodPost, path, body, cy.token); st != http.StatusConflict || code(out) != "totp_not_enabled" {
			t.Errorf("%s without TOTP: %d %v", path, st, out)
		}
	}
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/enable", body, cy.token); st != http.StatusConflict || code(out) != "totp_not_set_up" {
		t.Errorf("enable without setup: %d %v", st, out)
	}
	// The state answers before the password and before the gate: nothing counted.
	if n := e.mfaRows("cy"); n != 0 {
		t.Errorf("%d rows from state conflicts", n)
	}
}

func TestTOTPRecoveryCodesRegenerateAndDisable(t *testing.T) {
	e := newTOTPEnv(t)
	dee := e.account("dee", "viewer")
	secret, old := e.enabled(dee)
	pw := totpTestPassword

	// recovery-codes needs the password, then a TOTP code (not a recovery code).
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/recovery-codes", map[string]any{"code": e.code(secret)}, dee.token); st != http.StatusForbidden || code(out) != "wrong_password" {
		t.Errorf("recovery-codes without the password: %d %v", st, out)
	}
	if st, _, _ := e.call(http.MethodPost, "/api/me/totp/recovery-codes", map[string]any{"password": pw, "recovery_code": old[0]}, dee.token); st != http.StatusBadRequest {
		t.Errorf("recovery-codes with a recovery code: %d", st)
	}
	st, out, _ := e.call(http.MethodPost, "/api/me/totp/recovery-codes", map[string]any{"password": pw, "code": e.code(secret)}, dee.token)
	fresh, _ := out["recovery_codes"].([]any)
	if st != 200 || len(fresh) != 10 {
		t.Fatalf("recovery-codes: %d %v", st, out)
	}
	if st, out, _ := e.mfa(e.challenge("dee"), map[string]any{"recovery_code": old[0]}); st != http.StatusUnauthorized {
		t.Errorf("an old recovery code still works: %d %v", st, out)
	}
	if st, out, _ := e.mfa(e.challenge("dee"), map[string]any{"recovery_code": fresh[0]}); st != 200 {
		t.Errorf("a new recovery code: %d %v", st, out)
	}

	// disable with a recovery code.
	e.forget(dee)
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/disable", map[string]any{"password": pw, "recovery_code": fresh[1]}, dee.token); st != 200 {
		t.Fatalf("disable: %d %v", st, out)
	}
	var secretNull, enabledNull bool
	if err := e.st.DB().QueryRow(context.Background(), "SELECT totp_secret_enc IS NULL, totp_enabled_at IS NULL FROM users WHERE id = $1", dee.ID).Scan(&secretNull, &enabledNull); err != nil || !secretNull || !enabledNull {
		t.Errorf("disable left the secret: null=%v enabled null=%v (%v)", secretNull, enabledNull, err)
	}
	if n := e.count("SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = $1", dee.ID); n != 0 {
		t.Errorf("%d recovery codes survived disable", n)
	}
	if st, out, _ := e.login("dee", false); st != 200 || out["user"] == nil {
		t.Errorf("login after disable: %d %v", st, out)
	}
	for _, a := range []string{"totp.recovery_regenerate", "totp.disable", "login.recovery_code_used"} {
		if n := e.count("SELECT COUNT(*) FROM audit_logs WHERE action = $1", a); n == 0 {
			t.Errorf("no %s entry", a)
		}
	}
}

// The ten recovery codes and the code from the app are not three ways past the
// one-use rule: parallel requests with the same proof yield one session.
func TestTOTPParallelLoginsYieldOneSession(t *testing.T) {
	e := newTOTPEnv(t)
	ed := e.account("ed", "viewer")
	secret, recovery := e.enabled(ed)

	race := func(n int, f func(i int) (int, map[string]any)) (ok int) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if st, _ := f(i); st == 200 {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}(i)
		}
		close(start)
		wg.Wait()
		return ok
	}
	tokens := make([]string, 6)
	for i := range tokens {
		tokens[i] = e.challenge("ed")
	}
	c := e.code(secret)
	if n := race(len(tokens), func(i int) (int, map[string]any) {
		st, out, _ := e.mfa(tokens[i], map[string]any{"code": c})
		return st, out
	}); n != 1 {
		t.Errorf("one TOTP code opened %d sessions, want 1", n)
	}
	// A request that waited for the winner sees its code spent and counts as a
	// wrong code, so up to five of them can lock the gate; that lock is not
	// what this test is about.
	e.exec("DELETE FROM login_attempts WHERE stage = 'mfa'")
	for i := range tokens {
		tokens[i] = e.challenge("ed")
	}
	if n := race(len(tokens), func(i int) (int, map[string]any) {
		st, out, _ := e.mfa(tokens[i], map[string]any{"recovery_code": recovery[0]})
		return st, out
	}); n != 1 {
		t.Errorf("one recovery code opened %d sessions, want 1", n)
	}
	// A request that waited for the winner sees its code spent and counts as a
	// wrong code, so up to five of them can lock the gate; that lock is not
	// what this test is about.
	e.exec("DELETE FROM login_attempts WHERE stage = 'mfa'")
	// One challenge, two different valid proofs.
	e.forget(ed)
	one := e.challenge("ed")
	proofs := []map[string]any{{"code": e.code(secret)}, {"recovery_code": recovery[1]}, {"recovery_code": recovery[2]}}
	if n := race(len(proofs), func(i int) (int, map[string]any) {
		st, out, _ := e.mfa(one, proofs[i])
		return st, out
	}); n != 1 {
		t.Errorf("one challenge opened %d sessions, want 1", n)
	}
	// Parallel wrong codes never leave more than five rows.
	e.exec("DELETE FROM login_attempts WHERE stage = 'mfa'")
	toks := make([]string, 4)
	for i := range toks {
		toks[i] = e.challenge("ed")
	}
	race(12, func(i int) (int, map[string]any) {
		st, out, _ := e.mfa(toks[i%len(toks)], map[string]any{"code": "000000"})
		return st, out
	})
	if n := e.mfaRows("ed"); n < 1 || n > 5 {
		t.Errorf("%d mfa rows from 12 parallel wrong codes, want 1..5", n)
	}
}

// T9, T10: the session door asks for the password, and the session gets five
// wrong ones.
func TestTOTPSessionPasswordBudget(t *testing.T) {
	e := newTOTPEnv(t)
	fay := e.account("fay", "viewer")

	// Parallel: six requests' worth is beyond the budget, and only five get to
	// try a password.
	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[int]int{}
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			st, _, _ := e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": "wrong-password-1"}, fay.token)
			mu.Lock()
			statuses[st]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	if statuses[http.StatusForbidden] != 5 || statuses[http.StatusForbidden]+statuses[http.StatusUnauthorized] != 12 {
		t.Errorf("12 parallel wrong passwords: %v, want five 403 and the rest 401", statuses)
	}
	if st, _, _ := e.call(http.MethodGet, "/api/me", nil, fay.token); st != http.StatusUnauthorized {
		t.Errorf("the session survived its fifth wrong password: %d", st)
	}

	// changePassword sits behind the same counter; a right password resets it.
	gus := e.account("gus", "viewer")
	change := func(cur string) (int, map[string]any) {
		st, out, _ := e.call(http.MethodPost, "/api/me/password", map[string]any{"current_password": cur, "new_password": "another-long-password"}, gus.token)
		return st, out
	}
	for i := 0; i < 4; i++ {
		if st, out := change("wrong-password-1"); st != http.StatusForbidden || errMessage(out) != "current password is wrong" {
			t.Fatalf("wrong current password %d: %d %v", i+1, st, out)
		}
	}
	if st, _, _ := e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": totpTestPassword}, gus.token); st != 200 {
		t.Fatalf("a right password after four wrong ones: %d", st)
	}
	var tries int
	if err := e.st.DB().QueryRow(context.Background(), "SELECT password_failures FROM sessions WHERE user_id = $1", gus.ID).Scan(&tries); err != nil || tries != 0 {
		t.Errorf("password_failures after a right password = %d (%v), want 0", tries, err)
	}
	for i := 1; i <= 5; i++ {
		if st, _ := change("wrong-password-1"); st != http.StatusForbidden {
			t.Fatalf("wrong current password %d of the second round: %d", i, st)
		}
	}
	if st, _ := change(totpTestPassword); st != http.StatusUnauthorized {
		t.Errorf("a right password after the fifth wrong one: %d, want the session gone", st)
	}
}

// Every TOTP endpoint is for a signed-in dashboard session, not for a key.
func TestTOTPEndpointsRefuseKeys(t *testing.T) {
	e := newTOTPEnv(t)
	adm := e.account("root", "admin")
	other := e.account("other", "viewer")
	_, raw, err := e.st.CreateAPIKey(context.Background(), &store.APIKey{Kind: store.KindManagement, Name: "all", UserID: adm.ID, Scopes: store.ManagementScopes()})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"password": totpTestPassword, "code": "123456"}
	for _, path := range []string{
		"/api/me/totp/setup", "/api/me/totp/enable", "/api/me/totp/disable", "/api/me/totp/recovery-codes",
		"/api/users/" + itoa(other.ID) + "/totp/reset",
	} {
		if st, out, _ := e.call(http.MethodPost, path, body, raw); st != http.StatusForbidden || code(out) != "session_required" {
			t.Errorf("%s with a key: %d %v", path, st, out)
		}
	}
	// And an unauthenticated caller is not a session either.
	if st, _, _ := e.call(http.MethodPost, "/api/me/totp/setup", body, ""); st != http.StatusUnauthorized {
		t.Errorf("setup without credentials: %d", st)
	}
}

// T6, T12 and the ways out of the lock.
func TestTOTPExitsAndSelfProtection(t *testing.T) {
	e := newTOTPEnv(t)
	root := e.account("root", "admin")
	viewer := e.account("viewer", "viewer")

	if st, _, _ := e.call(http.MethodPost, "/api/users/"+itoa(root.ID)+"/totp/reset", nil, root.token); st != http.StatusBadRequest {
		t.Errorf("T6 on oneself: %d, want 400", st)
	}
	if st, _, _ := e.call(http.MethodPost, "/api/users/"+itoa(viewer.ID)+"/totp/reset", nil, viewer.token); st != http.StatusForbidden {
		t.Errorf("T6 by a viewer: %d, want 403", st)
	}
	if st, _, _ := e.call(http.MethodPost, "/api/users/99999/totp/reset", nil, root.token); st != http.StatusNotFound {
		t.Errorf("T6 on nobody: %d, want 404", st)
	}
	if st, out, _ := e.call(http.MethodPost, "/api/users/"+itoa(root.ID)+"/reset-password", map[string]any{"new_password": "another-long-password"}, root.token); st != http.StatusBadRequest {
		t.Errorf("T12 on oneself: %d %v, want 400", st, out)
	}
	// Still the old password: the refused reset changed nothing.
	if st, _, _ := e.call(http.MethodPost, "/api/login", map[string]any{"username": "root", "password": totpTestPassword}, ""); st != 200 {
		t.Errorf("the self reset changed the password")
	}

	// lock builds the state every exit has to clear: TOTP on, three wrong
	// codes, an open challenge, a second session.
	lock := func(name string) (*actor, string) {
		u := e.account(name, "viewer")
		secret, _ := e.enabled(u)
		for i := 0; i < 3; i++ {
			e.mfa(e.challenge(name), map[string]any{"code": "000000"})
		}
		e.challenge(name)
		if e.mfaRows(name) != 3 || e.count("SELECT COUNT(*) FROM login_challenges WHERE user_id = $1", u.ID) == 0 {
			t.Fatalf("setup of %s", name)
		}
		return u, secret
	}

	// An admin's reset-password: rows and challenges go, TOTP stays.
	hal, _ := lock("hal")
	if st, out, _ := e.call(http.MethodPost, "/api/users/"+itoa(hal.ID)+"/reset-password", map[string]any{"new_password": "another-long-password"}, root.token); st != 200 {
		t.Fatalf("reset-password: %d %v", st, out)
	}
	if e.mfaRows("hal") != 0 || e.count("SELECT COUNT(*) FROM login_challenges WHERE user_id = $1", hal.ID) != 0 {
		t.Errorf("reset-password left the mfa rows or challenges")
	}
	if st, _ := e.mustStatus("/api/me", hal.token); st != http.StatusUnauthorized {
		t.Errorf("hal's session survived the reset: %d", st)
	}
	if s, _ := e.st.GetTOTPStatus(context.Background(), hal.ID); !s.Enabled {
		t.Error("reset-password switched TOTP off; only --disable-totp / T6 do")
	}

	// T6.
	ivy, _ := lock("ivy")
	if st, out, _ := e.call(http.MethodPost, "/api/users/"+itoa(ivy.ID)+"/totp/reset", nil, root.token); st != 200 {
		t.Fatalf("T6: %d %v", st, out)
	}
	if s, _ := e.st.GetTOTPStatus(context.Background(), ivy.ID); s.Enabled || s.HasSecret {
		t.Errorf("T6 left %+v", s)
	}
	if e.mfaRows("ivy") != 0 || e.count("SELECT COUNT(*) FROM login_challenges WHERE user_id = $1", ivy.ID) != 0 ||
		e.count("SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = $1", ivy.ID) != 0 {
		t.Errorf("T6 left rows, challenges or recovery codes")
	}
	if st, _ := e.mustStatus("/api/me", ivy.token); st != http.StatusUnauthorized {
		t.Errorf("ivy's session survived T6: %d", st)
	}
	if n := e.count("SELECT COUNT(*) FROM audit_logs WHERE action = 'user.totp_reset' AND target_id = $1 AND actor_user_id = $2", ivy.ID, root.ID); n != 1 {
		t.Errorf("user.totp_reset entries = %d", n)
	}

	// changePassword closes challenges and keeps the counter.
	jon, _ := lock("jon")
	if st, out, _ := e.call(http.MethodPost, "/api/me/password", map[string]any{"current_password": totpTestPassword, "new_password": "another-long-password"}, jon.token); st != 200 {
		t.Fatalf("changePassword: %d %v", st, out)
	}
	if n := e.count("SELECT COUNT(*) FROM login_challenges WHERE user_id = $1", jon.ID); n != 0 {
		t.Errorf("changePassword left %d challenges", n)
	}
	if n := e.mfaRows("jon"); n != 3 {
		t.Errorf("changePassword touched the mfa counter: %d rows, want 3", n)
	}
}

func (e *totpEnv) mustStatus(path, token string) (int, map[string]any) {
	st, out, _ := e.call(http.MethodGet, path, nil, token)
	return st, out
}

// A check of the same account in flight makes the next one a 429 with a
// one-second Retry-After; the challenge is not spent and nothing is counted.
func TestTOTPBusyGateIs429(t *testing.T) {
	e := newTOTPEnv(t)
	kit := e.account("kit", "viewer")
	secret, _ := e.enabled(kit)
	tok := e.challenge("kit")

	ctx := context.Background()
	holder, err := e.st.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE", kit.ID); err != nil {
		t.Fatal(err)
	}
	st, out, hdr := e.mfa(tok, map[string]any{"code": e.code(secret)})
	if st != http.StatusTooManyRequests || code(out) != "mfa_busy" || hdr.Get("Retry-After") != "1" {
		t.Errorf("login/mfa while the account is busy: %d %v retry-after=%q", st, out, hdr.Get("Retry-After"))
	}
	if st, out, hdr := e.call(http.MethodPost, "/api/me/totp/disable", map[string]any{"password": totpTestPassword, "code": "000000"}, kit.token); st != http.StatusTooManyRequests || code(out) != "mfa_busy" || hdr.Get("Retry-After") != "1" {
		t.Errorf("disable while the account is busy: %d %v", st, out)
	}
	if n := e.mfaRows("kit"); n != 0 {
		t.Errorf("busy refusals counted: %d rows", n)
	}
	_ = holder.Rollback(ctx)
	if st, out, _ := e.mfa(tok, map[string]any{"code": e.code(secret)}); st != 200 {
		t.Errorf("the same challenge once the account is free: %d %v", st, out)
	}
}

// The lock writes one login.mfa_locked entry per episode, however many
// requests knock on it; /me says whether TOTP is on; /security/logins is about
// passwords.
func TestTOTPLockAuditOnceAndMeAndSummary(t *testing.T) {
	e := newTOTPEnv(t)
	wes := e.account("wes", "admin")
	if st, out, _ := e.call(http.MethodGet, "/api/me", nil, wes.token); st != 200 || out["totp_enabled"] != false {
		t.Fatalf("/me before: %d %v", st, out)
	}
	secret, _ := e.enabled(wes)
	if st, out, _ := e.call(http.MethodGet, "/api/me", nil, wes.token); st != 200 || out["totp_enabled"] != true {
		t.Fatalf("/me after enable: %d %v", st, out)
	}
	// A pending secret is not an enabled one.
	pend := e.account("pam", "viewer")
	e.call(http.MethodPost, "/api/me/totp/setup", map[string]any{"password": totpTestPassword}, pend.token)
	if _, out, _ := e.call(http.MethodGet, "/api/me", nil, pend.token); out["totp_enabled"] != false {
		t.Errorf("/me with a pending secret: %v", out)
	}

	held := e.challenge("wes")
	for i := 0; i < 5; i++ {
		e.mfa(e.challenge("wes"), map[string]any{"code": "000000"})
	}
	for i := 0; i < 8; i++ {
		if st, out, _ := e.mfa(held, map[string]any{"code": e.code(secret)}); st != http.StatusTooManyRequests {
			t.Fatalf("knock %d: %d %v", i, st, out)
		}
	}
	e.login("wes", false)
	if n := e.count("SELECT COUNT(*) FROM audit_logs WHERE action = 'login.mfa_locked' AND target_id = $1", wes.ID); n != 1 {
		t.Errorf("login.mfa_locked entries = %d after 9 refused requests, want 1", n)
	}

	st, out, _ := e.call(http.MethodGet, "/api/security/logins", nil, wes.token)
	if st != 200 || out["failed_last_hour"] != float64(0) || out["failed_last_24h"] != float64(0) {
		t.Errorf("/security/logins with only mfa failures: %d %v", st, out)
	}
}

// A recovery code spent on disable leaves none: the audit entry says 0.
func TestTOTPDisableWithRecoveryCodeAuditsZeroRemaining(t *testing.T) {
	e := newTOTPEnv(t)
	xan := e.account("xan", "viewer")
	_, recovery := e.enabled(xan)
	if st, out, _ := e.call(http.MethodPost, "/api/me/totp/disable", map[string]any{"password": totpTestPassword, "recovery_code": recovery[0]}, xan.token); st != 200 {
		t.Fatalf("disable: %d %v", st, out)
	}
	var details string
	if err := e.st.DB().QueryRow(context.Background(), "SELECT details::text FROM audit_logs WHERE action = 'login.recovery_code_used'").Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"remaining": 0`) || !strings.Contains(details, `"via": "disable"`) {
		t.Errorf("details = %s", details)
	}
}

// The race the conditional insert closes, through the handler: a one-step login
// read the account before an enable and writes its session after it. The login
// waits on the enable's row lock, and once that commits it must come back as a
// challenge, with no session, token or cookie.
func TestTOTPLoginRacingAnEnableGetsAChallenge(t *testing.T) {
	e := newTOTPEnv(t)
	ned := e.account("ned", "viewer")
	ctx := context.Background()
	if ok, err := e.st.SetTOTPSecret(ctx, ned.ID, "JBSWY3DPEHPK3PXP"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// The enable, as the endpoint runs it, not yet committed.
	enable, err := e.st.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enable.Rollback(ctx) }()
	if err := store.EnableTOTPTx(ctx, enable, ned.ID); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		st  int
		out map[string]any
		hdr http.Header
	}
	done := make(chan answer, 1)
	go func() {
		st, out, hdr := e.login("ned", true)
		done <- answer{st, out, hdr}
	}()

	// The login has read "no second factor" (it sees the committed row) and is
	// now parked on the lock: wait for a session insert blocked on a lock
	// instead of sleeping for a guessed time.
	deadline := time.Now().Add(15 * time.Second)
	for blocked := 0; blocked == 0; {
		select {
		case a := <-done:
			t.Fatalf("the login finished while the enable was in flight: %d %v", a.st, a.out)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the login never reached the session insert's lock wait")
		}
		blocked = e.count(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE 'INSERT INTO sessions%'`)
		if blocked == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}

	if err := store.DeleteUserSessionsExceptTx(ctx, enable, ned.ID, "none"); err != nil {
		t.Fatal(err)
	}
	if err := enable.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	a := <-done
	if a.st != 200 || a.out["mfa_required"] != true || a.out["challenge_token"] == nil ||
		a.out["token"] != nil || a.out["user"] != nil || a.hdr.Get("Set-Cookie") != "" {
		t.Errorf("the login that raced an enable: %d %v cookie=%q", a.st, a.out, a.hdr.Get("Set-Cookie"))
	}
	if n := e.count("SELECT COUNT(*) FROM sessions WHERE user_id = $1", ned.ID); n != 0 {
		t.Errorf("%d sessions of the account exist after the enable", n)
	}
}
