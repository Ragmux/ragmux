package provider

import (
	"context"
	"testing"
)

// Cohere bills a rerank call in search units; the adapter reads them from
// meta.billed_units and falls back to one unit when the field is missing,
// because a successful call is never free.
func TestCohereRerankUsageSearchUnits(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantUnits     int
		wantPrompt    int
		wantCompleted int
	}{
		{
			name:      "billed units are read",
			body:      `{"results":[{"index":0,"relevance_score":0.9}],"meta":{"billed_units":{"search_units":3}}}`,
			wantUnits: 3,
		},
		{
			name:      "missing meta counts as one unit",
			body:      `{"results":[{"index":0,"relevance_score":0.9}]}`,
			wantUnits: 1,
		},
		{
			name:      "zero units count as one unit",
			body:      `{"results":[{"index":0,"relevance_score":0.9}],"meta":{"billed_units":{"search_units":0}}}`,
			wantUnits: 1,
		},
		{
			name: "tokens and units come from the same meta",
			body: `{"results":[{"index":0,"relevance_score":0.9}],"meta":{"tokens":{"input_tokens":12,"output_tokens":2},` +
				`"billed_units":{"search_units":2}}}`,
			wantUnits: 2, wantPrompt: 12, wantCompleted: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := embedServer(t, c.body)
			rr, err := NewReranker(Config{ProviderType: "cohere_rerank", Model: "rerank-v3.5", APIKey: "k", BaseURL: url})
			if err != nil {
				t.Fatal(err)
			}
			ur, ok := rr.(interface {
				RerankUsage(ctx context.Context, query string, docs []string, topN int) ([]RerankResult, *Usage, error)
			})
			if !ok {
				t.Fatalf("%T has no RerankUsage", rr)
			}
			_, usage, err := ur.RerankUsage(context.Background(), "q", []string{"a", "b"}, 1)
			if err != nil {
				t.Fatal(err)
			}
			if usage == nil {
				t.Fatal("usage is nil")
			}
			if usage.SearchUnits != c.wantUnits || usage.PromptTokens != c.wantPrompt || usage.CompletionTokens != c.wantCompleted {
				t.Errorf("usage = %+v, want units %d prompt %d completion %d",
					usage, c.wantUnits, c.wantPrompt, c.wantCompleted)
			}
		})
	}
}
