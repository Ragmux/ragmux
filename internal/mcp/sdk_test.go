package mcp

// The conformance test connects the official Go SDK's client to the server
// (ADR-030), once per protocol era. The SDK is a test-only module: nothing
// outside _test.go files imports it, so it never reaches the binary.
//
// The SDK offers no public way to pin the era a client speaks, so each era is
// forced from the server side and then checked in the traffic: the modern
// subtest requires that the client never sent initialize, the legacy one
// puts a front before the server that answers a modern request the way a
// server that predates the revision does: with a bare 400 it cannot parse as
// MCP. The versioning rules expect a dual-era client to fall back to
// initialize then, and the traffic has to show that it did. (A -32022 would
// not do: that is a modern error, answered by retrying with a version from
// its supported list, not by falling back.)

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// seen is one request the client sent, as the server saw it.
type seen struct {
	method string
	modern bool // params._meta names a protocol version
}

type traffic struct {
	mu       sync.Mutex
	requests []seen
	sessions []string
}

func (tr *traffic) has(method string, modern bool) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, s := range tr.requests {
		if s.method == method && s.modern == modern {
			return true
		}
	}
	return false
}

func (tr *traffic) any(method string) bool { return tr.has(method, true) || tr.has(method, false) }

// legacyOnlyFront refuses every modern request with a plain 400, as a server
// that predates the revision does: it knows nothing of _meta versions and
// answers no JSON-RPC error a modern client would recognise.
func legacyOnlyFront(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req rpcRequest
		if json.Unmarshal(body, &req) == nil && metaVersion(req.Params) != "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func connectSDK(t *testing.T, h http.Handler) (*sdk.ClientSession, *traffic) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tr := &traffic{}
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req rpcRequest
			if json.Unmarshal(body, &req) == nil && req.Method != "" {
				tr.mu.Lock()
				tr.requests = append(tr.requests, seen{req.Method, metaVersion(req.Params) != ""})
				tr.mu.Unlock()
			}
		}
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil && resp.Header.Get("Mcp-Session-Id") != "" {
			tr.mu.Lock()
			tr.sessions = append(tr.sessions, resp.Header.Get("Mcp-Session-Id"))
			tr.mu.Unlock()
		}
		return resp, err
	})}
	client := sdk.NewClient(&sdk.Implementation{Name: "conformance", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(),
		&sdk.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, tr
}

// exercise lists the tool and calls it, the whole of what a client does here.
func exercise(t *testing.T, cs *sdk.ClientSession, tr *traffic) {
	t.Helper()
	ctx := context.Background()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != toolSearch {
		t.Fatalf("list tools: %v %+v", err, tools)
	}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: toolSearch, Arguments: map[string]any{"query": "banana", "top_k": 1}})
	if err != nil || res.IsError || len(res.Content) != 1 {
		t.Fatalf("call tool: %v %+v", err, res)
	}
	if txt, ok := res.Content[0].(*sdk.TextContent); !ok || !strings.Contains(txt.Text, "Bananas are yellow.") {
		t.Errorf("content: %+v", res.Content[0])
	}
	if len(tr.sessions) != 0 || cs.ID() != "" {
		t.Errorf("a session was minted: %v / %q", tr.sessions, cs.ID())
	}
}

func TestSDKClientModern(t *testing.T) {
	cs, tr := connectSDK(t, New(Config{Backend: newFake(), Version: "test"}))
	exercise(t, cs, tr)
	if tr.any("initialize") || !tr.has("tools/call", true) || tr.has("tools/call", false) {
		t.Errorf("the client did not speak the modern era: %+v", tr.requests)
	}
}

func TestSDKClientLegacy(t *testing.T) {
	cs, tr := connectSDK(t, legacyOnlyFront(New(Config{Backend: newFake(), Version: "test"})))
	exercise(t, cs, tr)
	if !tr.has("initialize", false) || !tr.has("tools/call", false) || tr.has("tools/call", true) {
		t.Errorf("the client did not speak the legacy era: %+v", tr.requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
