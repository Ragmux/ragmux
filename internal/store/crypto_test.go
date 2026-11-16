package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func legacyRow(t *testing.T, s *store.Store, name, key string) int64 {
	t.Helper()
	id, err := s.InsertLegacyConnection(context.Background(), name, key)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func rowVersion(t *testing.T, s *store.Store, id int64) int16 {
	t.Helper()
	var v int16
	if err := s.DB().QueryRow(context.Background(), "SELECT key_version FROM model_connections WHERE id = $1", id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestConnectionKeyBoundToRow(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	a, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "a", ProviderType: "openai", ModelName: "m", APIKey: "sk-aaaa-11111111"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "b", ProviderType: "openai", ModelName: "m", APIKey: "sk-bbbb-22222222"})
	if err != nil {
		t.Fatal(err)
	}
	if rowVersion(t, s, a.ID) != store.KeyVersionBound || rowVersion(t, s, b.ID) != store.KeyVersionBound {
		t.Fatal("new rows must be written at the bound key version")
	}
	// Swap the two ciphertexts as someone with database write access could.
	if _, err := s.DB().Exec(ctx, `UPDATE model_connections m SET api_key_enc = o.api_key_enc
		FROM (SELECT id, api_key_enc FROM model_connections) o
		WHERE (m.id = $1 AND o.id = $2) OR (m.id = $2 AND o.id = $1)`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{a.ID, b.ID} {
		if _, err := s.GetConnection(ctx, id); err == nil || !strings.Contains(err.Error(), "decrypt api key") {
			t.Errorf("connection %d: swapped blob should not decrypt, got %v", id, err)
		}
	}
	if _, err := s.ListConnections(ctx); err == nil {
		t.Error("listing must fail while a blob is on the wrong row")
	}
	// A new key written through UpdateConnection repairs the row.
	if _, err := s.UpdateConnection(ctx, &store.ModelConnection{ID: a.ID, Name: "a", ProviderType: "openai", ModelName: "m", APIKey: "sk-aaaa-33333333"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetConnection(ctx, a.ID); err != nil || got.APIKey != "sk-aaaa-33333333" {
		t.Errorf("after update: %v %+v", err, got)
	}
}

func TestLegacyKeysDecryptAndUpgrade(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	legacy := legacyRow(t, s, "old", "sk-legacy-12345678")
	empty := legacyRow(t, s, "old-empty", "")
	fresh, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "new", ProviderType: "openai", ModelName: "m", APIKey: "sk-new-123456789"})
	if err != nil {
		t.Fatal(err)
	}
	// Version 0 rows still read with nil associated data.
	if got, err := s.GetConnection(ctx, legacy); err != nil || got.APIKey != "sk-legacy-12345678" {
		t.Fatalf("legacy row: %v %+v", err, got)
	}
	// The startup step re-seals exactly the version 0 rows, once.
	n, err := s.UpgradeConnectionKeys(ctx)
	if err != nil || n != 2 {
		t.Fatalf("upgrade: n=%d err=%v", n, err)
	}
	for _, id := range []int64{legacy, empty, fresh.ID} {
		if v := rowVersion(t, s, id); v != store.KeyVersionBound {
			t.Errorf("connection %d: key_version = %d after upgrade", id, v)
		}
	}
	if n, err := s.UpgradeConnectionKeys(ctx); err != nil || n != 0 {
		t.Errorf("second upgrade: n=%d err=%v", n, err)
	}
	s.Close()
	// After a restart the upgraded rows decrypt with the same key.
	s2 := testdb.OpenWith(t, cfg)
	list, err := s2.ListConnections(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("list after reopen: %v %d", err, len(list))
	}
	got := map[string]string{}
	for _, c := range list {
		got[c.Name] = c.APIKey
	}
	if got["old"] != "sk-legacy-12345678" || got["old-empty"] != "" || got["new"] != "sk-new-123456789" {
		t.Errorf("keys after upgrade: %v", got)
	}
}

func TestRotateKeyWritesBoundVersion(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	legacy := legacyRow(t, s, "old", "sk-legacy-12345678")
	newKey := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	if n, err := s.ReencryptConnections(ctx, newKey); err != nil || n != 1 {
		t.Fatalf("reencrypt: n=%d err=%v", n, err)
	}
	if v := rowVersion(t, s, legacy); v != store.KeyVersionBound {
		t.Errorf("key_version after rotate = %d", v)
	}
	cfg.SecretKeyHex = newKey
	s2 := testdb.OpenWith(t, cfg)
	if got, err := s2.GetConnection(ctx, legacy); err != nil || got.APIKey != "sk-legacy-12345678" {
		t.Errorf("after rotation: %v %+v", err, got)
	}
	if n, err := s2.UpgradeConnectionKeys(ctx); err != nil || n != 0 {
		t.Errorf("nothing left to upgrade after rotate-key: n=%d err=%v", n, err)
	}
}

// A key opened from Vault or KMS is the same 32 bytes as any other, so the
// store works exactly as before; only the name it reports differs.
func TestOpenConfigCarriesTheKeySource(t *testing.T) {
	for _, source := range []string{"vault_transit", "aws_kms"} {
		cfg := testdb.Config(t)
		cfg.SecretKeySource = source
		s := testdb.OpenWith(t, cfg)
		if s.SecretKeySource != source {
			t.Errorf("SecretKeySource = %q, want %q", s.SecretKeySource, source)
		}
		if _, err := s.CreateConnection(context.Background(), &store.ModelConnection{Name: "c", ProviderType: "openai", APIKey: "sk-remote-123456", ModelName: "m"}); err != nil {
			t.Errorf("%s: %v", source, err)
		}
		s.Close()
		// A restart from the same remote key finds the same canary.
		_ = testdb.OpenWith(t, cfg)
	}

	// Left empty, the historical names stand.
	cfg := testdb.Config(t)
	if s := testdb.OpenWith(t, cfg); s.SecretKeySource != "env" {
		t.Errorf("default source = %q, want env", s.SecretKeySource)
	}
	cfg = testdb.Config(t)
	cfg.SecretKeySource = "env"
	cfg.SecretKeyHex = ""
	if s := testdb.OpenWith(t, cfg); s.SecretKeySource != "file" {
		t.Errorf("env source without a key = %q, want file (the secret.key fallback)", s.SecretKeySource)
	}
}

// The secret.key fallback is for the env source only. A remote source that
// handed over no key must stop the open: minting a key of its own would seal
// data nobody else can read. The error comes before the database is touched.
func TestRemoteKeySourceNeverFallsBackToTheKeyFile(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, source := range []string{"vault_transit", "aws_kms"} {
		dir := t.TempDir()
		_, err := store.Open(context.Background(), store.OpenConfig{
			DatabaseURL: "postgres://nobody@127.0.0.1:1/none", DataDir: dir, SecretKeySource: source,
		}, quiet)
		if err == nil || !strings.Contains(err.Error(), "refusing to fall back to secret.key") {
			t.Errorf("%s: error = %v", source, err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "secret.key")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s: secret.key was created (%v)", source, statErr)
		}
	}

	_, err := store.Open(context.Background(), store.OpenConfig{
		DatabaseURL: "postgres://nobody@127.0.0.1:1/none", SecretKeyHex: strings.Repeat("ab", 32), SecretKeySource: "gcp_kms",
	}, quiet)
	if err == nil || !strings.Contains(err.Error(), "unknown secret key source") {
		t.Errorf("unknown source: %v", err)
	}
	_, err = store.Open(context.Background(), store.OpenConfig{
		DatabaseURL: "postgres://nobody@127.0.0.1:1/none", SecretKeyHex: "abcd", SecretKeySource: "aws_kms",
	}, quiet)
	if err == nil || !strings.Contains(err.Error(), "not 32 bytes") {
		t.Errorf("short remote key: %v", err)
	}
}

// A database with no connection but a sealed TOTP secret (and no canary) must
// still refuse a key that cannot open that secret, instead of adopting it.
func TestCanaryPreconditionReadsTheSealedColumns(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	u, err := s.CreateUser(ctx, "ops", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetTOTPSecret(ctx, u.ID, "JBSWY3DPEHPK3PXP"); err != nil || !ok {
		t.Fatalf("SetTOTPSecret: %v %v", ok, err)
	}
	var n int
	if err := s.DB().QueryRow(ctx, "SELECT count(*) FROM model_connections").Scan(&n); err != nil || n != 0 {
		t.Fatalf("this test needs a database with no connection: %d %v", n, err)
	}
	if _, err := s.DB().Exec(ctx, "DELETE FROM instance_settings WHERE key = 'secret_key_canary'"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	wrong := cfg
	wrong.SecretKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err = store.Open(ctx, wrong, quiet)
	if !errors.Is(err, store.ErrSecretKeyMismatch) {
		t.Fatalf("the wrong key was adopted over a sealed TOTP secret: %v", err)
	}
	if !strings.Contains(err.Error(), "users.totp_secret_enc") {
		t.Errorf("the error does not say which value failed: %v", err)
	}

	// Nothing was recorded, so the right key still opens it.
	s2, err := store.Open(ctx, cfg, quiet)
	if err != nil {
		t.Fatalf("the original key was locked out: %v", err)
	}
	s2.Close()
}
