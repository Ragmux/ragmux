package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

var pseudonymRe = regexp.MustCompile(`^anon-[0-9a-f]{16}$`)

type anonFixture struct {
	t    *testing.T
	ctx  context.Context
	s    *store.Store
	p1   *store.Project
	p2   *store.Project
	root *store.User // a second active admin, so the target is never the last one
}

func newAnonFixture(t *testing.T) *anonFixture {
	t.Helper()
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", APIKey: "k", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p1, _, err := s.CreateProject(ctx, &store.Project{Name: "p1", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := s.CreateProject(ctx, &store.Project{Name: "p2", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateUser(ctx, "root", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	return &anonFixture{t: t, ctx: ctx, s: s, p1: p1, p2: p2, root: root}
}

func (f *anonFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.s.DB().Exec(f.ctx, q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *anonFixture) count(q string, args ...any) int64 {
	f.t.Helper()
	var n int64
	if err := f.s.DB().QueryRow(f.ctx, q, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (f *anonFixture) audit(actor *int64, actorName, action, targetType string, target *int64, details map[string]any) {
	f.t.Helper()
	if err := f.s.InsertAuditLog(f.ctx, &store.AuditLog{ActorUserID: actor, ActorUsername: actorName, Action: action,
		TargetType: targetType, TargetID: target, Details: details, IP: "203.0.113.7"}); err != nil {
		f.t.Fatal(err)
	}
}

func i64(v int64) *int64 { return &v }

// auditText is every audit row as one lower-cased string, for "the original
// value is nowhere" checks.
func (f *anonFixture) auditText() string {
	f.t.Helper()
	var s string
	if err := f.s.DB().QueryRow(f.ctx,
		`SELECT lower(COALESCE(string_agg(actor_username || ' ' || details::text, ' ' ORDER BY id), '')) FROM audit_logs`).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestAnonymizeUserKeepsUsageAndRemovesTheName(t *testing.T) {
	f := newAnonFixture(t)
	ctx := f.ctx
	alice, err := f.s.CreateUser(ctx, "Alice", "h", "editor")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := f.s.CreateUser(ctx, "bob", "h", "editor")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.AddProjectMember(ctx, f.p1.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateSession(ctx, alice.ID, "tok-alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	// A second factor and what hangs off it.
	f.exec("UPDATE users SET totp_secret_enc = '\\x01', totp_enabled_at = now(), totp_last_step = 99 WHERE id = $1", alice.ID)
	f.exec("INSERT INTO user_recovery_codes (user_id, code_hash) VALUES ($1, 'x')", alice.ID)
	f.exec("INSERT INTO login_challenges (user_id, token_hash, expires_at) VALUES ($1, 'y', now() + interval '1 hour')", alice.ID)

	// Keys: a gateway key whose name is a person's name, and a management key
	// named "key-<id of the other>" to prove the rename cannot collide.
	k1, _, err := f.s.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: "Alice's laptop", UserID: alice.ID,
		Scopes: store.DefaultGatewayScopes(), ProjectIDs: []int64{f.p1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	k2, _, err := f.s.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindManagement, Name: "tmp", UserID: alice.ID, Scopes: store.ManagementScopes()})
	if err != nil {
		t.Fatal(err)
	}
	f.exec("UPDATE api_keys SET name = $2 WHERE id = $1", k2.ID, fmt.Sprintf("key-%d", k1.ID))
	bobKey, _, err := f.s.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: "Bob key", UserID: bob.ID,
		Scopes: store.DefaultGatewayScopes(), ProjectIDs: []int64{f.p1.ID}})
	if err != nil {
		t.Fatal(err)
	}

	// Usage history: rows attributed to Alice and to her key.
	for i := 0; i < 3; i++ {
		if err := f.s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: f.p1.ID, ModelName: "m", StatusCode: 200,
			PromptTokens: 10 + i, CompletionTokens: 5, CostMicros: 100, APIKeyID: &k1.ID, UserID: &alice.ID}); err != nil {
			t.Fatal(err)
		}
	}
	type totals struct{ n, prompt, cost int64 }
	read := func() totals {
		var x totals
		if err := f.s.DB().QueryRow(ctx, "SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(cost_micros),0) FROM request_logs WHERE user_id = $1",
			alice.ID).Scan(&x.n, &x.prompt, &x.cost); err != nil {
			t.Fatal(err)
		}
		return x
	}
	before := read()
	if before.n != 3 {
		t.Fatalf("fixture: %+v", before)
	}

	// Login attempts and audit rows in every shape the rules name.
	f.exec("INSERT INTO login_attempts (username, ip, success) VALUES ('alice', '1.1.1.1', false), ('alice', '1.1.1.1', true), ('bob', '2.2.2.2', false)")
	f.audit(&alice.ID, "Alice", "project.update", "project", &f.p1.ID, map[string]any{"name": "p1"})
	f.audit(&f.root.ID, "root", "user.update", "user", &alice.ID, map[string]any{"username": "Alice", "role": "editor"})
	f.audit(nil, "", "login.failure", "user", nil, map[string]any{"username": "ALICE"})
	f.audit(nil, "", "login.rate_limited", "user", nil, map[string]any{"username": "aLice"})
	f.audit(nil, "", "login.locked", "user", nil, map[string]any{"username": "alice"})
	f.audit(nil, "", "login.failure", "user", nil, map[string]any{"username": "bob"})
	// apikey.* with user_id, without it, for a key that is gone, and by actor.
	f.audit(&f.root.ID, "root", "apikey.create", "api_key", &k1.ID, map[string]any{"name": "Alice's laptop", "user_id": alice.ID})
	f.audit(&f.root.ID, "root", "apikey.revoke", "api_key", &k1.ID, map[string]any{"name": "Alice's laptop", "kind": "gateway"})
	const goneKey = 990001
	f.audit(&f.root.ID, "root", "apikey.create", "api_key", i64(goneKey), map[string]any{"name": "Alice old", "user_id": alice.ID})
	f.audit(&f.root.ID, "root", "apikey.delete", "api_key", i64(goneKey), map[string]any{"name": "Alice old", "kind": "gateway"})
	f.audit(&alice.ID, "Alice", "apikey.revoke", "api_key", &bobKey.ID, map[string]any{"name": "Bob key", "kind": "gateway"})
	f.audit(&alice.ID, "Alice", "apikey.delete", "api_key", i64(990002), map[string]any{"name": "Carol's key", "kind": "gateway"})
	f.audit(&bob.ID, "bob", "apikey.create", "api_key", &bobKey.ID, map[string]any{"name": "Bob key", "user_id": bob.ID})

	res, err := f.s.AnonymizeUser(ctx, alice.ID, "Alice", "unusable-hash")
	if err != nil {
		t.Fatal(err)
	}
	if !pseudonymRe.MatchString(res.Pseudonym) {
		t.Fatalf("pseudonym %q", res.Pseudonym)
	}
	// Not a hash of the name: neither of the two obvious digests.
	for _, in := range []string{"Alice", "alice"} {
		sum := sha256.Sum256([]byte(in))
		if h := hex.EncodeToString(sum[:])[:16]; res.Pseudonym == "anon-"+h {
			t.Errorf("pseudonym is the SHA-256 prefix of %q", in)
		}
	}
	if res.RevokedKeys != 2 {
		t.Errorf("revoked keys = %d, want 2", res.RevokedKeys)
	}

	var (
		name, hash string
		active     bool
		stamped    bool
		totp       bool
	)
	if err := f.s.DB().QueryRow(ctx, `SELECT username, password_hash, is_active, anonymized_at IS NOT NULL,
		(totp_secret_enc IS NOT NULL OR totp_enabled_at IS NOT NULL OR totp_last_step <> 0) FROM users WHERE id = $1`,
		alice.ID).Scan(&name, &hash, &active, &stamped, &totp); err != nil {
		t.Fatal(err)
	}
	if name != res.Pseudonym || hash != "unusable-hash" || active || !stamped || totp {
		t.Errorf("user row: %q %q active=%v stamped=%v totp=%v", name, hash, active, stamped, totp)
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM sessions WHERE user_id = $1",
		"SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = $1",
		"SELECT COUNT(*) FROM login_challenges WHERE user_id = $1",
		"SELECT COUNT(*) FROM project_members WHERE user_id = $1",
		"SELECT COUNT(*) FROM api_keys WHERE user_id = $1 AND (revoked_at IS NULL OR name <> 'key-' || id)",
	} {
		if n := f.count(q, alice.ID); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
	if n := f.count("SELECT COUNT(*) FROM api_keys WHERE user_id = $1", alice.ID); n != 2 {
		t.Errorf("keys kept = %d, want 2 (revoked, not deleted)", n)
	}
	// Login attempts: Alice's go, Bob's stay.
	if n := f.count("SELECT COUNT(*) FROM login_attempts WHERE lower(username) = 'alice'"); n != 0 {
		t.Errorf("login_attempts of the name = %d", n)
	}
	if n := f.count("SELECT COUNT(*) FROM login_attempts WHERE username = 'bob'"); n != 1 {
		t.Errorf("login_attempts of another user = %d, want 1", n)
	}
	// Usage history is exactly what it was.
	if after := read(); after != before {
		t.Errorf("usage changed: %+v -> %+v", before, after)
	}
	// The original value is in no audit row; Bob's own are untouched.
	text := f.auditText()
	if strings.Contains(text, "alice") {
		t.Errorf("an audit row still carries the original value:\n%s", text)
	}
	for _, want := range []string{"bob key", `"username": "bob"`} {
		if !strings.Contains(text, want) {
			t.Errorf("Bob's rows lost %q", want)
		}
	}
	// One pseudonym on every touched row, the key rows carry key-<id>.
	var distinct int64
	if err := f.s.DB().QueryRow(ctx, `SELECT COUNT(DISTINCT v) FROM (
		SELECT actor_username v FROM audit_logs WHERE actor_username LIKE 'anon-%'
		UNION ALL SELECT details->>'username' FROM audit_logs WHERE details->>'username' LIKE 'anon-%') x`).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 1 {
		t.Errorf("distinct pseudonyms across audit = %d, want 1", distinct)
	}
	for id, want := range map[int64]string{k1.ID: fmt.Sprintf("key-%d", k1.ID), goneKey: fmt.Sprintf("key-%d", goneKey),
		bobKey.ID: fmt.Sprintf("key-%d", bobKey.ID), 990002: "key-990002"} {
		// Every entry of these keys that was reachable now carries key-<id>;
		// the ones that are not Alice's (Bob's own create entry) keep their name.
		rows, err := f.s.DB().Query(ctx, "SELECT actor_user_id, action, details->>'name' FROM audit_logs WHERE target_type = 'api_key' AND target_id = $1", id)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var actor *int64
			var action, got string
			if err := rows.Scan(&actor, &action, &got); err != nil {
				t.Fatal(err)
			}
			bobsOwn := actor != nil && *actor == bob.ID
			if bobsOwn && got != "Bob key" || !bobsOwn && got != want {
				t.Errorf("key %d entry %s: name %q, want %q (bobsOwn=%v)", id, action, got, want, bobsOwn)
			}
		}
		rows.Close()
	}
	// Audit IP is a security record and stays (D3).
	if n := f.count("SELECT COUNT(*) FROM audit_logs WHERE ip <> '203.0.113.7'"); n != 0 {
		t.Errorf("audit ip changed on %d rows", n)
	}

	// The name is free again.
	if _, err := f.s.CreateUser(ctx, "Alice", "h", "viewer"); err != nil {
		t.Errorf("re-registering the name: %v", err)
	}
	// A second call, whatever it confirms.
	for _, c := range []string{"Alice", res.Pseudonym} {
		if _, err := f.s.AnonymizeUser(ctx, alice.ID, c, "h"); !errors.Is(err, store.ErrAlreadyAnonymized) {
			t.Errorf("second call confirming %q: %v, want ErrAlreadyAnonymized", c, err)
		}
	}
}

func TestAnonymizeUserRefusals(t *testing.T) {
	f := newAnonFixture(t)
	ctx := f.ctx
	carol, err := f.s.CreateUser(ctx, "Carol", "h", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	// The confirmation is the stored name, exactly.
	for _, c := range []string{"", "carol", "Carol ", "someone"} {
		if _, err := f.s.AnonymizeUser(ctx, carol.ID, c, "h"); !errors.Is(err, store.ErrConfirmMismatch) {
			t.Errorf("confirm %q: %v, want ErrConfirmMismatch", c, err)
		}
	}
	if _, err := f.s.AnonymizeUser(ctx, 987654, "x", "h"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown id: %v, want ErrNotFound", err)
	}
	if n := f.count("SELECT COUNT(*) FROM users WHERE anonymized_at IS NOT NULL"); n != 0 {
		t.Errorf("a refused call anonymized %d accounts", n)
	}
}

// The last-active-admin check runs after every statement of the operation, so
// it is also the proof that a failure leaves nothing half done: all of it must
// be rolled back.
func TestAnonymizeUserRollsBackEntirely(t *testing.T) {
	f := newAnonFixture(t) // root is the only admin
	ctx := f.ctx
	if err := f.s.CreateSession(ctx, f.root.ID, "tok-root", time.Hour); err != nil {
		t.Fatal(err)
	}
	k, _, err := f.s.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: "root laptop", UserID: f.root.ID,
		Scopes: store.DefaultGatewayScopes(), ProjectIDs: []int64{f.p1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.AddProjectMember(ctx, f.p1.ID, f.root.ID); err != nil {
		t.Fatal(err)
	}
	f.exec("INSERT INTO login_attempts (username, ip, success) VALUES ('root', '1.1.1.1', false)")
	f.audit(&f.root.ID, "root", "project.update", "project", &f.p1.ID, map[string]any{})
	f.audit(nil, "", "login.failure", "user", nil, map[string]any{"username": "Root"})
	f.audit(&f.root.ID, "root", "apikey.create", "api_key", &k.ID, map[string]any{"name": "root laptop", "user_id": f.root.ID})

	if _, err := f.s.AnonymizeUser(ctx, f.root.ID, "root", "h"); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("last active admin: %v, want ErrLastAdmin", err)
	}
	var (
		name   string
		active bool
	)
	if err := f.s.DB().QueryRow(ctx, "SELECT username, is_active FROM users WHERE id = $1", f.root.ID).Scan(&name, &active); err != nil {
		t.Fatal(err)
	}
	if name != "root" || !active {
		t.Errorf("user row changed: %q active=%v", name, active)
	}
	for q, want := range map[string]int64{
		"SELECT COUNT(*) FROM sessions":                                          1,
		"SELECT COUNT(*) FROM project_members":                                   1,
		"SELECT COUNT(*) FROM login_attempts":                                    1,
		"SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL":                 1,
		"SELECT COUNT(*) FROM api_keys WHERE name = 'root laptop'":               1,
		"SELECT COUNT(*) FROM audit_logs WHERE actor_username = 'root'":          2,
		"SELECT COUNT(*) FROM audit_logs WHERE details->>'username' = 'Root'":    1,
		"SELECT COUNT(*) FROM audit_logs WHERE details->>'name' = 'root laptop'": 1,
	} {
		if n := f.count(q); n != want {
			t.Errorf("%s = %d, want %d (rolled back)", q, n, want)
		}
	}
}

func TestAnonymizeEndUserAllProjectsAndOne(t *testing.T) {
	f := newAnonFixture(t)
	ctx := f.ctx
	// A table big enough for three batches in project 1.
	const big = 12001
	f.exec(`INSERT INTO request_logs (project_id, status_code, prompt_tokens, cost_micros, end_user)
		SELECT $1, 200, 3, 7, 'u-1' FROM generate_series(1, $2)`, f.p1.ID, big)
	f.exec(`INSERT INTO request_logs (project_id, status_code, prompt_tokens, cost_micros, end_user)
		SELECT $1, 200, 3, 7, 'u-1' FROM generate_series(1, 7)`, f.p2.ID)
	f.exec(`INSERT INTO request_logs (project_id, status_code, prompt_tokens, cost_micros, end_user)
		SELECT $1, 200, 3, 7, 'u-2' FROM generate_series(1, 5)`, f.p1.ID)
	f.exec(`INSERT INTO request_logs (project_id, status_code, prompt_tokens, cost_micros)
		SELECT $1, 200, 3, 7 FROM generate_series(1, 4)`, f.p1.ID)
	type totals struct{ n, prompt, cost int64 }
	read := func() totals {
		var x totals
		if err := f.s.DB().QueryRow(ctx, "SELECT COUNT(*), SUM(prompt_tokens), SUM(cost_micros) FROM request_logs").Scan(&x.n, &x.prompt, &x.cost); err != nil {
			t.Fatal(err)
		}
		return x
	}
	before := read()

	w := store.UsageWindows{Minute: time.Now().UTC().Truncate(time.Minute), Day: time.Now().UTC().Truncate(24 * time.Hour),
		Month: time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)}
	for _, pid := range []int64{f.p1.ID, f.p2.ID} {
		if err := f.s.RecordEndUserUsage(ctx, pid, "u-1", w, 100, 20, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.RecordEndUserUsage(ctx, f.p1.ID, "u-2", w, 1, 1, true); err != nil {
		t.Fatal(err)
	}
	usageBefore := f.count("SELECT COALESCE(SUM(prompt_tokens + completion_tokens + requests), 0) FROM end_user_usage")

	for _, pid := range []int64{f.p1.ID, f.p2.ID} {
		f.exec(`INSERT INTO alert_events (project_id, subject_kind, subject_id, limit_kind, threshold, window_start, limit_value)
			VALUES ($1, 'end_user', 'u-1', 'budget_daily_tokens', 80, now(), 100), ($1, 'key', 'u-1', 'budget_daily_tokens', 80, now(), 100)`, pid)
	}
	var endpoints = map[int64]int64{}
	for _, pid := range []int64{f.p1.ID, f.p2.ID} {
		ep, err := f.s.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: pid, Purpose: store.WebhookPurposeAlerts,
			URL: "https://hooks.example/a", Secret: secretA})
		if err != nil {
			t.Fatal(err)
		}
		endpoints[pid] = ep.ID
		for key, subj := range map[string]string{"a": `{"kind":"end_user","id":"u-1"}`, "b": `{"kind":"key","id":"u-1"}`} {
			body := `{"type":"limit.threshold","subject":` + subj + `}`
			if _, err := f.s.EnqueueWebhookDelivery(ctx, ep.ID, "limit.threshold", key, []byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Project 1 only.
	r1, err := f.s.AnonymizeEndUser(ctx, "u-1", &f.p1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !pseudonymRe.MatchString(r1.Pseudonym) {
		t.Fatalf("pseudonym %q", r1.Pseudonym)
	}
	if r1.RequestLogs != big || r1.EndUserUsage != 3 || r1.AlertEvents != 1 || r1.WebhookDeliveries != 1 {
		t.Errorf("project 1 counts: %+v", r1)
	}
	if n := f.count("SELECT COUNT(*) FROM request_logs WHERE project_id = $1 AND end_user = 'u-1'", f.p1.ID); n != 0 {
		t.Errorf("project 1 still has %d rows of u-1", n)
	}
	if n := f.count("SELECT COUNT(*) FROM request_logs WHERE project_id = $1 AND end_user = $2", f.p1.ID, r1.Pseudonym); n != big {
		t.Errorf("pseudonym rows in project 1 = %d, want %d", n, big)
	}
	if n := f.count("SELECT COUNT(*) FROM request_logs WHERE project_id = $1 AND end_user = 'u-1'", f.p2.ID); n != 7 {
		t.Errorf("project 2 was touched: %d rows of u-1 left, want 7", n)
	}
	if n := f.count("SELECT COUNT(*) FROM request_logs WHERE end_user = 'u-2'"); n != 5 {
		t.Errorf("another end user changed: %d", n)
	}

	// Every project.
	r2, err := f.s.AnonymizeEndUser(ctx, "u-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Pseudonym == r1.Pseudonym {
		t.Error("two operations shared a pseudonym")
	}
	if r2.RequestLogs != 7 || r2.EndUserUsage != 3 || r2.AlertEvents != 1 || r2.WebhookDeliveries != 1 {
		t.Errorf("all-project counts: %+v", r2)
	}
	for q, want := range map[string]int64{
		"SELECT COUNT(*) FROM request_logs WHERE end_user = 'u-1'":                                  0,
		"SELECT COUNT(*) FROM end_user_usage WHERE end_user = 'u-1'":                                0,
		"SELECT COUNT(*) FROM alert_events WHERE subject_id = 'u-1' AND subject_kind = 'end_user'":  0,
		"SELECT COUNT(*) FROM alert_events WHERE subject_id = 'u-1' AND subject_kind = 'key'":       2,
		"SELECT COUNT(*) FROM webhook_deliveries WHERE payload #>> '{subject,id}' = 'u-1'":          2, // the two kind=key rows
		"SELECT COUNT(*) FROM webhook_deliveries WHERE payload::text LIKE '%" + r1.Pseudonym + "%'": 1,
		"SELECT COUNT(*) FROM webhook_deliveries WHERE payload::text LIKE '%" + r2.Pseudonym + "%'": 1,
		"SELECT COUNT(*) FROM webhook_deliveries WHERE payload #>> '{type}' = 'limit.threshold'":    4,
	} {
		if n := f.count(q); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
	// Counts and sums never move.
	if after := read(); after != before {
		t.Errorf("request_logs totals changed: %+v -> %+v", before, after)
	}
	if after := f.count("SELECT COALESCE(SUM(prompt_tokens + completion_tokens + requests), 0) FROM end_user_usage"); after != usageBefore {
		t.Errorf("end_user_usage totals changed: %d -> %d", usageBefore, after)
	}
	if _, err := f.s.AnonymizeEndUser(ctx, "u-1", i64(987654)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project: %v, want ErrNotFound", err)
	}
}

// An anonymized account cannot be switched back on and its password cannot be
// replaced; an ordinary account is unaffected by the same calls.
func TestAnonymizedUserCannotBeReopened(t *testing.T) {
	f := newAnonFixture(t)
	ctx := f.ctx
	dan, err := f.s.CreateUser(ctx, "Dan", "h", "editor")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.UpdateUserPassword(ctx, dan.ID, "new-hash"); err != nil {
		t.Fatalf("password of an ordinary account: %v", err)
	}
	if _, err := f.s.AnonymizeUser(ctx, dan.ID, "Dan", "unusable-hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UpdateUser(ctx, dan.ID, "editor", true); !errors.Is(err, store.ErrAlreadyAnonymized) {
		t.Errorf("activate: %v, want ErrAlreadyAnonymized", err)
	}
	if err := f.s.UpdateUserPassword(ctx, dan.ID, "attacker-hash"); !errors.Is(err, store.ErrAlreadyAnonymized) {
		t.Errorf("password: %v, want ErrAlreadyAnonymized", err)
	}
	var hash string
	var active bool
	if err := f.s.DB().QueryRow(ctx, "SELECT password_hash, is_active FROM users WHERE id = $1", dan.ID).Scan(&hash, &active); err != nil {
		t.Fatal(err)
	}
	if hash != "unusable-hash" || active {
		t.Errorf("account changed: hash=%q active=%v", hash, active)
	}
	if u, err := f.s.UpdateUser(ctx, dan.ID, "viewer", false); err != nil || u.Role != "viewer" || u.IsActive {
		t.Errorf("a change that leaves it closed: %v %+v", err, u)
	}
}
