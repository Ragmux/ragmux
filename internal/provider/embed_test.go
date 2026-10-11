package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// embedServer answers every request with body and returns its URL.
func embedServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestEmbedUsage(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		body     string
		inputs   []string
		wantNil  bool
		wantToks int
	}{
		{
			name:     "openai reports prompt tokens",
			cfg:      Config{ProviderType: "custom_openai", Model: "m", APIKey: "k"},
			body:     `{"data":[{"index":0,"embedding":[1,2]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`,
			inputs:   []string{"a"},
			wantToks: 7,
		},
		{
			name:     "openai total only",
			cfg:      Config{ProviderType: "custom_openai", Model: "m", APIKey: "k"},
			body:     `{"data":[{"index":0,"embedding":[1,2]}],"usage":{"total_tokens":5}}`,
			inputs:   []string{"a"},
			wantToks: 5,
		},
		{
			name:    "openai without usage",
			cfg:     Config{ProviderType: "custom_openai", Model: "m", APIKey: "k"},
			body:    `{"data":[{"index":0,"embedding":[1,2]}]}`,
			inputs:  []string{"a"},
			wantNil: true,
		},
		{
			name:    "openai zero usage counts as none",
			cfg:     Config{ProviderType: "custom_openai", Model: "m", APIKey: "k"},
			body:    `{"data":[{"index":0,"embedding":[1,2]}],"usage":{"prompt_tokens":0,"total_tokens":0}}`,
			inputs:  []string{"a"},
			wantNil: true,
		},
		{
			name:     "ollama prompt_eval_count",
			cfg:      Config{ProviderType: "ollama", Model: "m"},
			body:     `{"embeddings":[[1,2]],"prompt_eval_count":9}`,
			inputs:   []string{"a"},
			wantToks: 9,
		},
		{
			name:    "ollama without count",
			cfg:     Config{ProviderType: "ollama", Model: "m"},
			body:    `{"embeddings":[[1,2]]}`,
			inputs:  []string{"a"},
			wantNil: true,
		},
		{
			name:    "gemini reports nothing",
			cfg:     Config{ProviderType: "gemini", Model: "text-embedding-004", APIKey: "k"},
			body:    `{"embeddings":[{"values":[1,2]}]}`,
			inputs:  []string{"a"},
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.BaseURL = embedServer(t, tc.body)
			emb, err := NewEmbedder(cfg)
			if err != nil {
				t.Fatal(err)
			}
			vecs, usage, err := emb.Embed(context.Background(), tc.inputs)
			if err != nil {
				t.Fatal(err)
			}
			if len(vecs) != len(tc.inputs) {
				t.Fatalf("vectors = %d, want %d", len(vecs), len(tc.inputs))
			}
			if tc.wantNil {
				if usage != nil {
					t.Errorf("usage = %+v, want nil", usage)
				}
				return
			}
			if usage == nil || usage.PromptTokens != tc.wantToks || usage.TotalTokens != tc.wantToks {
				t.Errorf("usage = %+v, want %d prompt and total tokens", usage, tc.wantToks)
			}
		})
	}
}

func TestAzureEmbedUsage(t *testing.T) {
	url := embedServer(t, `{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`)
	emb, err := NewEmbedder(Config{ProviderType: AzureProviderType, BaseURL: url, Model: "dep", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	_, usage, err := emb.Embed(context.Background(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.PromptTokens != 3 {
		t.Errorf("usage = %+v, want 3 prompt tokens", usage)
	}
}
