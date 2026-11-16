package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/config"
	"github.com/ragmux/ragmux/internal/keysource"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// These tests prove the production wiring of SECRET_KEY_SOURCE (ADR-038): the
// gateway, reset-password and rotate-key all obtain the instance key through
// resolveSecretKey. A command that did not would reach the store with an empty
// key, take the secret.key fallback, mint a key in the data directory and stop
// at the canary -- or, worse, on a fresh database, adopt it.

// fakeVault is a TLS Vault Transit that maps ciphertexts to plain keys.
type fakeVault struct {
	srv    *httptest.Server
	calls  atomic.Int32
	denied atomic.Bool
	caFile string
	keys   map[string][]byte // wrapped -> plaintext
}

func startVault(t *testing.T, keys map[string][]byte) *fakeVault {
	t.Helper()
	v := &fakeVault{keys: keys}
	v.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.calls.Add(1)
		if v.denied.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var in struct {
			Ciphertext string `json:"ciphertext"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		plain, ok := v.keys[in.Ciphertext]
		if r.URL.Path != "/v1/transit/decrypt/ragmux" || r.Header.Get("X-Vault-Token") != "test-token" || !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": base64.StdEncoding.EncodeToString(plain)}})
	}))
	t.Cleanup(v.srv.Close)
	v.caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(v.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: v.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return v
}

// setKeyEnv points the process environment at v the way an operator's would:
// no SECRET_KEY at all, the wrapped key and the Vault settings instead.
func setKeyEnv(t *testing.T, v *fakeVault, wrapped, dataDir string) {
	t.Helper()
	for k, val := range map[string]string{
		"SECRET_KEY": "", "SECRET_KEY_FILE": "", "SECRET_KEY_WRAPPED_FILE": "",
		"SECRET_KEY_SOURCE": "vault_transit", "SECRET_KEY_WRAPPED": wrapped,
		"VAULT_ADDR": v.srv.URL, "VAULT_TRANSIT_KEY": "ragmux", "VAULT_TRANSIT_MOUNT": "", "VAULT_NAMESPACE": "",
		"VAULT_TOKEN": "test-token", "VAULT_TOKEN_FILE": "", "VAULT_CACERT": v.caFile,
		"DATA_DIR": dataDir,
	} {
		t.Setenv(k, val)
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func noKeyFile(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); err == nil {
		t.Error("secret.key was created in the data directory")
	}
}

func loadConfig(t *testing.T, dbURL string) config.Config {
	t.Helper()
	t.Setenv("DATABASE_URL", dbURL)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestResolveSecretKeyKeepsTheEnvPaths(t *testing.T) {
	key := strings.Repeat("ab", 32)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"SECRET_KEY_SOURCE": "", "SECRET_KEY_WRAPPED": "", "SECRET_KEY_WRAPPED_FILE": ""} {
		t.Setenv(k, v)
	}

	// SECRET_KEY
	t.Setenv("SECRET_KEY_FILE", "")
	t.Setenv("SECRET_KEY", key)
	got, source, err := resolveSecretKey(context.Background(), loadConfig(t, "postgres://x/y"), quietLog())
	if err != nil || got != key || source != "" {
		t.Errorf("SECRET_KEY: %q %q %v", got, source, err)
	}
	// SECRET_KEY_FILE
	t.Setenv("SECRET_KEY", "")
	t.Setenv("SECRET_KEY_FILE", keyFile)
	got, source, err = resolveSecretKey(context.Background(), loadConfig(t, "postgres://x/y"), quietLog())
	if err != nil || got != key || source != "" {
		t.Errorf("SECRET_KEY_FILE: %q %q %v", got, source, err)
	}
	// Neither: empty, so the store takes the secret.key fallback as before.
	t.Setenv("SECRET_KEY_FILE", "")
	got, source, err = resolveSecretKey(context.Background(), loadConfig(t, "postgres://x/y"), quietLog())
	if err != nil || got != "" || source != "" {
		t.Errorf("fallback: %q %q %v", got, source, err)
	}
}

func TestResolveSecretKeyOpensTheWrappedKeyFromVault(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	v := startVault(t, map[string][]byte{"vault:v1:wrapped": key})
	dataDir := t.TempDir()
	setKeyEnv(t, v, "vault:v1:wrapped", dataDir)

	got, source, err := resolveSecretKey(context.Background(), loadConfig(t, "postgres://x/y"), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(key) || source != "vault_transit" {
		t.Errorf("key %q source %q", got, source)
	}
	noKeyFile(t, dataDir)
	if v.calls.Load() != 1 {
		t.Errorf("%d Vault calls, want exactly one", v.calls.Load())
	}
}

// Whatever goes wrong at Vault, the answer is an error and no key; nothing
// falls back to a file in the data directory.
func TestResolveSecretKeyFailsClosed(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)

	t.Run("denied", func(t *testing.T) {
		v := startVault(t, map[string][]byte{"vault:v1:wrapped": key})
		v.denied.Store(true)
		dataDir := t.TempDir()
		setKeyEnv(t, v, "vault:v1:wrapped", dataDir)
		// A denial is retried like any other failure (one and three seconds
		// apart); a short deadline ends the wait without a slow test. The
		// class-by-class retry counts are pinned in internal/keysource.
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		got, _, err := resolveSecretKey(ctx, loadConfig(t, "postgres://x/y"), quietLog())
		if err == nil || got != "" || v.calls.Load() < 1 {
			t.Errorf("key %q, error %v, %d Vault calls", got, err, v.calls.Load())
		}
		noKeyFile(t, dataDir)
	})

	t.Run("unreachable", func(t *testing.T) {
		v := startVault(t, map[string][]byte{"vault:v1:wrapped": key})
		dataDir := t.TempDir()
		setKeyEnv(t, v, "vault:v1:wrapped", dataDir)
		cfg := loadConfig(t, "postgres://x/y")
		v.srv.Close()
		// The retry waits are one and three seconds; a short deadline ends
		// the wait without making the test slow.
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		got, source, err := resolveSecretKey(ctx, cfg, quietLog())
		if err == nil || got != "" || source != "" {
			t.Errorf("key %q source %q error %v", got, source, err)
		}
		noKeyFile(t, dataDir)
	})

	t.Run("not a 32 byte key", func(t *testing.T) {
		v := startVault(t, map[string][]byte{"vault:v1:wrapped": key[:16]})
		dataDir := t.TempDir()
		setKeyEnv(t, v, "vault:v1:wrapped", dataDir)
		_, _, err := resolveSecretKey(context.Background(), loadConfig(t, "postgres://x/y"), quietLog())
		if keysource.ClassOf(err) != keysource.ClassMalformed {
			t.Errorf("error = %v", err)
		}
		noKeyFile(t, dataDir)
	})
}

// run() cannot be started in a test, so this reads the three production call
// sites: each hands store.Open a key from resolveSecretKey, and none reads
// cfg.SecretKeyHex into the OpenConfig directly (which would skip the source).
func TestEveryStoreOpenGoesThroughResolveSecretKey(t *testing.T) {
	for _, file := range []string{"main.go", "rotate.go", "resetpw.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		resolves, opens := 0, 0
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "resolveSecretKey" {
					resolves++
				}
			case *ast.CompositeLit:
				sel, ok := x.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "OpenConfig" {
					return true
				}
				opens++
				for _, el := range x.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "SecretKeyHex" {
						if v, ok := kv.Value.(*ast.SelectorExpr); ok && v.Sel.Name == "SecretKeyHex" {
							t.Errorf("%s: store.OpenConfig takes cfg.SecretKeyHex directly", file)
						}
					}
				}
			}
			return true
		})
		if opens != 1 || resolves < 1 {
			t.Errorf("%s: %d store.OpenConfig literal(s), %d resolveSecretKey call(s)", file, opens, resolves)
		}
	}
}

// vaultInstance is a database written under a Vault-sourced key: a provider
// credential, a webhook secret and a TOTP secret, so a rotation has every kind
// of sealed value to move.
type vaultInstance struct {
	cfg  store.OpenConfig
	user *store.User
}

func newVaultInstance(t *testing.T, key []byte) vaultInstance {
	t.Helper()
	ctx := context.Background()
	cfg := testdb.Config(t)
	cfg.SecretKeyHex = hex.EncodeToString(key)
	cfg.SecretKeySource = "vault_transit"
	cfg.DataDir = t.TempDir()
	s := testdb.OpenWith(t, cfg)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", APIKey: "sk-vault-123456", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: p.ID, Purpose: store.WebhookPurposeAlerts,
		URL: "https://hooks.example/a", Secret: "whsec_" + strings.Repeat("QUJD", 11)}); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(ctx, "Ops", "old-hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetTOTPSecret(ctx, u.ID, "JBSWY3DPEHPK3PXP"); err != nil || !ok {
		t.Fatalf("SetTOTPSecret: %v %v", ok, err)
	}
	s.Close()
	return vaultInstance{cfg: cfg, user: u}
}

func stdinOf(t *testing.T, text string) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString(text)
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	return func() { os.Stdin = old; _ = r.Close() }
}

func TestResetPasswordOpensTheKeyFromVault(t *testing.T) {
	ctx := context.Background()
	key := bytes.Repeat([]byte{0x11}, 32)
	inst := newVaultInstance(t, key)
	v := startVault(t, map[string][]byte{"vault:v1:wrapped": key})
	dataDir := t.TempDir()
	setKeyEnv(t, v, "vault:v1:wrapped", dataDir)
	t.Setenv("DATABASE_URL", inst.cfg.DatabaseURL)

	// --disable-totp is the way out of a second-factor lock; it must work
	// under a Vault key too, with no SECRET_KEY and no secret.key.
	restore := stdinOf(t, "a-good-long-password\n")
	code := resetPassword([]string{"ops", "--stdin", "--disable-totp"})
	restore()
	if code != 0 {
		t.Fatalf("reset-password exit %d", code)
	}
	noKeyFile(t, dataDir)
	if v.calls.Load() != 1 {
		t.Errorf("%d Vault calls, want 1", v.calls.Load())
	}
	s := testdb.OpenWith(t, inst.cfg)
	u, err := s.GetUser(ctx, inst.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(u.PasswordHash, "a-good-long-password") {
		t.Error("password was not changed")
	}
	if st, err := s.GetTOTPStatus(ctx, u.ID); err != nil || st.Enabled || st.HasSecret {
		t.Errorf("TOTP status after --disable-totp = %+v, %v", st, err)
	}

	// Vault down: exit 1, nothing changed, still no key file.
	v.denied.Store(true)
	restore = stdinOf(t, "another-long-password\n")
	code = resetPassword([]string{"ops", "--stdin"})
	restore()
	if code != 1 {
		t.Errorf("reset-password with Vault denying: exit %d, want 1", code)
	}
	noKeyFile(t, dataDir)
	if u2, _ := s.GetUser(ctx, u.ID); !auth.CheckPassword(u2.PasswordHash, "a-good-long-password") {
		t.Error("the password changed although the key could not be opened")
	}
}

// captureStderr runs f and returns what it wrote to os.Stderr.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	os.Stderr = old
	_ = w.Close()
	return <-done
}

// sealedSnapshot is every sealed byte of the instance as stored: the provider
// credential, the webhook secret, the TOTP secret and the canary. Two equal
// snapshots mean a command changed none of them.
func sealedSnapshot(t *testing.T, cfg store.OpenConfig) string {
	t.Helper()
	s := testdb.OpenWith(t, cfg)
	defer s.Close()
	var out []string
	for _, q := range []string{
		"SELECT coalesce(string_agg(encode(api_key_enc, 'hex'), ',' ORDER BY id), '') FROM model_connections",
		"SELECT coalesce(string_agg(encode(secret_enc, 'hex'), ',' ORDER BY id), '') FROM webhook_endpoints",
		"SELECT coalesce(string_agg(encode(totp_secret_enc, 'hex'), ',' ORDER BY id), '') FROM users",
		"SELECT coalesce(string_agg(encode(value, 'hex'), ',' ORDER BY key), '') FROM instance_settings WHERE key = 'secret_key_canary'",
	} {
		var v string
		if err := s.DB().QueryRow(context.Background(), q).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v == "" {
			t.Fatalf("snapshot query found nothing: %s", q)
		}
		out = append(out, v)
	}
	return strings.Join(out, "|")
}

func TestRotateKeyWithVaultSource(t *testing.T) {
	ctx := context.Background()
	old := bytes.Repeat([]byte{0x11}, 32)
	next := bytes.Repeat([]byte{0x22}, 32)
	third := bytes.Repeat([]byte{0x33}, 32)
	inst := newVaultInstance(t, old)
	v := startVault(t, map[string][]byte{
		"vault:v1:old": old, "vault:v1:next": next, "vault:v1:same-as-old": old, "vault:v1:short": next[:16],
	})
	dataDir := t.TempDir()
	setKeyEnv(t, v, "vault:v1:old", dataDir)
	t.Setenv("DATABASE_URL", inst.cfg.DatabaseURL)
	before := sealedSnapshot(t, inst.cfg)

	// Bad arguments and a new key equal to the current one are usage errors.
	for name, args := range map[string][]string{
		"neither":         {},
		"both":            {"--new", hex.EncodeToString(next), "--new-wrapped", "vault:v1:next"},
		"short hex":       {"--new", "abcd"},
		"same as wrapped": {"--new-wrapped", "vault:v1:same-as-old"},
	} {
		if got := rotateKey(args); got != 2 {
			t.Errorf("%s: exit %d, want 2", name, got)
		}
	}

	// ADR-038: under a remote source --new is refused before anything is read
	// or written. A key given as hex is stored nowhere, so the rotation would
	// leave a database no gateway could start from. The message points at the
	// way out and does not repeat the key.
	calls := v.calls.Load()
	var code int
	stderr := captureStderr(t, func() { code = rotateKey([]string{"--new", hex.EncodeToString(next)}) })
	if code != 2 {
		t.Errorf("--new under vault_transit: exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "--new-wrapped") || !strings.Contains(stderr, "vault_transit") || strings.Contains(stderr, hex.EncodeToString(next)) {
		t.Errorf("stderr = %q", stderr)
	}
	if v.calls.Load() != calls {
		t.Error("Vault was called although --new was refused")
	}
	if got := sealedSnapshot(t, inst.cfg); got != before {
		t.Error("a row changed although --new was refused")
	}

	// A new blob Vault cannot open, or opens to the wrong size: exit 1 and
	// the database is byte for byte what it was.
	for name, blob := range map[string]string{"unknown blob": "vault:v1:unknown", "16 byte key": "vault:v1:short"} {
		if got := rotateKey([]string{"--new-wrapped", blob}); got != 1 {
			t.Errorf("%s: exit %d, want 1", name, got)
		}
		if got := sealedSnapshot(t, inst.cfg); got != before {
			t.Errorf("%s: a row changed", name)
		}
	}
	v.denied.Store(true)
	if got := rotateKey([]string{"--new-wrapped", "vault:v1:next"}); got != 1 {
		t.Errorf("Vault denying: exit %d, want 1", got)
	}
	v.denied.Store(false)
	if got := sealedSnapshot(t, inst.cfg); got != before {
		t.Error("a row changed although Vault denied the key")
	}

	// --new-wrapped: both keys come from Vault, the same source.
	if got := rotateKey([]string{"--new-wrapped", "vault:v1:next"}); got != 0 {
		t.Fatalf("rotate-key --new-wrapped: exit %d", got)
	}
	noKeyFile(t, dataDir)
	cfgNext := inst.cfg
	cfgNext.SecretKeyHex = hex.EncodeToString(next)
	s := testdb.OpenWith(t, cfgNext) // fatals if the canary was not re-sealed
	if _, err := store.Open(ctx, inst.cfg, quietLog()); err == nil {
		t.Error("the old key still opens the database after the rotation")
	}

	// Every kind of sealed value moved: a rotation away from `next` can only
	// decrypt them with `next`, and reports one row of each.
	rep, err := s.ReencryptSealed(ctx, hex.EncodeToString(third))
	if err != nil {
		t.Fatalf("a sealed value did not open with the new key: %v", err)
	}
	rows := map[string]int{}
	for _, c := range rep.Columns {
		rows[c.Table+"."+c.Column] = c.Rows
	}
	if rep.Connections != 1 || rows["webhook_endpoints.secret_enc"] != 1 || rows["users.totp_secret_enc"] != 1 {
		t.Errorf("rotated rows: connections %d, columns %v", rep.Connections, rows)
	}
	s.Close()
	noKeyFile(t, dataDir)

	// The env source keeps --new exactly as it was: silent, and it rotates.
	// The database holds `third` now.
	t.Setenv("SECRET_KEY_SOURCE", "")
	t.Setenv("SECRET_KEY_WRAPPED", "")
	t.Setenv("SECRET_KEY", hex.EncodeToString(third))
	fourth := bytes.Repeat([]byte{0x44}, 32)
	stderr = captureStderr(t, func() { code = rotateKey([]string{"--new", hex.EncodeToString(fourth)}) })
	if code != 0 || strings.Contains(stderr, "--new-wrapped") {
		t.Errorf("env source: exit %d, stderr %q", code, stderr)
	}
	cfgFourth := inst.cfg
	cfgFourth.SecretKeyHex = hex.EncodeToString(fourth)
	s4 := testdb.OpenWith(t, cfgFourth) // fatals if the canary was not re-sealed
	rep, err = s4.ReencryptSealed(ctx, hex.EncodeToString(bytes.Repeat([]byte{0x55}, 32)))
	if err != nil {
		t.Fatalf("a sealed value did not open with the --new key: %v", err)
	}
	rows = map[string]int{}
	for _, c := range rep.Columns {
		rows[c.Table+"."+c.Column] = c.Rows
	}
	if rep.Connections != 1 || rows["webhook_endpoints.secret_enc"] != 1 || rows["users.totp_secret_enc"] != 1 {
		t.Errorf("rotated rows after env --new: connections %d, columns %v", rep.Connections, rows)
	}
	s4.Close()
}

// The same refusal for AWS KMS, with nothing reachable: it must come before any
// network or database access, so no credentials and no endpoint are needed.
func TestRotateKeyNewIsRefusedUnderAWSKMS(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nobody@127.0.0.1:1/none")
	for k, v := range map[string]string{
		"SECRET_KEY": "", "SECRET_KEY_FILE": "", "SECRET_KEY_WRAPPED_FILE": "", "SECRET_KEY_SOURCE": "aws_kms",
		"SECRET_KEY_WRAPPED": "AAAA", "AWS_REGION": "eu-west-1", "AWS_ACCESS_KEY_ID": "", "AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(k, v)
	}
	var code int
	stderr := captureStderr(t, func() { code = rotateKey([]string{"--new", strings.Repeat("ab", 32)}) })
	if code != 2 || !strings.Contains(stderr, "--new-wrapped") || !strings.Contains(stderr, "aws_kms") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestRotateKeyNewWrappedNeedsARemoteSource(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x/y")
	for k, v := range map[string]string{"SECRET_KEY": strings.Repeat("ab", 32), "SECRET_KEY_SOURCE": "", "SECRET_KEY_WRAPPED": "", "SECRET_KEY_FILE": ""} {
		t.Setenv(k, v)
	}
	if got := rotateKey([]string{"--new-wrapped", "vault:v1:x"}); got != 2 {
		t.Errorf("exit %d, want 2", got)
	}
}
