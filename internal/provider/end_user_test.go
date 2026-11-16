package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The end user on a translated request (ADR-035): only the value the gateway
// set aside in ChatRequest.EndUser is ever sent, and the Anthropic adapter
// sends it hashed.

func wantHash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "h-" + hex.EncodeToString(sum[:])[:61]
}

func TestValidEndUser(t *testing.T) {
	for in, want := range map[string]bool{
		"a": true, "ali@firma.com": true, "A1._:@+=/-": true, strings.Repeat("a", 128): true,
		"": false, strings.Repeat("a", 129): false, "a b": false, "a\tb": false, "a\nb": false, `a"b`: false,
		"a,b": false, "a;b": false, "a?b": false, "ş": false, "a\x00b": false, "é": false,
	} {
		if got := ValidEndUser(in); got != want {
			t.Errorf("ValidEndUser(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestEndUserHashShape(t *testing.T) {
	h := endUserHash("ali@firma.com")
	if len(h) != 63 || !strings.HasPrefix(h, "h-") || h != strings.ToLower(h) || h != wantHash("ali@firma.com") {
		t.Errorf("hash = %q", h)
	}
	// The same value always gives the same identifier; another value another one.
	first, again := endUserHash("a"), endUserHash("a")
	if first != again || first == endUserHash("b") {
		t.Error("the hash is not a stable per-value identifier")
	}
}

func TestAnthropicSendsOnlyTheGatewaysEndUserHashed(t *testing.T) {
	req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"user":"free text from the client","safety_identifier":"sid-1"}`)
	// User is the client's field and is never read: nothing goes without the
	// gateway's say-so.
	a, err := translateAnthropic(context.Background(), Config{Model: "claude-x"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Metadata != nil {
		t.Errorf("metadata = %+v, want none: User must not be read", a.Metadata)
	}

	req.EndUser = "ali@firma.com"
	a, err = translateAnthropic(context.Background(), Config{Model: "claude-x"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Metadata == nil || a.Metadata.UserID != wantHash("ali@firma.com") {
		t.Fatalf("metadata = %+v, want the hash", a.Metadata)
	}
	raw, _ := json.Marshal(a)
	for _, leak := range []string{"ali@firma.com", "free text", "sid-1", "safety_identifier"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("%q is on the wire: %s", leak, raw)
		}
	}
	if !strings.Contains(string(raw), `"metadata":{"user_id":"`+wantHash("ali@firma.com")+`"}`) {
		t.Errorf("wire = %s", raw)
	}

	// A value the grammar refuses is dropped even if something set it.
	for _, bad := range []string{"has space", strings.Repeat("x", 129), strings.Repeat("x", 600)} {
		req.EndUser = bad
		a, err := translateAnthropic(context.Background(), Config{Model: "claude-x"}, req)
		if err != nil {
			t.Fatalf("%.20q: %v", bad, err)
		}
		if a.Metadata != nil {
			t.Errorf("%.20q: metadata = %+v, want none", bad, a.Metadata)
		}
	}
}

// ChatRequest.EndUser is internal: it is never decoded from a client body and
// never written to an OpenAI-compatible upstream, which gets the client's own
// "user" untouched.
func TestEndUserIsNotOnTheOpenAIWire(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	p, _ := New(Config{ProviderType: "openai", BaseURL: srv.URL, APIKey: "k", Model: "gpt-x"})

	req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"user":"client user","EndUser":"smuggled","end_user":"smuggled"}`)
	if req.EndUser != "" {
		t.Fatalf("EndUser was decoded from the body: %q", req.EndUser)
	}
	req.EndUser = "gateway-set"
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got["user"] != "client user" {
		t.Errorf("upstream user = %v, want the client's value untouched", got["user"])
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "gateway-set") {
		t.Errorf("the internal end user reached the OpenAI-compatible upstream: %s", raw)
	}
	// The request marshals without it either (the Extra fields are the client's).
	out, _ := json.Marshal(req)
	if strings.Contains(string(out), "gateway-set") {
		t.Errorf("EndUser was marshalled: %s", out)
	}
}

func TestOllamaIgnoresTheEndUser(t *testing.T) {
	req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"user":"client user"}`)
	req.EndUser = "gateway-set"
	or, err := translateOllama(context.Background(), Config{Model: "llama"}, req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(or)
	if strings.Contains(string(b), "gateway-set") || strings.Contains(string(b), "client user") {
		t.Errorf("ollama request carries an end user: %s", b)
	}
}
