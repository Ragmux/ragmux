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

func (e *openAIEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	body := map[string]any{"model": e.cfg.Model, "input": inputs, "encoding_format": "float"}
	if err := doJSON(ctx, e.cfg, http.MethodPost, e.base+"/embeddings",
		map[string]string{"Authorization": "Bearer " + e.cfg.APIKey}, body, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(inputs) {
		return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "embedding count mismatch"}
	}
	vecs := make([][]float32, len(inputs))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "embedding index out of range"}
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

// ollamaEmbedder uses Ollama's native /api/embed (batch capable).
type ollamaEmbedder struct {
	cfg  Config
	base string
}

func (e *ollamaEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	body := map[string]any{"model": e.cfg.Model, "input": inputs}
	hdr := map[string]string{}
	if e.cfg.APIKey != "" {
		hdr["Authorization"] = "Bearer " + e.cfg.APIKey
	}
	if err := doJSON(ctx, e.cfg, http.MethodPost, e.base+"/api/embed", hdr, body, &out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != len(inputs) {
		return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "embedding count mismatch"}
	}
	return out.Embeddings, nil
}

// geminiEmbedder uses batchEmbedContents.
type geminiEmbedder struct {
	cfg  Config
	base string
}

func (e *geminiEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	model := e.cfg.Model
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}
	reqs := make([]map[string]any, len(inputs))
	for i, in := range inputs {
		reqs[i] = map[string]any{"model": model, "content": map[string]any{"parts": []map[string]string{{"text": in}}}}
	}
	var out struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := doJSON(ctx, e.cfg, http.MethodPost, e.base+"/"+model+":batchEmbedContents",
		map[string]string{"x-goog-api-key": e.cfg.APIKey}, map[string]any{"requests": reqs}, &out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != len(inputs) {
		return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "embedding count mismatch"}
	}
	vecs := make([][]float32, len(inputs))
	for i, em := range out.Embeddings {
		vecs[i] = em.Values
	}
	return vecs, nil
}
