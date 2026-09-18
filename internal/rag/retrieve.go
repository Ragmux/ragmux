package rag

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// Retriever embeds a query, runs vector or hybrid search in a store and
// optionally reranks the candidates with a chat model.
type Retriever struct {
	store    *store.Store
	factory  EmbedderFactory
	Reranker *Reranker
	Log      *slog.Logger
}

// NewRetriever wires a retriever.
func NewRetriever(st *store.Store, factory EmbedderFactory) *Retriever {
	return &Retriever{store: st, factory: factory, Reranker: &Reranker{}, Log: slog.Default()}
}

// Result is the outcome of one retrieval.
type Result struct {
	Hits []store.SearchHit
	// Mode is the search mode that ran (vector or hybrid).
	Mode string
	// Reranked is true when the LLM reranker successfully reordered the hits.
	Reranked bool
}

// Search returns the top-k hits for a query using the store's settings.
// prov and model are the chat provider used for reranking; with a nil
// provider reranking is skipped. Rerank failures are logged and the fused
// order is returned instead.
func (r *Retriever) Search(ctx context.Context, rs *store.RAGStore, query string, k int, prov provider.Provider, model string) ([]store.SearchHit, error) {
	res, err := r.SearchWith(ctx, rs, query, k, prov, model)
	if err != nil {
		return nil, err
	}
	return res.Hits, nil
}

// SearchWith is Search plus the mode and rerank outcome.
func (r *Retriever) SearchWith(ctx context.Context, rs *store.RAGStore, query string, k int, prov provider.Provider, model string) (*Result, error) {
	mode := rs.SearchMode
	if mode == "" {
		mode = store.SearchHybrid
	}
	res := &Result{Hits: []store.SearchHit{}, Mode: mode}
	if rs.ChunkCount == 0 || strings.TrimSpace(query) == "" {
		return res, nil
	}
	conn, err := r.store.GetConnection(ctx, rs.EmbeddingConnectionID)
	if err != nil {
		return nil, fmt.Errorf("embedding connection: %w", err)
	}
	emb, err := r.factory(conn)
	if err != nil {
		return nil, err
	}
	vecs, err := emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if k <= 0 {
		k = rs.TopK
	}
	if k <= 0 {
		k = 5
	}
	rerank := rs.Rerank && prov != nil
	candidates := k
	switch {
	case rerank:
		candidates = max(k, rs.RerankCandidates)
	case mode == store.SearchHybrid:
		candidates = k * 3
	}
	hits, err := r.store.Search(ctx, rs.ID, query, vecs[0], store.SearchOptions{
		Mode: mode, FTSConfig: rs.FTSConfig, MaxDistance: rs.MaxDistance, Candidates: candidates})
	if err != nil {
		return nil, err
	}
	if rerank && len(hits) > 1 {
		rr := r.Reranker
		if rr == nil {
			rr = &Reranker{}
		}
		ranked, err := rr.Rerank(ctx, prov, model, query, hits, k)
		if err != nil {
			if r.Log != nil {
				r.Log.Warn("rerank failed; using fused order", "store", rs.ID, "err", err)
			}
		} else {
			res.Reranked = true
		}
		hits = ranked
	}
	if len(hits) > k {
		hits = hits[:k]
	}
	res.Hits = hits
	return res, nil
}

// ContextHeader introduces retrieved passages to the model.
const ContextHeader = "Use the following retrieved context to answer the user's request. " +
	"If the context does not contain the answer, say so rather than guessing. Cite passages by their [n] label when useful."

// FormatContext renders hits as a numbered block for prompt injection.
func FormatContext(hits []store.SearchHit) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(ContextHeader)
	b.WriteString("\n\n<context>\n")
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] (%s)\n%s\n\n", i+1, hitLabel(h), strings.TrimSpace(h.Content))
	}
	b.WriteString("</context>")
	return b.String()
}

// hitLabel renders "filename · section · p.12" with the parts that exist.
func hitLabel(h store.SearchHit) string {
	parts := []string{h.Filename}
	if h.Section != "" {
		parts = append(parts, h.Section)
	}
	if h.Page > 0 {
		parts = append(parts, fmt.Sprintf("p.%d", h.Page))
	}
	return strings.Join(parts, " · ")
}

// LastUserQuery finds the most recent user message text.
func LastUserQuery(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Text()
		}
	}
	return ""
}

// InjectContext prepends the context block to the system prompt (creating
// one when absent). It returns a copy; the input is not mutated.
func InjectContext(msgs []provider.Message, contextBlock string) []provider.Message {
	if contextBlock == "" {
		return msgs
	}
	out := make([]provider.Message, 0, len(msgs)+1)
	injected := false
	for _, m := range msgs {
		if !injected && (m.Role == "system" || m.Role == "developer") {
			m.Content = provider.TextContent(contextBlock + "\n\n" + m.Text())
			injected = true
		}
		out = append(out, m)
	}
	if !injected {
		out = append([]provider.Message{{Role: "system", Content: provider.TextContent(contextBlock)}}, out...)
	}
	return out
}
