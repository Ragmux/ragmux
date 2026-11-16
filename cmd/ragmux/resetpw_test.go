package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func TestCheckResetPassword(t *testing.T) {
	cases := map[string]bool{
		"":                            false,
		"short":                       false,
		"elevenchars":                 false, // 12 characters exactly is the floor
		"twelve-chars":                true,
		strings.Repeat("a", 72):       true,
		strings.Repeat("a", 73):       false,
		"a much longer passphrase ok": true,
	}
	for pw, ok := range cases {
		if got := checkResetPassword(pw) == ""; got != ok {
			t.Errorf("checkResetPassword(%q) accepted = %v, want %v", pw, got, ok)
		}
	}
	// The command and the dashboard refuse with the same sentence.
	if got, want := checkResetPassword("elevenchars"), auth.ErrPasswordTooShort.Error(); got != want {
		t.Errorf("11 characters: %q, want %q", got, want)
	}
	if got, want := checkResetPassword(strings.Repeat("a", 73)), auth.ErrPasswordTooLong.Error(); got != want {
		t.Errorf("73 bytes: %q, want %q", got, want)
	}
}

// runReset drives the subcommand against a test schema, feeding stdin from a
// string and capturing nothing but the exit code: the command prints to the
// process's stdout, which the test does not need to read.
func runReset(t *testing.T, cfg store.OpenConfig, stdin string, args ...string) int {
	t.Helper()
	t.Setenv("DATABASE_URL", cfg.DatabaseURL)
	t.Setenv("SECRET_KEY", cfg.SecretKeyHex)
	t.Setenv("DATA_DIR", cfg.DataDir)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old; _ = r.Close() }()
	return resetPassword(args)
}

func TestResetPasswordCLI(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	st := testdb.OpenWith(t, cfg)
	u, err := st.CreateUser(ctx, "Ops", "old-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, u.ID, "tok", 3600e9); err != nil {
		t.Fatal(err)
	}
	k, raw, err := st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindManagement, Name: "ops",
		UserID: u.ID, Scopes: []string{store.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}

	// Argument validation happens before the database is touched.
	for _, c := range []struct {
		name string
		args []string
		want int
	}{
		{"no username", []string{"--generate"}, 2},
		{"neither source", []string{"ops"}, 2},
		{"both sources", []string{"ops", "--generate", "--stdin"}, 2},
		{"too short", []string{"ops", "--stdin"}, 2},
	} {
		if got := runReset(t, cfg, "short\n", c.args...); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
	}
	if got := runReset(t, cfg, "eleven-char\n", "ops", "--stdin"); got != 2 {
		t.Errorf("11 characters: exit %d, want 2", got)
	}

	// An unknown account is exit 1 and creates nothing.
	if got := runReset(t, cfg, "a-good-long-password\n", "nobody", "--stdin"); got != 1 {
		t.Errorf("unknown user: exit %d", got)
	}
	if n, _ := st.CountUsers(ctx); n != 1 {
		t.Fatalf("users after a failed reset: %d", n)
	}

	// --stdin sets the password, matched case-insensitively, and revokes the
	// sessions but not the keys.
	if got := runReset(t, cfg, "a-good-long-password\n", "ops", "--stdin"); got != 0 {
		t.Fatalf("stdin reset: exit %d", got)
	}
	after, err := st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(after.PasswordHash, "a-good-long-password") {
		t.Error("password was not changed")
	}
	if _, err := st.UserBySession(ctx, "tok"); err == nil {
		t.Error("sessions survived the reset")
	}
	if _, _, err := st.ResolveManagementKey(ctx, raw); err != nil {
		t.Errorf("api key was revoked without --revoke-keys: %v", err)
	}

	// --no-revoke leaves the sessions; --revoke-keys takes the keys.
	if err := st.CreateSession(ctx, u.ID, "tok2", 3600e9); err != nil {
		t.Fatal(err)
	}
	if got := runReset(t, cfg, "another-long-password\n", "ops", "--stdin", "--no-revoke", "--revoke-keys"); got != 0 {
		t.Fatalf("no-revoke reset: exit %d", got)
	}
	if _, err := st.UserBySession(ctx, "tok2"); err != nil {
		t.Errorf("--no-revoke still revoked the sessions: %v", err)
	}
	if got, err := st.GetAPIKey(ctx, k.ID); err != nil || got.RevokedAt == nil {
		t.Errorf("--revoke-keys left the key live: %+v %v", got, err)
	}

	// --generate needs no stdin and prints a password that works.
	if got := runReset(t, cfg, "", "ops", "--generate"); got != 0 {
		t.Fatalf("generate: exit %d", got)
	}
	regenerated, err := st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if auth.CheckPassword(regenerated.PasswordHash, "another-long-password") {
		t.Error("--generate did not change the password")
	}

	// Every run is audited as the "cli" actor, with no account behind it.
	entries, _, err := st.ListAuditLogs(ctx, store.AuditFilter{Action: "user.reset_password"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("%d audit entries, want 3", len(entries))
	}
	e := entries[0]
	if e.ActorUsername != "cli" || e.ActorUserID != nil || e.TargetID == nil || *e.TargetID != u.ID {
		t.Errorf("audit entry = %+v", e)
	}
	if e.Details["via"] != "cli" || e.Details["revoked_sessions"] == nil || e.Details["revoked_keys"] == nil {
		t.Errorf("audit details = %v", e.Details)
	}
}

// T5 exits and T7: a reset forgets the user's wrong second-factor codes and
// closes the open challenges (the way out of the lock) under the lower-cased
// name the gate writes; TOTP itself stays unless --disable-totp is given.
func TestResetPasswordCLITOTP(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	st := testdb.OpenWith(t, cfg)
	u, err := st.CreateUser(ctx, "Ops", "old-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	bystander, err := st.CreateUser(ctx, "other", "old-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []*store.User{u, bystander} {
		if ok, err := st.SetTOTPSecret(ctx, x.ID, "JBSWY3DPEHPK3PXP"); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if _, err := st.DB().Exec(ctx, "UPDATE users SET totp_enabled_at = now() WHERE id = $1", x.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(ctx, "INSERT INTO user_recovery_codes (user_id, code_hash) VALUES ($1, 'h')", x.ID); err != nil {
			t.Fatal(err)
		}
	}
	wrong := func() {
		t.Helper()
		for i := 0; i < store.MFALockFailures; i++ {
			_, err := st.VerifySecondFactor(ctx, store.SecondFactor{UserID: u.ID, Via: store.ViaDisable, Code: "000000",
				Check: func(string, string, int64) (int64, bool) { return 0, false }}, nil)
			if !errors.Is(err, store.ErrMFAWrongCode) {
				t.Fatal(err)
			}
		}
		if _, err := st.CreateLoginChallenge(ctx, u.ID, "ch-"+time.Now().String(), false, "1.2.3.4", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	rows := func(name string) (n int) {
		t.Helper()
		if err := st.DB().QueryRow(ctx, "SELECT COUNT(*) FROM login_attempts WHERE stage = 'mfa' AND username = $1", name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	challenges := func(id int64) (n int) {
		t.Helper()
		if err := st.DB().QueryRow(ctx, "SELECT COUNT(*) FROM login_challenges WHERE user_id = $1", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := st.CreateLoginChallenge(ctx, bystander.ID, "ch-other", false, "1.2.3.4", time.Minute); err != nil {
		t.Fatal(err)
	}

	wrong()
	if l, _ := st.MFALocked(ctx, u.ID); l == nil || rows("ops") != 5 {
		t.Fatalf("setup: locked=%v rows=%d", l, rows("ops"))
	}
	// Default: the lock goes, TOTP stays.
	if got := runReset(t, cfg, "a-good-long-password\n", "OPS", "--stdin"); got != 0 {
		t.Fatalf("reset: exit %d", got)
	}
	if rows("ops") != 0 || challenges(u.ID) != 0 {
		t.Errorf("reset left %d rows and %d challenges", rows("ops"), challenges(u.ID))
	}
	if l, _ := st.MFALocked(ctx, u.ID); l != nil {
		t.Error("the account is still locked after the reset")
	}
	if s, _ := st.GetTOTPStatus(ctx, u.ID); !s.Enabled {
		t.Error("a plain reset switched TOTP off")
	}
	if challenges(bystander.ID) != 1 {
		t.Error("another account's challenge was closed")
	}

	// --disable-totp: the secret, the codes and the rows go, the audit says so.
	wrong()
	if got := runReset(t, cfg, "another-long-password\n", "ops", "--stdin", "--disable-totp"); got != 0 {
		t.Fatalf("reset --disable-totp: exit %d", got)
	}
	if s, _ := st.GetTOTPStatus(ctx, u.ID); s.Enabled || s.HasSecret {
		t.Errorf("status after --disable-totp = %+v", s)
	}
	if n, _ := unusedRecoveryCodes(t, st, u.ID); n != 0 || rows("ops") != 0 || challenges(u.ID) != 0 {
		t.Errorf("--disable-totp left %d codes, %d rows, %d challenges", n, rows("ops"), challenges(u.ID))
	}
	if s, _ := st.GetTOTPStatus(ctx, bystander.ID); !s.Enabled {
		t.Error("another account's TOTP was switched off")
	}
	entries, _, err := st.ListAuditLogs(ctx, store.AuditFilter{Action: "user.reset_password"})
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit entries = %d, %v", len(entries), err)
	}
	if entries[0].Details["totp_disabled"] != true || entries[1].Details["totp_disabled"] != nil {
		t.Errorf("audit details: newest %v, older %v", entries[0].Details, entries[1].Details)
	}
}

// unusedRecoveryCodes counts a user's recovery codes that are not spent.
func unusedRecoveryCodes(t *testing.T, s *store.Store, userID int64) (int, error) {
	t.Helper()
	var n int
	err := s.DB().QueryRow(context.Background(), "SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = $1 AND used_at IS NULL", userID).Scan(&n)
	return n, err
}
