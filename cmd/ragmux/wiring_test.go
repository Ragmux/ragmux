package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/config"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/webhook"
)

// The guardrail webhook only works in production if main hands the gateway a
// client built from the process's configuration. The gateway tests build their
// own client and the e2e test builds the Gateway by hand, so neither would
// notice a missing wire: a project that fails closed would answer every
// request 503. These tests are the proof (ADR-033, ADR-034).

// hookCall sends one signed guardrail-shaped POST through client to rawURL.
func hookCall(client *webhook.Client, rawURL string) (webhook.Response, error) {
	return client.Call(context.Background(), webhook.Request{
		Purpose: obs.WebhookPurposeGuardrail, URL: rawURL, EventID: "evt_test", Body: []byte(`{"texts":[]}`),
		Secrets: []string{"whsec_" + strings.Repeat("QUJD", 11)}, Timeout: 5 * time.Second, MaxResponseBytes: 1 << 10,
	})
}

func blocked(err error) bool {
	var ce *webhook.CallError
	return errors.As(err, &ce) && ce.Class == store.DeliveryErrBlocked
}

// loopbackHook is a receiver on 127.0.0.1 that answers allow and remembers the
// headers it was sent.
func loopbackHook(t *testing.T) (*httptest.Server, *http.Header) {
	t.Helper()
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"action":"allow"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestNewWebhookClientHonoursThePrivateWebhookAllowlist(t *testing.T) {
	srv, seen := loopbackHook(t)
	u, _ := url.Parse(srv.URL)
	host := u.Hostname()

	listed := newWebhookClient(config.Config{PrivateWebhookAllowlist: map[string]bool{host: true}})
	resp, err := hookCall(listed, srv.URL)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), "allow") {
		t.Fatalf("a host on PRIVATE_WEBHOOK_ALLOWLIST must be reachable: %v %+v", err, resp)
	}
	if seen.Get("webhook-signature") == "" || seen.Get("webhook-id") != "evt_test" || !strings.HasPrefix(seen.Get("User-Agent"), "ragmux/") {
		t.Errorf("the receiver saw %v, want a signed, identified request", *seen)
	}

	for name, cfg := range map[string]config.Config{
		"empty list": {PrivateWebhookAllowlist: map[string]bool{}},
		"nil list":   {},
		// PRIVATE_UPSTREAM_ALLOWLIST and ALLOW_PRIVATE_UPSTREAMS are for provider
		// URLs; they must not open a path to a URL an editor typed.
		"upstream list only": {PrivateUpstreamAllowlist: map[string]bool{host: true}, AllowPrivateUpstreams: true,
			PrivateWebhookAllowlist: map[string]bool{}},
	} {
		_, err := hookCall(newWebhookClient(cfg), srv.URL)
		if !blocked(err) {
			t.Errorf("%s: err = %v, want a blocked call", name, err)
		}
	}
}

// TestGatewayGetsTheWebhookClient reads main.go and checks that the Gateway
// literal sets Webhooks from the client newWebhookClient built.
func TestGatewayGetsTheWebhookClient(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wired, built bool
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Gateway" {
				for _, el := range x.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Webhooks" {
						if v, ok := kv.Value.(*ast.Ident); ok && v.Name == "webhookClient" {
							wired = true
						}
					}
				}
			}
		case *ast.AssignStmt:
			for i, l := range x.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == "webhookClient" && i < len(x.Rhs) {
					if call, ok := x.Rhs[i].(*ast.CallExpr); ok {
						if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "newWebhookClient" {
							built = true
						}
					}
				}
			}
		}
		return true
	})
	if !built {
		t.Error("main.go does not build webhookClient with newWebhookClient")
	}
	if !wired {
		t.Error("the gateway.Gateway literal in main.go does not set Webhooks: webhookClient")
	}
}
