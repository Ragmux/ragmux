package store_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

const rotatedKey = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// A rotation moves the provider keys, the canary and every registered sealed
// column together; the restart with the new key then opens all of them.
func TestReencryptSealedRotatesConnectionsCanaryAndWebhookSecrets(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)

	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", APIKey: "sk-one", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := s.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: p.ID, Purpose: store.WebhookPurposeAlerts, URL: "https://hooks.example/a", Secret: secretA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateWebhookSecret(ctx, alerts.ID, secretB, 0); err != nil { // fills prev_secret_enc
		t.Fatal(err)
	}
	// A second endpoint with no previous secret (NULL) must be skipped, not fail.
	guard, err := s.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: p.ID, Purpose: store.WebhookPurposeGuardrail, URL: "https://hooks.example/g", Secret: secretA})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.ReencryptSealed(ctx, "not-hex"); err == nil {
		t.Error("a bad key was accepted")
	}
	rep, err := s.ReencryptSealed(ctx, rotatedKey)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Connections != 1 {
		t.Errorf("connections = %d, want 1", rep.Connections)
	}
	rows := map[string]int{}
	for _, c := range rep.Columns {
		rows[c.Table+"."+c.Column] = c.Rows
	}
	if rows["webhook_endpoints.secret_enc"] != 2 || rows["webhook_endpoints.prev_secret_enc"] != 1 {
		t.Errorf("columns = %v, want secret_enc 2 and prev_secret_enc 1", rows)
	}

	// The old key no longer opens the webhook secrets ...
	if _, err := s.GetWebhookEndpointByID(ctx, alerts.ID); err == nil {
		t.Error("the old key still opens a rotated webhook secret")
	}
	// ... and the new key opens everything, the canary included (OpenWith
	// fails the test when the canary was left sealed with the old key).
	cfg.SecretKeyHex = rotatedKey
	s2 := testdb.OpenWith(t, cfg)
	got, err := s2.GetWebhookEndpointByID(ctx, alerts.ID)
	if err != nil || got.Secret != secretB || got.PrevSecret != secretA {
		t.Fatalf("alerts after rotation: %+v, %v", got, err)
	}
	if got, err := s2.GetWebhookEndpointByID(ctx, guard.ID); err != nil || got.Secret != secretA || got.PrevSecret != "" {
		t.Fatalf("guardrail after rotation: %+v, %v", got, err)
	}
	list, err := s2.ListConnections(ctx)
	if err != nil || len(list) != 1 || list[0].APIKey != "sk-one" {
		t.Fatalf("connections after rotation: %+v, %v", list, err)
	}
	// Still bound to the row: the rotation kept the AAD.
	var raw []byte
	if err := s2.DB().QueryRow(ctx, "SELECT secret_enc FROM webhook_endpoints WHERE id = $1", alerts.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.DB().Exec(ctx, "UPDATE webhook_endpoints SET secret_enc = $1 WHERE id = $2", raw, guard.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.GetWebhookEndpointByID(ctx, guard.ID); err == nil {
		t.Error("a rotated secret opened on another row")
	}
}

// One column that cannot be re-sealed aborts the whole rotation: nothing else
// is rewritten, the canary stays on the old key.
func TestReencryptSealedIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	if _, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", APIKey: "sk-one", ModelName: "m"}); err != nil {
		t.Fatal(err)
	}
	conn, _ := s.ListConnections(ctx)
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := s.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: p.ID, Purpose: store.WebhookPurposeAlerts, URL: "https://hooks.example/a", Secret: secretA})
	if err != nil {
		t.Fatal(err)
	}
	canary := func() []byte {
		var b []byte
		if err := s.DB().QueryRow(ctx, "SELECT value FROM instance_settings WHERE key = 'secret_key_canary'").Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	canaryBefore := canary()
	var connBefore []byte
	if err := s.DB().QueryRow(ctx, "SELECT api_key_enc FROM model_connections").Scan(&connBefore); err != nil {
		t.Fatal(err)
	}
	// Garbage that cannot be opened with the current key.
	if _, err := s.DB().Exec(ctx, "UPDATE webhook_endpoints SET prev_secret_enc = $1 WHERE id = $2", bytes.Repeat([]byte{7}, 64), ep.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ReencryptSealed(ctx, rotatedKey); err == nil {
		t.Fatal("a rotation over an unreadable secret succeeded")
	}
	var connAfter []byte
	if err := s.DB().QueryRow(ctx, "SELECT api_key_enc FROM model_connections").Scan(&connAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(connBefore, connAfter) {
		t.Error("the connection key was rewritten although the rotation failed")
	}
	if !bytes.Equal(canaryBefore, canary()) {
		t.Error("the canary was rewritten although the rotation failed")
	}
}
