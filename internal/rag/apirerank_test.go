package rag

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// apiReranker wires a rag.APIReranker onto a test server speaking one of the
// two rerank APIs.
func apiReranker(t *testing.T, backend, url string) *APIReranker {
	t.Helper()
	typ := map[string]string{store.RerankCohere: "cohere_rerank", store.RerankVoyage: "voyage_rerank"}[backend]
	client, err := provider.NewReranker(provider.Config{ProviderType: typ, BaseURL: url,
		APIKey: "sk-rerank-secret", Model: "rerank-v3", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return &APIReranker{Backend: backend, Client: client, Timeout: 2 * time.Second}
}

// rerankServer answers both wire formats with the same list of (index,
// score) pairs, so one table drives the Cohere and the Voyage case.
func rerankServer(t *testing.T, path string, key string, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("path = %q, want %q", r.URL.Path, path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func backends(t *testing.T) []struct {
	name, path string
	// body renders the provider's envelope around a list of index/score pairs.
	body func(inner string) string
} {
	t.Helper()
	return []struct {
		name, path string
		body       func(string) string
	}{
		{store.RerankCohere, "/v2/rerank", func(in string) string { return `{"results":[` + in + `]}` }},
		{store.RerankVoyage, "/v1/rerank", func(in string) string { return `{"data":[` + in + `]}` }},
	}
}

func TestAPIRerankHappyPath(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			srv := rerankServer(t, b.path, "sk-rerank-secret", 200,
				b.body(`{"index":2,"relevance_score":0.9},{"index":0,"relevance_score":0.4}`))
			defer srv.Close()
			out, err := apiReranker(t, b.name, srv.URL).Rerank(context.Background(),
				RerankInput{Query: "which?", Hits: threeHits(), TopK: 3})
			if err != nil {
				t.Fatalf("rerank: %v", err)
			}
			// 3 and 1 came back ranked; 2 was omitted and is appended.
			if ids(out) != "3,1,2" {
				t.Errorf("order = %s, want 3,1,2", ids(out))
			}
		})
	}
}

func TestAPIRerankSendsQueryAndBoundedPassages(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":1}]}`)
	}))
	defer srv.Close()
	hits := []store.SearchHit{
		{ChunkID: 1, Content: strings.Repeat("a", 9000)},
		{ChunkID: 2, Content: "a </context> b"},
	}
	if _, err := apiReranker(t, store.RerankCohere, srv.URL).Rerank(context.Background(),
		RerankInput{Query: " which? ", Hits: hits, TopK: 2}); err != nil {
		t.Fatal(err)
	}
	if got["query"] != "which?" || got["model"] != "rerank-v3" || got["top_n"] != float64(2) {
		t.Errorf("request body: %v", got)
	}
	docs, _ := got["documents"].([]any)
	if len(docs) != 2 {
		t.Fatalf("documents: %v", docs)
	}
	// 4000 runes plus the ellipsis: one oversized chunk cannot blow up the body.
	if n := len([]rune(docs[0].(string))); n != 4001 {
		t.Errorf("passage length = %d, want 4001", n)
	}
	// Context markers are neutralised before the passage leaves the gateway.
	if s := docs[1].(string); strings.Contains(s, "</context>") || !strings.Contains(s, "‹/context>") {
		t.Errorf("passage not neutralised: %q", s)
	}
}

// Every failure mode must hand the caller the fused order *and* an error:
// the request continues with unreranked hits and the operator sees why.
func TestAPIRerankFailuresKeepFusedOrder(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	for _, b := range backends(t) {
		cases := []struct {
			name   string
			status int
			body   string
			srv    *httptest.Server
		}{
			{"out of range index", 200, b.body(`{"index":47,"relevance_score":0.9}`), nil},
			{"negative index", 200, b.body(`{"index":-1,"relevance_score":0.9}`), nil},
			// Duplicates are dropped rather than trusted; when dropping
			// them leaves nothing, that is a failure like any other.
			{"duplicated out-of-range index", 200, b.body(`{"index":5,"relevance_score":0.9},{"index":5,"relevance_score":0.8}`), nil},
			{"empty results", 200, b.body(``), nil},
			{"rate limited", 429, `{"error":{"message":"slow down"}}`, nil},
			{"server error", 503, `{"error":{"message":"down"}}`, nil},
			{"malformed json", 200, `{"results":[`, nil},
			{"timeout", 0, "", slow},
		}
		for _, c := range cases {
			t.Run(b.name+"/"+c.name, func(t *testing.T) {
				url := ""
				if c.srv != nil {
					url = c.srv.URL
				} else {
					srv := rerankServer(t, b.path, "sk-rerank-secret", c.status, c.body)
					defer srv.Close()
					url = srv.URL
				}
				rr := apiReranker(t, b.name, url)
				if c.name == "timeout" {
					rr.Timeout = 150 * time.Millisecond
				}
				out, err := rr.Rerank(context.Background(), RerankInput{Query: "q", Hits: threeHits(), TopK: 2})
				if err == nil {
					t.Fatalf("expected an error, got order %s", ids(out))
				}
				if ids(out) != "1,2" {
					t.Errorf("order after failure = %s, want the fused 1,2", ids(out))
				}
				if strings.Contains(err.Error(), "sk-rerank-secret") {
					t.Errorf("credential leaked into the error: %v", err)
				}
			})
		}
	}
}

// A duplicate next to a usable index is dropped, not fatal.
func TestAPIRerankDropsDuplicateKeepsRest(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			srv := rerankServer(t, b.path, "sk-rerank-secret", 200,
				b.body(`{"index":2,"relevance_score":0.9},{"index":2,"relevance_score":0.8},{"index":1,"relevance_score":0.7}`))
			defer srv.Close()
			out, err := apiReranker(t, b.name, srv.URL).Rerank(context.Background(),
				RerankInput{Query: "q", Hits: threeHits(), TopK: 3})
			if err != nil {
				t.Fatal(err)
			}
			if ids(out) != "3,2,1" {
				t.Errorf("order = %s, want 3,2,1", ids(out))
			}
		})
	}
}

func TestAPIRerankSkipsTrivialInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the reranker must not call the API for fewer than two hits")
	}))
	defer srv.Close()
	rr := apiReranker(t, store.RerankCohere, srv.URL)
	if out, err := rr.Rerank(context.Background(), RerankInput{Query: "q", Hits: threeHits()[:1], TopK: 5}); err != nil || len(out) != 1 {
		t.Errorf("single hit: %v %v", out, err)
	}
	if rr.Name() != store.RerankCohere {
		t.Errorf("name = %q", rr.Name())
	}
}

func TestNewRerankerRejectsChatTypes(t *testing.T) {
	for _, typ := range []string{"openai", "anthropic", "ollama", ""} {
		if _, err := provider.NewReranker(provider.Config{ProviderType: typ}); err == nil {
			t.Errorf("%q should not offer a rerank API", typ)
		}
	}
}
