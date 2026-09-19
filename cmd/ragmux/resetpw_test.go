package main

import (
	"context"
	"os"
	"strings"
	"testing"

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
