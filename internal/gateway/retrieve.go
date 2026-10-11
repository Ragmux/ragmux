package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

const (
	// maxRetrieveQueryBytes bounds the query text. It is embedded as one
	// input, so it has to fit an embedding model's window anyway.
	maxRetrieveQueryBytes = 32 << 10
	// maxRetrieveTopK matches the ceiling the admin search clamps to.
	maxRetrieveTopK = 50
)

// retrieveRequest is the body of POST /v1/retrieve.
type retrieveRequest struct {
	Query string `json:"query"`
	// TopK is how many hits to return; zero uses the store's own top_k.
	TopK int `json:"top_k"`
}

// retrieve is POST /v1/retrieve: a search of the project's RAG store through
// the same core the dashboard's search and the chat endpoint's context
// injection use (rag.Retriever.SearchWith), behind the pipeline steps. The
// store's own retrieval settings apply; the request cannot override them.
func (g *Gateway) retrieve(w http.ResponseWriter, r *http.Request) {
	c, ok := g.authenticate(w, r, retrieveOperation)
	if !ok {
		return
	}
	defer c.span.End()

	req, ok := g.decodeRetrieve(c)
	if !ok {
		return
	}
	rs, ok := g.retrieveTargets(c)
	if !ok {
		return
	}
	if !g.checkLimits(c, (len(req.Query)+3)/4, false) {
		return
	}
	g.openRecord(c, false, nil)
	defer g.record(c)

	prov, rerankConn := g.rerankProvider(c, rs)
	g.invoke(c, &retrieveInvocation{gw: g, rs: rs, query: req.Query, topK: req.TopK, prov: prov, rerankConn: rerankConn})
}

// decodeRetrieve reads and validates the request body, answering the client
// itself when it cannot.
func (g *Gateway) decodeRetrieve(c *call) (retrieveRequest, bool) {
	var req retrieveRequest
	body, ok := g.readBody(c)
	if !ok {
		return req, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return req, false
	}
	switch {
	case strings.TrimSpace(req.Query) == "":
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "query is required")
	case len(req.Query) > maxRetrieveQueryBytes:
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "query is too long")
	case req.TopK < 0 || req.TopK > maxRetrieveTopK:
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "top_k must be 0 (the store's default) or between 1 and 50")
	default:
		return req, true
	}
	return req, false
}

// retrieveTargets loads the project's RAG store and installs its embedding
// connection as the call's only target: that connection is what a retrieval
// spends tokens on, so it is what the request log and the price name. A
// project without a RAG store has nothing to search.
func (g *Gateway) retrieveTargets(c *call) (*store.RAGStore, bool) {
	if c.project.RAGStoreID == nil {
		writeErrorCode(c.w, http.StatusBadRequest, "invalid_request_error", "no_rag_store", "this project has no RAG store to search")
		return nil, false
	}
	if g.Retriever == nil {
		// A server-side configuration gap, not something the client can fix.
		c.log.Error("retrieve: no retriever configured")
		noRoute(c)
		return nil, false
	}
	rs, err := g.Store.GetRAGStore(c.ctx(), *c.project.RAGStoreID)
	if err != nil {
		c.log.Error("retrieve: rag store", "id", *c.project.RAGStoreID, "err", err)
		noRoute(c)
		return nil, false
	}
	conn, err := g.Store.GetConnection(c.ctx(), rs.EmbeddingConnectionID)
	if err != nil {
		c.log.Error("retrieve: embedding connection", "id", rs.EmbeddingConnectionID, "err", err)
		noRoute(c)
		return nil, false
	}
	return rs, acceptTargets(c, []*store.ModelConnection{conn})
}

// rerankProvider is the chat adapter the LLM rerank backend asks, built from
// the project's own chat connection like the chat endpoint's retrieval does.
// It is nil when the store does not use that backend or the adapter cannot be
// built; a nil provider makes the search keep the fused order. The connection
// comes back with the adapter: it is the one that does the rerank, so it is
// the one whose price covers the rerank's tokens.
func (g *Gateway) rerankProvider(c *call, rs *store.RAGStore) (provider.Provider, *store.ModelConnection) {
	if !rs.Rerank || rs.RerankBackend == store.RerankCohere || rs.RerankBackend == store.RerankVoyage || g.Providers == nil {
		return nil, nil
	}
	conn, err := g.Store.GetConnection(c.ctx(), c.project.ModelConnectionID)
	if err != nil {
		return nil, nil
	}
	prov, err := g.Providers(conn)
	if err != nil {
		c.log.Warn("rerank provider setup", "connection", conn.ID, "err", err)
		return nil, nil
	}
	return prov, conn
}

// retrieveInvocation is the retrieve endpoint's upstreamCall. The "upstream"
// is the embedding connection of the store; the database search that follows
// is local.
type retrieveInvocation struct {
	gw    *Gateway
	rs    *store.RAGStore
	query string
	topK  int
	prov  provider.Provider
	// rerankConn is the chat connection prov was built from; nil with prov.
	rerankConn *store.ModelConnection
}

// attempt is one retrieval. It writes nothing until the search has finished,
// so a failed embedding call can be retried.
func (in *retrieveInvocation) attempt(c *call, conn *store.ModelConnection) error {
	ctx, cancel := c.attemptContext()
	defer cancel()
	model := ""
	var prov provider.Provider
	var rerankSpend *rerankUsageProvider
	if in.prov != nil {
		model = in.rerankConn.ModelName
		rerankSpend = &rerankUsageProvider{Provider: in.prov}
		prov = rerankSpend
	}
	res, err := in.gw.Retriever.SearchWith(ctx, in.rs, in.query, in.topK, prov, model)
	if res != nil {
		c.obs.upstream = time.Duration(res.RetrievalLatencyMS) * time.Millisecond
	}
	if err != nil {
		return retrievalError(c, err)
	}
	c.obs.noteResponseModel("", "", conn.ModelName)
	// Nothing was embedded for an empty store, so there is nothing to bill or
	// estimate.
	if in.rs.ChunkCount > 0 {
		fillUsage(c.rec, res.EmbedUsage, len(in.query), 0)
	}
	// An LLM rerank is a chat call on the project's chat connection that a
	// retrieve-only key opens; an API rerank (Cohere, Voyage) is a call on the
	// store's rerank connection. Their tokens are counted on the row so the
	// limits and the usage figures see them, and priced apart with the
	// connection that did the work, not the embedding connection the row names.
	//
	// The charges of an earlier attempt are dropped: a retried retrieval
	// repeats the rerank, and the row carries the tokens of the last attempt.
	c.sideCharges = c.sideCharges[:0]
	var llm sideCharge
	if in.rerankConn != nil {
		llm = llmCharge(rerankSpend, in.rerankConn.ProviderType, in.rerankConn.ModelName, true)
	}
	api := apiCharge(res.RerankSpend, true)
	rerankSpend.addTo(c.rec)
	c.rec.PromptTokens += api.prompt
	c.rec.CompletionTokens += api.completion
	c.rec.Estimated = c.rec.Estimated || api.estimated
	c.addSideCharge(llm)
	c.addSideCharge(api)
	c.rec.RAGUsed, c.rec.RAGHits = true, len(res.Hits)
	c.rec.StatusCode = http.StatusOK

	hits := make([]retrieveHit, len(res.Hits))
	for i, h := range res.Hits {
		hits[i] = retrieveHit{ChunkID: h.ChunkID, DocumentID: h.DocumentID, Content: h.Content, Score: h.Score,
			Distance: h.Distance, Metadata: retrieveMetadata{Filename: h.Filename, Section: h.Section, Page: h.Page,
				ChunkIndex: h.Index}}
	}
	c.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(c.w).Encode(retrieveResponse{Object: "retrieval", Hits: hits, Mode: res.Mode,
		Backend: res.Backend, Reranked: res.Reranked, RerankBackend: res.RerankBackend,
		RerankFallback: res.RerankFallback, RetrievalLatencyMS: res.RetrievalLatencyMS,
		RerankLatencyMS: res.RerankLatencyMS,
		Usage: embeddingsUsage{PromptTokens: c.rec.PromptTokens + c.rec.CompletionTokens,
			TotalTokens: c.rec.PromptTokens + c.rec.CompletionTokens}})
	return nil
}

// rerankUsageProvider wraps the chat adapter the LLM reranker uses and adds up
// the tokens of its calls, from the upstream's report or, when it gives none,
// from the characters sent and received. The search runs one attempt on one
// goroutine, so the counters need no lock.
type rerankUsageProvider struct {
	provider.Provider
	prompt, completion int
	estimated          bool
}

func (p *rerankUsageProvider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	resp, err := p.Provider.Chat(ctx, req)
	if err != nil || resp == nil {
		return resp, err
	}
	var rec store.RequestLog
	fillUsage(&rec, resp.Usage, messageChars(req.Messages), completionChars(resp))
	p.prompt += rec.PromptTokens
	p.completion += rec.CompletionTokens
	p.estimated = p.estimated || rec.Estimated
	return resp, nil
}

// addTo adds the counted tokens to the request row. A nil receiver, a search
// without an LLM rerank, adds nothing.
func (p *rerankUsageProvider) addTo(rec *store.RequestLog) {
	if p == nil {
		return
	}
	rec.PromptTokens += p.prompt
	rec.CompletionTokens += p.completion
	rec.Estimated = rec.Estimated || p.estimated
}

// retrievalError turns a failed search into the error invoke classifies. An
// adapter error (the embedding call) passes through with its status; anything
// else is the gateway's own side, the database or a missing row, and is
// answered as a plain server error. It is marked ClientInput so it is neither
// retried nor counted against the embedding connection's circuit breaker,
// which it says nothing about.
func retrievalError(c *call, err error) error {
	var pe *provider.Error
	if errors.As(err, &pe) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	c.log.Error("retrieval failed", "err", err)
	return &provider.Error{Status: http.StatusInternalServerError, Type: "server_error", Message: "retrieval failed",
		ClientInput: true}
}

type retrieveMetadata struct {
	Filename   string `json:"filename"`
	Section    string `json:"section"`
	Page       int    `json:"page"`
	ChunkIndex int    `json:"chunk_index"`
}

type retrieveHit struct {
	ChunkID    int64            `json:"chunk_id"`
	DocumentID int64            `json:"document_id"`
	Content    string           `json:"content"`
	Score      float64          `json:"score"`
	Distance   float64          `json:"distance"`
	Metadata   retrieveMetadata `json:"metadata"`
}

type retrieveResponse struct {
	Object             string          `json:"object"`
	Hits               []retrieveHit   `json:"hits"`
	Mode               string          `json:"mode"`
	Backend            string          `json:"backend"`
	Reranked           bool            `json:"reranked"`
	RerankBackend      string          `json:"rerank_backend"`
	RerankFallback     bool            `json:"rerank_fallback"`
	RetrievalLatencyMS int64           `json:"retrieval_latency_ms"`
	RerankLatencyMS    *int64          `json:"rerank_latency_ms"`
	Usage              embeddingsUsage `json:"usage"`
}
