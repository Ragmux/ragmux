package provider

import (
	"context"
	"net/http"
	"strings"
)

// openAIEmbedder calls /embeddings on any OpenAI-compatible server.
type openAIEmbedder struct {
	cfg  Config
	base string
}

// openAIEmbeddingsResponse is the /embeddings body shared by every
// OpenAI-format server, Azure OpenAI included.
type openAIEmbeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage *struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// usage returns the reported token counts, or nil when the upstream sent
// none or sent only zeros.
func (r openAIEmbeddingsResponse) usage() *Usage {
	if r.Usage == nil {
		return nil
	}
	prompt, total := r.Usage.PromptTokens, r.Usage.TotalTokens
	if prompt <= 0 {
		prompt = total
	}
	if prompt <= 0 {
		return nil
	}
	if total < prompt {
		total = prompt
	}
	return &Usage{PromptTokens: prompt, TotalTokens: total}
}

// vectors returns the embeddings in input order and checks that the upstream
// answered for exactly n inputs.
func (r openAIEmbeddingsResponse) vectors(n int) ([][]float32, error) {
	if len(r.Data) != n {
		return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "embedding count mismatch"}
	}
	vecs := make([][]float32, n)
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "embedding index out of range"}
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

func (e *openAIEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, *Usage, error) {
	var out openAIEmbeddingsResponse
	body := map[string]any{"model": e.cfg.Model, "input": inputs, "encoding_format": "float"}
	if err := doJSON(ctx, e.cfg, opEmbed, e.base+"/embeddings",
		map[string]string{"Authorization": "Bearer " + e.cfg.APIKey}, body, &out); err != nil {
		return nil, nil, err
	}
	vecs, err := out.vectors(len(inputs))
	if err != nil {
		return nil, nil, err
	}
	return vecs, out.usage(), nil
}

// ollamaEmbedder uses Ollama's native /api/embed (batch capable).
type ollamaEmbedder struct {
	cfg  Config
	base string
}

func (e *ollamaEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, *Usage, error) {
	var out struct {
		Embeddings      [][]float32 `json:"embeddings"`
		PromptEvalCount int         `json:"prompt_eval_count"`
	}
	body := map[string]any{"model": e.cfg.Model, "input": inputs}
	hdr := map[string]string{}
	if e.cfg.APIKey != "" {
		hdr["Authorization"] = "Bearer " + e.cfg.APIKey
	}
	if err := doJSON(ctx, e.cfg, opEmbed, e.base+"/api/embed", hdr, body, &out); err != nil {
		return nil, nil, err
	}
	if len(out.Embeddings) != len(inputs) {
		return nil, nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "embedding count mismatch"}
	}
	var usage *Usage
	if out.PromptEvalCount > 0 {
		usage = &Usage{PromptTokens: out.PromptEvalCount, TotalTokens: out.PromptEvalCount}
	}
	return out.Embeddings, usage, nil
}

// geminiEmbedder uses batchEmbedContents.
type geminiEmbedder struct {
	cfg  Config
	base string
}

// Embed returns no usage: batchEmbedContents reports no token counts.
func (e *geminiEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, *Usage, error) {
	// The body carries the plain "models/<name>"; the URL gets the escaped
	// segment so the name cannot alter the request path.
	model := "models/" + strings.TrimPrefix(e.cfg.Model, "models/")
	path := "/models/" + geminiModelPath(e.cfg.Model)
	reqs := make([]map[string]any, len(inputs))
	for i, in := range inputs {
		reqs[i] = map[string]any{"model": model, "content": map[string]any{"parts": []map[string]string{{"text": in}}}}
	}
	var out struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := doJSON(ctx, e.cfg, opEmbed, e.base+path+":batchEmbedContents",
		map[string]string{"x-goog-api-key": e.cfg.APIKey}, map[string]any{"requests": reqs}, &out); err != nil {
		return nil, nil, err
	}
	if len(out.Embeddings) != len(inputs) {
		return nil, nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "embedding count mismatch"}
	}
	vecs := make([][]float32, len(inputs))
	for i, em := range out.Embeddings {
		vecs[i] = em.Values
	}
	return vecs, nil, nil
}
