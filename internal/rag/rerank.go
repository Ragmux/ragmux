package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// Reranker reorders retrieval candidates with a chat model. The model sees
// the numbered passages and returns the relevant ones ordered by relevance;
// anything it omits is appended in the original order so nothing is lost.
type Reranker struct {
	// Timeout bounds the model call (default 10 s).
	Timeout time.Duration
}

// rerankInstruction is the phrase the mock upstream in the e2e tests keys on.
const rerankInstruction = "Return only a JSON array of passage numbers ordered from most to least relevant to the query; include only relevant passages."

const rerankPassageChars = 800

// Rerank asks prov to rank hits for query and returns at most topK hits.
// On any failure (transport, timeout, unparseable reply) the original order
// cut to topK is returned together with the error so the caller can log it
// and continue.
func (rr *Reranker) Rerank(ctx context.Context, prov provider.Provider, model, query string, hits []store.SearchHit, topK int) ([]store.SearchHit, error) {
	if topK <= 0 || topK > len(hits) {
		topK = len(hits)
	}
	if len(hits) < 2 || prov == nil {
		return hits[:topK], nil
	}
	timeout := rr.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var b strings.Builder
	b.WriteString("You are a search result reranker. Query:\n")
	b.WriteString(strings.TrimSpace(query))
	b.WriteString("\n\nPassages:\n")
	for i, h := range hits {
		fmt.Fprintf(&b, "\n[%d] %s\n", i+1, truncateRunes(strings.TrimSpace(h.Content), rerankPassageChars))
	}
	b.WriteString("\n")
	b.WriteString(rerankInstruction)
	b.WriteString(" Example: [3, 1, 2]")

	temp := 0.0
	maxTok := 200
	req := provider.ChatRequest{
		Model:       model,
		Messages:    []provider.Message{{Role: "user", Content: provider.TextContent(b.String())}},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	}
	resp, err := prov.Chat(ctx, req)
	if err != nil {
		return hits[:topK], fmt.Errorf("rerank: %w", err)
	}
	reply := ""
	if len(resp.Choices) > 0 && resp.Choices[0].Message.Content != nil {
		reply = *resp.Choices[0].Message.Content
	}
	order, ok := parseRankArray(reply, len(hits))
	if !ok {
		return hits[:topK], fmt.Errorf("rerank: could not parse ranking from reply %q", truncateRunes(reply, 200))
	}
	out := make([]store.SearchHit, 0, len(hits))
	seen := make([]bool, len(hits))
	for _, n := range order {
		out = append(out, hits[n-1])
		seen[n-1] = true
	}
	for i, h := range hits {
		if !seen[i] {
			out = append(out, h)
		}
	}
	return out[:topK], nil
}

// parseRankArray finds the first JSON array of integers in s. Numbers out
// of 1..n and duplicates are dropped. ok is false when no array is found.
func parseRankArray(s string, n int) (order []int, ok bool) {
	start := strings.Index(s, "[")
	for start >= 0 {
		end := strings.Index(s[start:], "]")
		if end < 0 {
			return nil, false
		}
		var nums []json.Number
		if err := json.Unmarshal([]byte(s[start:start+end+1]), &nums); err == nil {
			seen := map[int]bool{}
			for _, num := range nums {
				v, err := num.Int64()
				if err != nil || v < 1 || int(v) > n || seen[int(v)] {
					continue
				}
				seen[int(v)] = true
				order = append(order, int(v))
			}
			return order, true
		}
		next := strings.Index(s[start+1:], "[")
		if next < 0 {
			break
		}
		start += 1 + next
	}
	return nil, false
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
