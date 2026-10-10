package store_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// An Azure OpenAI key lives in the same encrypted api_key field as every other
// provider key, so `ragmux rotate-key` (ADR-012) must carry it: sealed with
// the new SECRET_KEY, readable after a restart with it, and never stored in
// the clear.
func TestRotateKeyCoversAzureConnection(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	const key = "azure-key-0123456789abcdef"
	created, err := s.CreateConnection(ctx, &store.ModelConnection{
		Name: "azure", ProviderType: "azure_openai", BaseURL: "https://res.openai.azure.com?api-version=2024-10-21",
		ModelName: "my-gpt4o", APIKey: key})
	if err != nil {
		t.Fatal(err)
	}

	var raw []byte
	if err := s.DB().QueryRow(ctx, "SELECT api_key_enc FROM model_connections WHERE id = $1", created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || bytes.Contains(raw, []byte(key)) {
		t.Fatalf("stored credential is not sealed: %q", raw)
	}

	newKey := "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	if n, err := s.ReencryptConnections(ctx, newKey); err != nil || n != 1 {
		t.Fatalf("reencrypt: n=%d err=%v", n, err)
	}

	var rotated []byte
	if err := s.DB().QueryRow(ctx, "SELECT api_key_enc FROM model_connections WHERE id = $1", created.ID).Scan(&rotated); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(rotated, raw) {
		t.Error("ciphertext did not change on rotation")
	}

	cfg.SecretKeyHex = newKey
	s2 := testdb.OpenWith(t, cfg)
	got, err := s2.GetConnection(ctx, created.ID)
	if err != nil {
		t.Fatalf("read after rotation: %v", err)
	}
	if got.APIKey != key {
		t.Errorf("azure key after rotate-key = %q, want %q", got.APIKey, key)
	}
	if got.ProviderType != "azure_openai" || got.ModelName != "my-gpt4o" {
		t.Errorf("connection after rotation: %+v", got)
	}
}
