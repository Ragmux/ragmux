package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/rag"
	"github.com/ragmux/ragmux/internal/store"
)

const (
	// maxRerankDocuments is how many documents one request may rank; Cohere's
	// own ceiling, so SDK code written against it keeps working.
	maxRerankDocuments = 1000
	// maxRerankDocChars bounds one document. The provider adapters truncate what
	// they send; this only keeps one request from being a single huge text.
	maxRerankDocChars = 1 << 20
	// maxRerankQueryBytes bounds the query text.
	maxRerankQueryBytes = 32 << 10
	// maxLLMRerankDocuments is the document ceiling of a chat model acting as
	// the reranker: its prompt carries every passage, and the stores cap their
	// own candidates at the same number.
	maxLLMRerankDocuments = 100
)

// rerankRequest is the body of POST /v1/rerank, in the shape of Cohere's
// rerank endpoint.
type rerankRequest struct {
	Query string `json:"query"`
	// Documents are strings or objects with a "text" field (the Cohere v1
	// form); decodeRerank validates them.
	Documents json.RawMessage `json:"documents"`
	// TopN is how many results to return; absent means all of them.
	TopN  *int   `json:"top_n"`
	Model string `json:"model"`
	// ReturnDocuments echoes each result's document text.
	ReturnDocuments bool `json:"return_documents"`
}

// rerank is POST /v1/rerank: the pipeline steps in order with the
// rerank-specific parts (body decoding, route resolution, the reranker)
// between them.
func (g *Gateway) rerank(w http.ResponseWriter, r *http.Request) {
	c, ok := g.authenticate(w, r, rerankOperation)
	if !ok {
		return
	}
	defer c.span.End()

	req, docs, ok := g.decodeRerank(c)
	if !ok {
		return
	}
	c.requestedModel = req.Model
	if !g.resolveRerankRoute(c, req.Model) {
		return
	}

	// The adapters cut each passage before sending it; the estimate counts
	// what is sent, not the full size of an oversized document.
	chars := provider.RerankRequestChars(req.Query, docs)
	if !g.checkLimits(c, (chars+3)/4, false) {
		return
	}
	g.openRecord(c, false, nil)
	defer g.record(c)

	topN := len(docs)
	if req.TopN != nil && *req.TopN < topN {
		topN = *req.TopN
	}
	g.invoke(c, &rerankInvocation{gw: g, query: strings.TrimSpace(req.Query), docs: docs, topN: topN,
		chars: chars, returnDocuments: req.ReturnDocuments})
}

// decodeRerank reads and validates the request body, answering the client
// itself when it cannot. The documents it returns are non-empty strings.
func (g *Gateway) decodeRerank(c *call) (rerankRequest, []string, bool) {
	var req rerankRequest
	body, ok := g.readBody(c)
	if !ok {
		return req, nil, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return req, nil, false
	}
	bad := func(msg string) (rerankRequest, []string, bool) {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", msg)
		return req, nil, false
	}
	switch {
	case strings.TrimSpace(req.Query) == "":
		return bad("query is required")
	case len(req.Query) > maxRerankQueryBytes:
		return bad("query is too long")
	case !utf8.ValidString(req.Query):
		return bad("query must be valid UTF-8")
	case req.TopN != nil && *req.TopN < 1:
		return bad("top_n must be at least 1")
	}
	docs, msg := parseRerankDocuments(req.Documents)
	if msg != "" {
		return bad(msg)
	}
	return req, docs, true
}

// parseRerankDocuments turns the documents field into the texts to rank. It
// returns a client-facing message instead of the texts when the field is
// unusable.
func parseRerankDocuments(raw json.RawMessage) ([]string, string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, "documents is required"
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, "documents must be an array of strings"
	}
	if len(items) == 0 {
		return nil, "documents must not be empty"
	}
	if len(items) > maxRerankDocuments {
		return nil, "documents may hold at most 1000 entries"
	}
	docs := make([]string, len(items))
	for i, item := range items {
		var text string
		if err := json.Unmarshal(item, &text); err != nil {
			var obj struct {
				Text *string `json:"text"`
			}
			if err := json.Unmarshal(item, &obj); err != nil || obj.Text == nil {
				return nil, "documents must be an array of strings"
			}
			text = *obj.Text
		}
		switch {
		case strings.TrimSpace(text) == "":
			return nil, "documents must not contain an empty text"
		case len(text) > maxRerankDocChars:
			return nil, "a document is too long"
		case !utf8.ValidString(text):
			return nil, "documents must be valid UTF-8"
		}
		docs[i] = text
	}
	return docs, ""
}

// resolveRerankRoute picks the connections that serve a rerank call (ADR-018):
// a kind=rerank route named model; else the implicit route, which is the rerank
// connection of the project's RAG store and answers to that connection's model
// name and to an empty model; else model_not_found. It never falls back to the
// project's chat connection.
//
// When it returns true, c.targets holds at least one connection and c.conn is
// the first of them. When it returns false it has answered the client.
func (g *Gateway) resolveRerankRoute(c *call, model string) bool {
	return g.resolveKindRoute(c, store.RouteKindRerank, model, "rerank",
		func(rs *store.RAGStore) *int64 { return rs.RerankConnectionID })
}

// rerankInvocation is the rerank endpoint's upstreamCall.
type rerankInvocation struct {
	gw    *Gateway
	query string
	docs  []string
	topN  int
	// chars is the size of the query and documents, the basis of the usage
	// estimate when the upstream reports none.
	chars           int
	returnDocuments bool
}

// attempt is one rerank call. It writes nothing until the upstream has
// answered, so a failure can be retried at another target.
func (in *rerankInvocation) attempt(c *call, conn *store.ModelConnection) error {
	ctx, cancel := c.attemptContext()
	defer cancel()
	upstreamStart := time.Now()
	// An earlier attempt at another target may have billed search units that
	// this one does not.
	c.searchUnits = 0
	var ranked []provider.RerankResult
	var err error
	switch {
	case provider.SupportsRerank(conn.ProviderType):
		ranked, err = in.viaRerankAPI(ctx, c, conn)
	case provider.SupportsChat(conn.ProviderType):
		ranked, err = in.viaChatModel(ctx, c, conn)
	default:
		c.log.Error("rerank: connection cannot rerank", "connection", conn.ID, "provider", conn.ProviderType)
		err = newTargetConfigError()
	}
	c.obs.upstream = time.Since(upstreamStart)
	if err != nil {
		return err
	}
	c.obs.noteResponseModel("", "", conn.ModelName)
	c.rec.StatusCode = http.StatusOK

	results := make([]rerankResult, len(ranked))
	for i, r := range ranked {
		results[i] = rerankResult{Index: r.Index, RelevanceScore: r.Score}
		if in.returnDocuments {
			results[i].Document = &rerankDocument{Text: in.docs[r.Index]}
		}
	}
	c.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(c.w).Encode(rerankResponse{ID: c.requestID, Results: results,
		Meta: rerankMeta{Tokens: rerankTokens{InputTokens: c.rec.PromptTokens, OutputTokens: c.rec.CompletionTokens}}})
	return nil
}

// viaRerankAPI ranks with a dedicated rerank API (Cohere or Voyage).
func (in *rerankInvocation) viaRerankAPI(ctx context.Context, c *call, conn *store.ModelConnection) ([]provider.RerankResult, error) {
	if in.gw.Rerankers == nil {
		c.log.Error("rerank: no reranker factory configured")
		return nil, newTargetConfigError()
	}
	rr, err := in.gw.Rerankers(conn)
	if err != nil {
		// The factory error names the provider type or URL; it stays in the log.
		c.log.Error("reranker setup", "connection", conn.ID, "err", err)
		return nil, newTargetConfigError()
	}
	res, usage, err := provider.RerankWithUsage(ctx, rr, in.query, in.docs, in.topN)
	if err != nil {
		return nil, err
	}
	// The adapters drop out-of-range and repeated indexes; the cut and the
	// order are enforced again here because the client reads them as is.
	out := make([]provider.RerankResult, 0, len(res))
	for _, r := range res {
		if r.Index >= 0 && r.Index < len(in.docs) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > in.topN {
		out = out[:in.topN]
	}
	fillUsage(c.rec, usage, in.chars, 0)
	// Cohere bills this call by search unit; the row is priced with it. Zero
	// for Voyage, which reports tokens only.
	if usage != nil {
		c.searchUnits = max(usage.SearchUnits, 0)
	}
	return out, nil
}

// viaChatModel ranks with a chat model, the way the LLM rerank backend of a RAG
// store does. A chat model gives an order and no score, so the score is derived
// from the rank: 1 for the first result, falling in equal steps over the
// documents the model ranked. A document it left out (it lists only the
// relevant ones, and "[]" means none is) is returned after them with a score
// of 0, so a client that filters on score does not take it for a match.
func (in *rerankInvocation) viaChatModel(ctx context.Context, c *call, conn *store.ModelConnection) ([]provider.RerankResult, error) {
	if len(in.docs) > maxLLMRerankDocuments {
		return nil, &provider.Error{Status: http.StatusBadRequest, Type: "invalid_request_error", ClientInput: true,
			Message: "a chat model reranks at most 100 documents per request"}
	}
	if in.gw.Providers == nil {
		c.log.Error("rerank: no provider factory configured")
		return nil, newTargetConfigError()
	}
	prov, err := in.gw.Providers(conn)
	if err != nil {
		c.log.Error("provider setup", "connection", conn.ID, "err", err)
		return nil, newTargetConfigError()
	}
	spend := &rerankUsageProvider{Provider: prov}
	hits := make([]store.SearchHit, len(in.docs))
	for i, d := range in.docs {
		hits[i] = store.SearchHit{ChunkID: int64(i), Content: d}
	}
	llm := in.gw.LLMRerank
	ranked := len(hits)
	ordered, err := llm.Rerank(ctx, rag.RerankInput{Query: in.query, Hits: hits, TopK: in.topN,
		Provider: spend, Model: conn.ModelName, Ranked: &ranked})
	// The row restarts with every attempt; tokens of a reply that turned out
	// unreadable were still spent and stay counted.
	c.rec.PromptTokens, c.rec.CompletionTokens, c.rec.Estimated = 0, 0, false
	spend.addTo(c.rec)
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		c.log.Warn("rerank: unusable ranking from the chat model", "connection", conn.ID, "err", err)
		return nil, &provider.Error{Status: http.StatusBadGateway, Type: "upstream_error",
			Message: "the rerank model returned an unreadable ranking"}
	}
	out := make([]provider.RerankResult, len(ordered))
	for i, h := range ordered {
		out[i] = provider.RerankResult{Index: int(h.ChunkID)}
		if i < ranked {
			out[i].Score = float64(ranked-i) / float64(ranked)
		}
	}
	return out, nil
}

type rerankDocument struct {
	Text string `json:"text"`
}

type rerankResult struct {
	Index          int             `json:"index"`
	RelevanceScore float64         `json:"relevance_score"`
	Document       *rerankDocument `json:"document,omitempty"`
}

type rerankTokens struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type rerankMeta struct {
	Tokens rerankTokens `json:"tokens"`
}

type rerankResponse struct {
	ID      string         `json:"id"`
	Results []rerankResult `json:"results"`
	Meta    rerankMeta     `json:"meta"`
}
