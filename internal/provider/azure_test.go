package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func azureFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "azure", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// azureServer records the request the adapter sends and answers with a
// fixture. The recorded request is complete only after the call returns.
type azureSeen struct {
	path          string
	query         string
	apiKey        string
	authorization string
	contentType   string
	body          string
}

func azureServer(t *testing.T, contentType, fixture string) (*httptest.Server, *azureSeen) {
	t.Helper()
	seen := &azureSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path = r.URL.EscapedPath()
		seen.query = r.URL.RawQuery
		seen.apiKey = r.Header.Get("api-key")
		seen.authorization = r.Header.Get("Authorization")
		seen.contentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		seen.body = string(b)
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(azureFixture(t, fixture))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func TestAzureChatRequestShape(t *testing.T) {
	srv, seen := azureServer(t, "application/json", "chat.json")
	p, err := New(Config{ProviderType: AzureProviderType, BaseURL: srv.URL + "?api-version=2025-04-01-preview", Model: "my-gpt4o", APIKey: "azure-secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), parseReq(t, `{"model":"client-name","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if seen.path != "/openai/deployments/my-gpt4o/chat/completions" {
		t.Errorf("path = %s", seen.path)
	}
	if seen.query != "api-version=2025-04-01-preview" {
		t.Errorf("query = %s", seen.query)
	}
	if seen.apiKey != "azure-secret-key" {
		t.Errorf("api-key = %q", seen.apiKey)
	}
	if seen.authorization != "" {
		t.Errorf("Authorization must not be sent, got %q", seen.authorization)
	}
	if !strings.Contains(seen.contentType, "application/json") {
		t.Errorf("content-type = %q", seen.contentType)
	}
	// The deployment, not the client's model name, selects the model; the
	// body is the non-streaming form whatever the client asked for.
	if !strings.Contains(seen.body, `"model":"my-gpt4o"`) || strings.Contains(seen.body, `"stream":true`) {
		t.Errorf("body = %s", seen.body)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content == nil ||
		*resp.Choices[0].Message.Content != "Hello from Azure" {
		t.Fatalf("response = %+v", resp)
	}
	if fr := resp.Choices[0].FinishReason; fr == nil || *fr != "stop" {
		t.Errorf("finish_reason = %v", fr)
	}
	if u := resp.Usage; u == nil || u.PromptTokens != 11 || u.CompletionTokens != 4 || u.TotalTokens != 15 ||
		u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 3 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestAzureEndpointForms(t *testing.T) {
	cases := []struct {
		name, suffix, wantPath, wantQuery string
	}{
		{"bare", "", "/openai/deployments/dep/chat/completions", "api-version=" + AzureDefaultAPIVersion},
		{"trailing slash", "/", "/openai/deployments/dep/chat/completions", "api-version=" + AzureDefaultAPIVersion},
		{"pasted openai segment", "/openai", "/openai/deployments/dep/chat/completions", "api-version=" + AzureDefaultAPIVersion},
		{"explicit version", "?api-version=2024-06-01", "/openai/deployments/dep/chat/completions", "api-version=2024-06-01"},
		{"slash and version", "/?api-version=preview", "/openai/deployments/dep/chat/completions", "api-version=preview"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, seen := azureServer(t, "application/json", "chat.json")
			p, _ := New(Config{ProviderType: AzureProviderType, BaseURL: srv.URL + c.suffix, Model: "dep", APIKey: "k"})
			if _, err := p.Chat(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`)); err != nil {
				t.Fatal(err)
			}
			if seen.path != c.wantPath || seen.query != c.wantQuery {
				t.Errorf("request = %s?%s, want %s?%s", seen.path, seen.query, c.wantPath, c.wantQuery)
			}
		})
	}
}

func TestAzureDeploymentIsOnePathSegment(t *testing.T) {
	srv, seen := azureServer(t, "application/json", "chat.json")
	p, _ := New(Config{ProviderType: AzureProviderType, BaseURL: srv.URL, Model: "a/../b?x=1", APIKey: "k"})
	if _, err := p.Chat(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatal(err)
	}
	if want := "/openai/deployments/a%2F..%2Fb%3Fx=1/chat/completions"; seen.path != want {
		t.Errorf("path = %s, want %s", seen.path, want)
	}
	if seen.query != "api-version="+AzureDefaultAPIVersion {
		t.Errorf("query = %s", seen.query)
	}
}

func TestAzureMissingTarget(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no endpoint":   {ProviderType: AzureProviderType, Model: "dep", APIKey: "k"},
		"no deployment": {ProviderType: AzureProviderType, BaseURL: "https://x.openai.azure.com", APIKey: "k"},
	} {
		p, err := New(cfg)
		if err != nil {
			t.Fatalf("%s: New: %v", name, err)
		}
		if _, err := p.Chat(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`)); err == nil {
			t.Errorf("%s: Chat succeeded", name)
		}
	}
}

func TestAzureChatStream(t *testing.T) {
	srv, seen := azureServer(t, "text/event-stream", "chat.sse")
	p, _ := New(Config{ProviderType: AzureProviderType, BaseURL: srv.URL, Model: "dep", APIKey: "azure-secret-key"})
	out := make(chan StreamChunk, 16)
	err := p.ChatStream(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`), out)
	close(out)
	if err != nil {
		t.Fatal(err)
	}
	if seen.path != "/openai/deployments/dep/chat/completions" || seen.query != "api-version="+AzureDefaultAPIVersion {
		t.Errorf("request = %s?%s", seen.path, seen.query)
	}
	if seen.apiKey != "azure-secret-key" || seen.authorization != "" {
		t.Errorf("auth headers: api-key=%q authorization=%q", seen.apiKey, seen.authorization)
	}
	if !strings.Contains(seen.body, `"stream":true`) || !strings.Contains(seen.body, `"include_usage":true`) {
		t.Errorf("body = %s", seen.body)
	}
	var text strings.Builder
	var finish string
	var usageSeen bool
	var chunks int
	for c := range out {
		chunks++
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text.WriteString(*ch.Delta.Content)
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
		if c.Usage != nil {
			usageSeen = c.Usage.PromptTokens == 11 && c.Usage.CompletionTokens == 4 && c.Usage.TotalTokens == 15
		}
		if c.Object == "" {
			t.Errorf("chunk without object: %+v", c)
		}
	}
	if text.String() != "Hello from Azure" {
		t.Errorf("text = %q", text.String())
	}
	if finish != "stop" {
		t.Errorf("finish = %q", finish)
	}
	if !usageSeen {
		t.Error("usage trailer was not delivered")
	}
	// The leading prompt-filter chunk (no choices, no usage) is dropped; the
	// usage trailer (no choices, with usage) is kept: 5 chunks remain.
	if chunks != 5 {
		t.Errorf("got %d chunks, want 5", chunks)
	}
}

func TestAzureStreamErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"error\":{\"code\":\"content_filter\",\"message\":\"blocked\"}}\n\n")
	}))
	t.Cleanup(srv.Close)
	p, _ := New(Config{ProviderType: AzureProviderType, BaseURL: srv.URL, Model: "dep", APIKey: "k"})
	out := make(chan StreamChunk, 4)
	if err := p.ChatStream(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`), out); err == nil {
		t.Error("an in-stream error event should surface as an error")
	}
}

func TestAzureStreamErrorDoesNotLeakKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"error\":{\"code\":\"401\",\"message\":\"bad key azure-secret-key\"}}\n\n")
	}))
	t.Cleanup(srv.Close)
	p, _ := New(Config{ProviderType: AzureProviderType, BaseURL: srv.URL, Model: "dep", APIKey: "azure-secret-key"})
	out := make(chan StreamChunk, 4)
	err := p.ChatStream(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`), out)
	if err == nil {
		t.Fatal("an in-stream error event should surface as an error")
	}
	if strings.Contains(err.Error(), "azure-secret-key") {
		t.Errorf("stream error leaks the key: %v", err)
	}
}

func TestAzureUpstreamErrorDoesNotLeakKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"401","message":"Access denied due to invalid subscription key azure-secret-key"}}`)
	}))
	t.Cleanup(srv.Close)
	p, _ := New(Config{ProviderType: AzureProviderType, BaseURL: srv.URL, Model: "dep", APIKey: "azure-secret-key"})
	_, err := p.Chat(context.Background(), parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "azure-secret-key") {
		t.Errorf("error leaks the key: %v", err)
	}
}

func TestAzureEmbeddings(t *testing.T) {
	srv, seen := azureServer(t, "application/json", "embeddings.json")
	e, err := NewEmbedder(Config{ProviderType: AzureProviderType, BaseURL: srv.URL, Model: "my-embed", APIKey: "azure-secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	vecs, _, err := e.Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if seen.path != "/openai/deployments/my-embed/embeddings" || seen.query != "api-version="+AzureDefaultAPIVersion {
		t.Errorf("request = %s?%s", seen.path, seen.query)
	}
	if seen.apiKey != "azure-secret-key" || seen.authorization != "" {
		t.Errorf("auth headers: api-key=%q authorization=%q", seen.apiKey, seen.authorization)
	}
	if !strings.Contains(seen.body, `"input":["first","second"]`) {
		t.Errorf("body = %s", seen.body)
	}
	// The fixture lists index 1 before index 0; results follow the input order.
	if len(vecs) != 2 || len(vecs[0]) != 3 || vecs[0][0] != 1 || vecs[0][2] != 0.5 || vecs[1][1] != 1 {
		t.Errorf("vectors = %v", vecs)
	}
}

func TestAzureCapabilities(t *testing.T) {
	c := CapabilitiesFor(AzureProviderType)
	if !c.Chat || !c.Streaming || !c.Embeddings {
		t.Errorf("capabilities = %+v", c)
	}
}
