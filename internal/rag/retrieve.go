package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/muhammetsafak/ragmux/internal/provider"
	"github.com/muhammetsafak/ragmux/internal/store"
)

// Retriever embeds a query and finds the closest chunks in a store.
type Retriever struct {
	store   *store.Store
	factory EmbedderFactory
}

// NewRetriever wires a retriever.
func NewRetriever(st *store.Store, factory EmbedderFactory) *Retriever {
	return &Retriever{store: st, factory: factory}
}

// Search returns the top-k hits for a query in the given store.
func (r *Retriever) Search(ctx context.Context, rs *store.RAGStore, query string, k int) ([]store.SearchHit, error) {
	if rs.ChunkCount == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
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
	return r.store.SearchTopK(ctx, rs.ID, vecs[0], k)
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
		fmt.Fprintf(&b, "[%d] (%s)\n%s\n\n", i+1, h.Filename, strings.TrimSpace(h.Content))
	}
	b.WriteString("</context>")
	return b.String()
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
