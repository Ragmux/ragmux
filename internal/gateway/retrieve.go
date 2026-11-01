package gateway

import (
	"bytes"
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
	// Filter restricts the search to documents whose metadata matches
	// (ADR-025): {"key": value, "key2": {"in": [...]}}. Kept raw here and
	// parsed in decodeRetrieve, where a bad one is answered with a 400.
	Filter json.RawMessage `json:"filter"`
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

	req, filter, ok := g.decodeRetrieve(c)
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
	g.invoke(c, &retrieveInvocation{gw: g, rs: rs, query: req.Query, topK: req.TopK, filter: filter,
		prov: prov, rerankConn: rerankConn})
}

// decodeRetrieve reads and validates the request body, answering the client
// itself when it cannot.
func (g *Gateway) decodeRetrieve(c *call) (retrieveRequest, *store.MetadataFilter, bool) {
	var req retrieveRequest
	body, ok := g.readBody(c)
	if !ok {
		return req, nil, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return req, nil, false
	}
	filter, msg := checkRetrieve(req)
	if msg != "" {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", msg)
		return req, nil, false
	}
	return req, filter, true
}

// checkRetrieve validates a decoded retrieve request and parses its filter.
// A non-empty message says what is wrong with it. The REST endpoint answers
// that with a 400 and the MCP tool (MCPSearch) reports it as a tool error, so
// both accept exactly the same requests.
func checkRetrieve(req retrieveRequest) (*store.MetadataFilter, string) {
	// An absent filter is no filter; one that is present must be in the
	// grammar, "null" and the empty string included.
	var filter *store.MetadataFilter
	if req.Filter != nil {
		var err error
		if filter, err = store.ParseMetadataFilter(req.Filter); err != nil {
			return nil, err.Error()
		}
	}
	switch {
	case strings.TrimSpace(req.Query) == "":
		return nil, "query is required"
	case len(req.Query) > maxRetrieveQueryBytes:
		return nil, "query is too long"
	case req.TopK < 0 || req.TopK > maxRetrieveTopK:
		return nil, "top_k must be 0 (the store's default) or between 1 and 50"
	}
	return filter, ""
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
	// filter is the request's metadata filter; nil restricts nothing.
	filter *store.MetadataFilter
	prov   provider.Provider
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
	res, err := in.gw.Retriever.SearchWithFilter(ctx, in.rs, in.query, in.topK, prov, model, in.filter)
	if res != nil {
		c.obs.upstream = time.Duration(res.RetrievalLatencyMS) * time.Millisecond
	}
	if err != nil {
		return retrievalError(c, err)
	}
	c.obs.noteResponseModel("", "", conn.ModelName)
	// Nothing was embedded for an empty store, or for a filter that matched no
	// document, so there is nothing to bill or estimate.
	if in.rs.ChunkCount > 0 && !res.FilterMatchedNothing {
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

	// The documents' own metadata (ADR-025) rides along on every hit. It is
	// read after the search, for the hit documents only, so the search SQL and
	// the chat path, whose sources carry no metadata, are unchanged.
	docIDs := make([]int64, 0, len(res.Hits))
	for _, h := range res.Hits {
		docIDs = append(docIDs, h.DocumentID)
	}
	docMeta, err := in.gw.Store.DocumentMetadataByID(ctx, docIDs)
	if err != nil {
		return retrievalError(c, err)
	}
	hits := make([]retrieveHit, len(res.Hits))
	for i, h := range res.Hits {
		dm := docMeta[h.DocumentID]
		if len(dm) == 0 {
			dm = json.RawMessage("{}")
		}
		hits[i] = retrieveHit{ChunkID: h.ChunkID, DocumentID: h.DocumentID, Content: h.Content, Score: h.Score,
			Distance: h.Distance, Metadata: retrieveMetadata{Filename: h.Filename, Section: h.Section, Page: h.Page,
				ChunkIndex: h.Index}, DocumentMetadata: dm}
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

// rerankUsageProvider wraps the chat adapter of a side call -- the LLM rerank
// of a search, and the query condense of a chat request -- and adds up the
// tokens of its calls, from the upstream's report or, when it gives none, from
// the characters sent and received. A wrapper serves one request's calls
// made in sequence on one goroutine, so the counters need no lock; do not
// share one between concurrent calls.
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
	// DocumentMetadata is the metadata of the hit's document (ADR-025), "{}"
	// when it has none.
	DocumentMetadata json.RawMessage `json:"document_metadata"`
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

// The MCP entry points (ADR-030). internal/mcp speaks the protocol; it
// reaches the gateway's authentication, limits and request log only through
// the three functions below, which wrap the steps /v1/retrieve itself runs.
// Nothing here searches or accounts on its own.

// MCPRefuseFunc renders a refusal the gateway wrote in its own error shape in
// the caller's: status is the HTTP status, code the machine-readable code
// (invalid_api_key, insufficient_scope, project_required, ...) and message
// the text for the client.
type MCPRefuseFunc func(w http.ResponseWriter, status int, code, message string)

// MCPAuth is the authentication of an MCP request: the credential is resolved
// like /v1 does (identify), then the retrieve scope and the project are
// checked. It runs for every request, the handshake included. A request that
// passes reaches next with the principal on its context; one that does not is
// answered through refuse, with the headers the gateway set (Retry-After,
// X-Ragmux-Projects) kept.
//
// identify reads the key from Authorization: Bearer or from x-api-key
// (ADR-028), so /mcp takes the same credentials as /v1.
func (g *Gateway) MCPAuth(refuse MCPRefuseFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// identify and the checks write OpenAI-shaped errors; rw holds
			// them back so refuse can render them. next gets the real writer.
			rw := &captureWriter{ResponseWriter: w}
			passed := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				pr := principalFrom(r.Context())
				if !checkScope(rw, pr, retrieveOperation.scope, retrieveOperation.what) || !checkProject(rw, pr) {
					return
				}
				next.ServeHTTP(w, r)
			})
			g.requestID(g.identify(passed)).ServeHTTP(rw, r)
			if rw.status != 0 {
				f := rw.failure(false)
				refuse(w, f.Status, f.Code, f.Message)
			}
		})
	}
}

// MCPStore is the RAG store of the project an MCP request resolved to.
type MCPStore struct {
	ID            int64
	Name          string
	DocumentCount int
}

// MCPStoreFor returns the RAG store of the request's project, or nil when the
// project has none. The request must have passed MCPAuth.
func (g *Gateway) MCPStoreFor(ctx context.Context) (*MCPStore, error) {
	p := projectFrom(ctx)
	if p == nil || p.RAGStoreID == nil {
		return nil, nil
	}
	rs, err := g.Store.GetRAGStore(ctx, *p.RAGStoreID)
	if err != nil {
		return nil, err
	}
	return &MCPStore{ID: rs.ID, Name: rs.Name, DocumentCount: rs.DocumentCount}, nil
}

// MCPSearchArgs are the arguments of the search tool: the retrieve request's
// query, top_k and filter.
type MCPSearchArgs struct {
	Query  string
	TopK   int
	Filter json.RawMessage
}

// MCPHit is one retrieved passage as the tool returns it.
type MCPHit struct {
	Content          string          `json:"content"`
	Score            float64         `json:"score"`
	Filename         string          `json:"filename"`
	Section          string          `json:"section,omitempty"`
	Page             int             `json:"page,omitempty"`
	ChunkIndex       int             `json:"chunk_index"`
	DocumentID       int64           `json:"document_id"`
	ChunkID          int64           `json:"chunk_id"`
	DocumentMetadata json.RawMessage `json:"document_metadata"`
}

// MCPSearchResult is what a successful search returns.
type MCPSearchResult struct {
	Hits []MCPHit `json:"hits"`
}

// MCPFailure is a search that did not produce hits. Tool reports that it
// belongs to the search itself (a bad argument, a failed embedding call) and
// is the tool's answer; otherwise it is a refusal of the request (limits,
// project, server configuration) and the HTTP status stands.
type MCPFailure struct {
	Status  int
	Code    string
	Message string
	Tool    bool
}

// MCPSearch is one tools/call: the same steps as POST /v1/retrieve --
// authenticate, limits, request log, the shared retrieval core -- with the
// arguments of the tool in place of a request body. The answer is captured
// instead of written, except for the headers the steps set (rate limit,
// Retry-After). The request must have passed MCPAuth.
func (g *Gateway) MCPSearch(w http.ResponseWriter, r *http.Request, args MCPSearchArgs) (*MCPSearchResult, *MCPFailure) {
	cw := &captureWriter{ResponseWriter: w}
	c, ok := g.authenticate(cw, r, retrieveOperation)
	if !ok {
		return nil, cw.failure(false)
	}
	defer c.span.End()

	req := retrieveRequest(args)
	filter, msg := checkRetrieve(req)
	if msg != "" {
		return nil, &MCPFailure{Status: http.StatusBadRequest, Code: "invalid_request_error", Message: msg, Tool: true}
	}
	rs, ok := g.retrieveTargets(c)
	if !ok {
		return nil, cw.failure(false)
	}
	if !g.checkLimits(c, (len(req.Query)+3)/4, false) {
		return nil, cw.failure(false)
	}
	g.openRecord(c, false, nil)
	defer g.record(c)

	prov, rerankConn := g.rerankProvider(c, rs)
	g.invoke(c, &retrieveInvocation{gw: g, rs: rs, query: req.Query, topK: req.TopK, filter: filter,
		prov: prov, rerankConn: rerankConn})
	if cw.status >= http.StatusBadRequest || cw.body.Len() == 0 {
		return nil, cw.failure(true)
	}
	var resp retrieveResponse
	if err := json.Unmarshal(cw.body.Bytes(), &resp); err != nil {
		c.log.Error("mcp search: decode retrieve answer", "err", err)
		return nil, &MCPFailure{Status: http.StatusInternalServerError, Code: "server_error", Message: "retrieval failed", Tool: true}
	}
	out := &MCPSearchResult{Hits: make([]MCPHit, len(resp.Hits))}
	for i, h := range resp.Hits {
		out.Hits[i] = MCPHit{Content: h.Content, Score: h.Score, Filename: h.Metadata.Filename, Section: h.Metadata.Section,
			Page: h.Metadata.Page, ChunkIndex: h.Metadata.ChunkIndex, DocumentID: h.DocumentID, ChunkID: h.ChunkID,
			DocumentMetadata: h.DocumentMetadata}
	}
	return out, nil
}

// captureWriter holds back what the pipeline steps write -- status and body --
// and shares the real writer's headers, so the ones they set on the response
// (rate limit, Retry-After, X-Ragmux-Projects) reach the client directly.
type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// failure reads the OpenAI-shaped error the steps wrote. A body that is not
// one (a client that went away writes nothing) is a plain server error.
func (w *captureWriter) failure(tool bool) *MCPFailure {
	f := &MCPFailure{Status: w.status, Code: "server_error", Message: "request failed", Tool: tool}
	if f.Status < http.StatusBadRequest {
		f.Status = http.StatusInternalServerError
	}
	var body struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Code    *string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(w.body.Bytes(), &body) != nil || body.Error.Message == "" {
		return f
	}
	f.Message = body.Error.Message
	switch {
	case body.Error.Code != nil && *body.Error.Code != "":
		f.Code = *body.Error.Code
	case body.Error.Type != "":
		f.Code = body.Error.Type
	}
	return f
}
