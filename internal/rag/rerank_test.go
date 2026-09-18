package rag

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

type mockChat struct {
	reply string
	err   error
	last  provider.ChatRequest
}

func (m *mockChat) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	m.last = req
	if m.err != nil {
		return nil, m.err
	}
	return &provider.ChatResponse{Choices: []provider.Choice{{Message: provider.ResponseMessage{Role: "assistant", Content: &m.reply}}}}, nil
}

func (m *mockChat) ChatStream(context.Context, provider.ChatRequest, chan<- provider.StreamChunk) error {
	return errors.New("not implemented")
}

func threeHits() []store.SearchHit {
	return []store.SearchHit{{ChunkID: 1, Content: "one"}, {ChunkID: 2, Content: "two"}, {ChunkID: 3, Content: "three"}}
}

func TestRerankReordersAndAppendsOmitted(t *testing.T) {
	m := &mockChat{reply: "Sure! Here is the ranking:\n```json\n[3, 1, 9, 3]\n```"}
	rr := &Reranker{}
	out, err := rr.Rerank(context.Background(), m, "chat-model", "which?", threeHits(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if ids(out) != "3,1,2" {
		t.Errorf("order = %s, want 3,1,2", ids(out))
	}
	if m.last.Model != "chat-model" || m.last.Temperature == nil || *m.last.Temperature != 0 || m.last.MaxTokens == nil || *m.last.MaxTokens != 200 {
		t.Errorf("request params: %+v", m.last)
	}
	prompt := m.last.Messages[0].Text()
	if !strings.Contains(prompt, "[1] one") || !strings.Contains(prompt, "[3] three") || !strings.Contains(prompt, "Return only a JSON array") {
		t.Errorf("prompt: %q", prompt)
	}
	// Cut to topK after ranking.
	out, _ = rr.Rerank(context.Background(), m, "m", "q", threeHits(), 2)
	if ids(out) != "3,1" {
		t.Errorf("cut order = %s", ids(out))
	}
}

func TestRerankFailuresKeepOriginalOrder(t *testing.T) {
	rr := &Reranker{}
	for _, m := range []*mockChat{{reply: "I cannot rank these."}, {err: errors.New("boom")}, {reply: "[]"}} {
		out, err := rr.Rerank(context.Background(), m, "m", "q", threeHits(), 2)
		if m.reply != "[]" && err == nil {
			t.Errorf("expected error for %+v", m)
		}
		if m.reply == "[]" && (err != nil || ids(out) != "1,2") {
			t.Errorf("empty ranking should keep order without error: %v %s", err, ids(out))
		}
		if ids(out) != "1,2" {
			t.Errorf("order after failure = %s, want 1,2", ids(out))
		}
	}
	// Fewer than two hits never call the model.
	m := &mockChat{err: errors.New("must not be called")}
	if out, err := rr.Rerank(context.Background(), m, "m", "q", threeHits()[:1], 5); err != nil || len(out) != 1 {
		t.Errorf("single hit: %v %v", out, err)
	}
}

func TestFormatContextLabels(t *testing.T) {
	ctx := FormatContext([]store.SearchHit{
		{Filename: "a.pdf", Section: "Install > Docker", Page: 12, Content: "x"},
		{Filename: "b.md", Content: "y"},
	})
	if !strings.Contains(ctx, "[1] (a.pdf · Install > Docker · p.12)\nx") || !strings.Contains(ctx, "[2] (b.md)\ny") {
		t.Errorf("context block:\n%s", ctx)
	}
}

func ids(hits []store.SearchHit) string {
	var s []string
	for _, h := range hits {
		s = append(s, strconv.FormatInt(h.ChunkID, 10))
	}
	return strings.Join(s, ",")
}
