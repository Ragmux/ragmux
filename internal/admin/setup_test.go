package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/testdb"
)

// The key source an instance runs with (ADR-038) is reported by both the
// setup status and /api/system, and both read Store.SecretKeySource, so a key
// opened from Vault or KMS shows up under its own name with no handler change.
func TestSecretKeySourceIsReported(t *testing.T) {
	for _, source := range []string{"env", "vault_transit", "aws_kms"} {
		ctx := context.Background()
		cfg := testdb.Config(t)
		if source != "env" {
			cfg.SecretKeySource = source
		}
		st := testdb.OpenWith(t, cfg)
		adm := &Admin{Store: st, Auth: &auth.Service{Store: st, TTL: time.Hour},
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		r := chi.NewRouter()
		r.Route("/admin", adm.Routes)
		srv := httptest.NewServer(r)

		get := func(path, bearer string) map[string]any {
			t.Helper()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/admin"+path, nil)
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: status %d", source, path, resp.StatusCode)
			}
			var out map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			return out
		}
		if out := get("/api/setup", ""); out["secret_key_source"] != source {
			t.Errorf("/api/setup secret_key_source = %v, want %s", out["secret_key_source"], source)
		}

		u, err := st.CreateUser(ctx, "root", "h", "admin")
		if err != nil {
			t.Fatal(err)
		}
		tok, err := adm.Auth.NewSession(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if out := get("/api/system", tok); out["secret_key_source"] != source {
			t.Errorf("/api/system secret_key_source = %v, want %s", out["secret_key_source"], source)
		}
		srv.Close()
	}
}
