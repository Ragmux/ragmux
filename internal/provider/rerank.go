package provider

import (
	"context"
	"fmt"
	"net/http"
	"unicode/utf8"
)

// The rerank HTTP clients live here rather than in internal/rag on purpose.
// A rerank call ships the retrieved passages -- document content, the most
// sensitive payload the gateway handles -- to a third party, so it must
// travel the one hardened outbound path every other upstream call uses: the
// netguard dialer and its redirect cap, transportError's fixed messages (a
// raw dial error carries addresses and proxy details), RedactWith over the
// credential, upstreamError's 5xx-to-502 normalisation, and the 32 MiB body
// cap. A second, ad-hoc http.Client inside internal/rag would opt out of all
// of it without a single line saying so.

// Reranker reorders passages by relevance to a query. Implementations return
// at most topN results, each pointing at an index into docs.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankResult, error)
}

// RerankResult is one reranked passage: Index is a position in the docs
// slice the caller passed in, Score the provider's relevance score.
type RerankResult struct {
	Index int
	Score float64
}

// Rerank API defaults.
const (
	cohereRerankBase = "https://api.cohere.com"
	voyageRerankBase = "https://api.voyageai.com"
	// rerankPassageChars bounds what one passage contributes to the request
	// body. An API reranker has no prompt budget to protect, but the body
	// still needs a ceiling: with rerank_candidates capped at 100 this keeps
	// one call under roughly 400 KB.
	rerankPassageChars = 4000
)

// NewReranker returns the rerank adapter for a connection configuration.
func NewReranker(cfg Config) (Reranker, error) {
	switch cfg.ProviderType {
	case "cohere_rerank":
		return &cohereReranker{cfg: cfg, base: cfg.baseURL(cohereRerankBase)}, nil
	case "voyage_rerank":
		return &voyageReranker{cfg: cfg, base: cfg.baseURL(voyageRerankBase)}, nil
	}
	return nil, fmt.Errorf("provider type %q does not offer a rerank API", cfg.ProviderType)
}

type cohereReranker struct {
	cfg  Config
	base string
}

func (r *cohereReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankResult, error) {
	body := map[string]any{
		"model":     r.cfg.Model,
		"query":     query,
		"documents": truncatePassages(docs),
		"top_n":     topN,
	}
	var out struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	headers := map[string]string{"Authorization": "Bearer " + r.cfg.APIKey}
	if err := doJSON(ctx, r.cfg, r.base+"/v2/rerank", headers, body, &out); err != nil {
		return nil, err
	}
	res := make([]RerankResult, 0, len(out.Results))
	for _, x := range out.Results {
		res = append(res, RerankResult{Index: x.Index, Score: x.RelevanceScore})
	}
	return filterRerankResults(res, len(docs))
}

type voyageReranker struct {
	cfg  Config
	base string
}

func (r *voyageReranker) Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankResult, error) {
	body := map[string]any{
		"model":     r.cfg.Model,
		"query":     query,
		"documents": truncatePassages(docs),
		"top_k":     topN,
		// Voyage rejects a request whose query or documents exceed the
		// model's context instead of trimming; truncation makes an oversized
		// passage a shorter passage rather than a failed retrieval.
		"truncation": true,
	}
	var out struct {
		Data []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"data"`
	}
	headers := map[string]string{"Authorization": "Bearer " + r.cfg.APIKey}
	if err := doJSON(ctx, r.cfg, r.base+"/v1/rerank", headers, body, &out); err != nil {
		return nil, err
	}
	res := make([]RerankResult, 0, len(out.Data))
	for _, x := range out.Data {
		res = append(res, RerankResult{Index: x.Index, Score: x.RelevanceScore})
	}
	return filterRerankResults(res, len(docs))
}

// filterRerankResults drops indexes outside 0..n-1 and repeats of one the
// provider already named, then fails when nothing is left.
//
// A remote index is never used as a slice offset without this: the value
// comes from an upstream response, so a malformed or hostile reply would
// otherwise panic the gateway on an out-of-range read. Silently dropping the
// bad entries and keeping the good ones degrades to "fewer passages
// reordered", which the caller already handles.
func filterRerankResults(res []RerankResult, n int) ([]RerankResult, error) {
	out := make([]RerankResult, 0, len(res))
	seen := make(map[int]bool, len(res))
	for _, x := range res {
		if x.Index < 0 || x.Index >= n || seen[x.Index] {
			continue
		}
		seen[x.Index] = true
		out = append(out, x)
	}
	if len(out) == 0 {
		return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error",
			Message: "rerank response carried no usable result index"}
	}
	return out, nil
}

// truncatePassages bounds each passage so one oversized chunk cannot blow up
// the request body.
func truncatePassages(docs []string) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = truncateRunes(d, rerankPassageChars)
	}
	return out
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
