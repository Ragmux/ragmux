package rag

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
)

type stubReranker struct {
	name   string
	err    error
	called bool
}

func (s *stubReranker) Name() string { return s.name }
func (s *stubReranker) Rerank(_ context.Context, in RerankInput) ([]store.SearchHit, error) {
	s.called = true
	topK := in.TopK
	if topK <= 0 || topK > len(in.Hits) {
		topK = len(in.Hits)
	}
	if s.err != nil {
		// The contract: a usable slice *and* the error.
		return in.Hits[:topK], s.err
	}
	out := append([]store.SearchHit(nil), in.Hits...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out[:topK], nil
}

func testRetriever(f RerankerFactory) *Retriever {
	return &Retriever{Rerankers: f, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// Whatever goes wrong, the caller gets hits back and learns that reranking
// did not happen: RerankFallback true, RerankLatencyMS non-nil so the panel
// shows "skipped" rather than a blank.
func TestRerankOutcomeIsAlwaysReported(t *testing.T) {
	rs := &store.RAGStore{ID: 3, Rerank: true, RerankBackend: store.RerankCohere}
	cases := []struct {
		name         string
		factory      RerankerFactory
		wantFallback bool
		wantReranked bool
		wantOrder    string
	}{
		{"reranker unavailable",
			func(context.Context, *store.RAGStore) (Reranker, error) { return nil, ErrRerankUnavailable },
			true, false, "1,2,3"},
		{"factory returns nothing",
			func(context.Context, *store.RAGStore) (Reranker, error) { return nil, nil },
			true, false, "1,2,3"},
		{"reranker fails",
			func(context.Context, *store.RAGStore) (Reranker, error) {
				return &stubReranker{name: store.RerankCohere, err: errors.New("429")}, nil
			},
			true, false, "1,2,3"},
		{"reranker works",
			func(context.Context, *store.RAGStore) (Reranker, error) {
				return &stubReranker{name: store.RerankCohere}, nil
			},
			false, true, "3,2,1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := &Result{}
			hits := testRetriever(c.factory).rerank(context.Background(), rs, res, "q", threeHits(), 3, nil, "")
			if res.RerankFallback != c.wantFallback || res.Reranked != c.wantReranked {
				t.Errorf("fallback = %v, reranked = %v", res.RerankFallback, res.Reranked)
			}
			if res.RerankBackend != store.RerankCohere {
				t.Errorf("rerank backend = %q", res.RerankBackend)
			}
			if res.RerankLatencyMS == nil {
				t.Error("rerank latency must be reported even when the reranker was skipped")
			}
			if ids(hits) != c.wantOrder {
				t.Errorf("order = %s, want %s", ids(hits), c.wantOrder)
			}
		})
	}
}

// The llm backend needs the project's chat model; without one there is
// nothing to ask, and the reranker must not be called at all.
func TestLLMRerankWithoutProviderFallsBack(t *testing.T) {
	stub := &stubReranker{name: store.RerankLLM}
	rs := &store.RAGStore{ID: 1, Rerank: true}
	res := &Result{}
	hits := testRetriever(func(context.Context, *store.RAGStore) (Reranker, error) { return stub, nil }).
		rerank(context.Background(), rs, res, "q", threeHits(), 3, nil, "")
	if stub.called {
		t.Error("the llm reranker must not be called without a chat provider")
	}
	if !res.RerankFallback || res.Reranked || res.RerankBackend != store.RerankLLM || ids(hits) != "1,2,3" {
		t.Errorf("result: %+v, order %s", res, ids(hits))
	}
	if res.RerankLatencyMS == nil {
		t.Error("rerank latency must be reported")
	}
}

// Fewer than two candidates: nothing to reorder, no call, no fallback flag.
func TestRerankSkipsSingleCandidate(t *testing.T) {
	stub := &stubReranker{name: store.RerankCohere}
	res := &Result{}
	hits := testRetriever(func(context.Context, *store.RAGStore) (Reranker, error) { return stub, nil }).
		rerank(context.Background(), &store.RAGStore{ID: 1, Rerank: true, RerankBackend: store.RerankCohere},
			res, "q", threeHits()[:1], 3, nil, "")
	if stub.called || len(hits) != 1 || res.Reranked || res.RerankFallback {
		t.Errorf("single candidate: called=%v hits=%d res=%+v", stub.called, len(hits), res)
	}
	if res.RerankLatencyMS == nil {
		t.Error("rerank latency must be reported")
	}
}

// A Retriever built without a factory still reranks with the chat model,
// which is what every caller from before rerank backends existed expects.
func TestNilFactoryDefaultsToTheLLMReranker(t *testing.T) {
	m := &mockChat{reply: "[3, 2, 1]"}
	res := &Result{}
	hits := testRetriever(nil).rerank(context.Background(), &store.RAGStore{ID: 1, Rerank: true},
		res, "q", threeHits(), 3, m, "chat-model")
	if !res.Reranked || res.RerankFallback || ids(hits) != "3,2,1" {
		t.Errorf("default factory: %+v, order %s", res, ids(hits))
	}
	if res.RerankBackend != store.RerankLLM {
		t.Errorf("backend = %q", res.RerankBackend)
	}
}
