package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

const (
	// maxEmbeddingInputs is how many strings one request may embed; it is the
	// ceiling OpenAI's own API sets, so SDK batching code keeps working.
	maxEmbeddingInputs = 2048
	// maxEmbeddingChars bounds one input string. The body limit already caps
	// the request; this keeps one request from being a single huge document
	// no embedding model would take anyway.
	maxEmbeddingChars = 1 << 20

	encodingFloat  = "float"
	encodingBase64 = "base64"
)

// embeddingsRequest is the body of POST /v1/embeddings, in the shape of
// OpenAI's embeddings.create.
type embeddingsRequest struct {
	// Input is a string or an array of strings; decodeEmbeddings validates it.
	Input json.RawMessage `json:"input"`
	Model string          `json:"model"`
	// EncodingFormat is "float" (the default) or "base64".
	EncodingFormat string `json:"encoding_format"`
	// Dimensions is rejected when set: the embedders have no way to shorten a
	// vector, and ignoring it would hand back a width the client did not ask for.
	Dimensions *int `json:"dimensions"`
}

// embeddings is POST /v1/embeddings: the pipeline steps in order with the
// embeddings-specific parts (body decoding, route resolution, the embedder)
// between them.
func (g *Gateway) embeddings(w http.ResponseWriter, r *http.Request) {
	c, ok := g.authenticate(w, r, embeddingsOperation)
	if !ok {
		return
	}
	defer c.span.End()

	req, inputs, ok := g.decodeEmbeddings(c)
	if !ok {
		return
	}
	c.requestedModel = req.Model
	if !g.resolveEmbeddingsRoute(c, req.Model) {
		return
	}

	chars := 0
	for _, in := range inputs {
		chars += len(in)
	}
	if !g.checkLimits(c, (chars+3)/4, false) {
		return
	}
	g.openRecord(c, false, nil)
	defer g.record(c)

	g.invoke(c, &embeddingsInvocation{gw: g, inputs: inputs, chars: chars, model: req.Model, base64: req.EncodingFormat == encodingBase64})
}

// decodeEmbeddings reads and validates the request body, answering the client
// itself when it cannot. The inputs it returns are non-empty strings.
func (g *Gateway) decodeEmbeddings(c *call) (embeddingsRequest, []string, bool) {
	var req embeddingsRequest
	body, ok := g.readBody(c)
	if !ok {
		return req, nil, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "malformed JSON body: "+err.Error())
		return req, nil, false
	}
	bad := func(msg string) (embeddingsRequest, []string, bool) {
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", msg)
		return req, nil, false
	}
	switch req.EncodingFormat {
	case "", encodingFloat:
		req.EncodingFormat = encodingFloat
	case encodingBase64:
	default:
		return bad("encoding_format must be \"float\" or \"base64\"")
	}
	if req.Dimensions != nil {
		return bad("dimensions is not supported")
	}
	inputs, msg := parseEmbeddingInput(req.Input)
	if msg != "" {
		return bad(msg)
	}
	return req, inputs, true
}

// parseEmbeddingInput turns the input field into the strings to embed. It
// returns a client-facing message instead of the inputs when the field is
// unusable. Arrays of token ids, which OpenAI also takes, are refused: the
// providers behind the gateway embed text.
func parseEmbeddingInput(raw json.RawMessage) ([]string, string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, "input is required"
	}
	var inputs []string
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, "input must be a string or an array of strings"
		}
		inputs = []string{s}
	} else if err := json.Unmarshal(raw, &inputs); err != nil {
		return nil, "input must be a string or an array of strings"
	}
	if len(inputs) == 0 {
		return nil, "input must not be empty"
	}
	if len(inputs) > maxEmbeddingInputs {
		return nil, "input may hold at most 2048 strings"
	}
	for _, in := range inputs {
		if strings.TrimSpace(in) == "" {
			return nil, "input must not contain an empty string"
		}
		if len(in) > maxEmbeddingChars {
			return nil, "an input string is too long"
		}
		if !utf8.ValidString(in) {
			return nil, "input must be valid UTF-8"
		}
	}
	return inputs, ""
}

// resolveEmbeddingsRoute picks the connections that serve an embeddings call
// (ADR-018): a kind=embeddings route named model; else the implicit route,
// which is the embedding connection of the project's RAG store and answers to
// that connection's model name and to an empty model; else model_not_found.
// Unlike chat it never falls back to the project's default connection, a chat
// model that cannot embed.
//
// When it returns true, c.targets holds at least one connection and c.conn is
// the first of them. When it returns false it has answered the client.
func (g *Gateway) resolveEmbeddingsRoute(c *call, model string) bool {
	return g.resolveKindRoute(c, store.RouteKindEmbeddings, model, "embedding",
		func(rs *store.RAGStore) *int64 { return &rs.EmbeddingConnectionID })
}

// embeddingsInvocation is the embeddings endpoint's upstreamCall.
type embeddingsInvocation struct {
	gw     *Gateway
	inputs []string
	// chars is the size of the inputs, the basis of the usage estimate when the
	// upstream reports none.
	chars int
	// model is the client's model string, echoed in the response.
	model  string
	base64 bool
}

// attempt is one embeddings call. It writes nothing until the upstream has
// answered, so a failure can be retried at another target.
func (in *embeddingsInvocation) attempt(c *call, conn *store.ModelConnection) error {
	emb, err := in.gw.Retriever.Embedder(conn)
	if err != nil {
		// The factory error names the provider type or URL; it stays in the
		// log, as in chatProvider.
		c.log.Error("embedder setup", "connection", conn.ID, "err", err)
		return newTargetConfigError()
	}
	ctx, cancel := c.attemptContext()
	defer cancel()
	upstreamStart := time.Now()
	vecs, usage, err := emb.Embed(ctx, in.inputs)
	c.obs.upstream = time.Since(upstreamStart)
	if err != nil {
		return err
	}
	if len(vecs) != len(in.inputs) {
		// An upstream that answers with the wrong number of vectors has not
		// done the job; another target may.
		return &provider.Error{Status: http.StatusBadGateway, Type: "upstream_error",
			Message: "the embedding upstream returned an unexpected number of vectors"}
	}
	model := in.model
	if model == "" {
		model = conn.ModelName
	}
	c.obs.noteResponseModel("", model, conn.ModelName)
	fillUsage(c.rec, usage, in.chars, 0)
	c.rec.StatusCode = http.StatusOK

	data := make([]embeddingObject, len(vecs))
	for i, v := range vecs {
		data[i] = embeddingObject{Object: "embedding", Index: i}
		if in.base64 {
			data[i].Embedding = encodeBase64Vector(v)
		} else {
			data[i].Embedding = v
		}
	}
	c.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(c.w).Encode(embeddingsResponse{Object: "list", Data: data, Model: model,
		Usage: embeddingsUsage{PromptTokens: c.rec.PromptTokens, TotalTokens: c.rec.PromptTokens}})
	return nil
}

type embeddingObject struct {
	Object string `json:"object"`
	Index  int    `json:"index"`
	// Embedding is a []float32, or a base64 string under encoding_format
	// "base64".
	Embedding any `json:"embedding"`
}

type embeddingsUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type embeddingsResponse struct {
	Object string            `json:"object"`
	Data   []embeddingObject `json:"data"`
	Model  string            `json:"model"`
	Usage  embeddingsUsage   `json:"usage"`
}

// encodeBase64Vector is OpenAI's base64 encoding: the little-endian IEEE 754
// bytes of the float32 values, base64 standard encoded.
func encodeBase64Vector(v []float32) string {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(f))
	}
	return base64.StdEncoding.EncodeToString(buf)
}
