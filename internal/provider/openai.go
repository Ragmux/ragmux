package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Default base URLs for OpenAI-compatible providers.
const (
	openAIBase   = "https://api.openai.com/v1"
	deepSeekBase = "https://api.deepseek.com/v1"
)

// openAICompat handles every provider that speaks the OpenAI wire format:
// openai, deepseek and custom_openai (vLLM, LM Studio, LiteLLM, Ollama's
// /v1 shim, ...). The ollama type uses the native adapter in ollama.go.
type openAICompat struct {
	cfg  Config
	base string
}

func newOpenAICompat(cfg Config) *openAICompat {
	def := openAIBase
	if cfg.ProviderType == "deepseek" {
		def = deepSeekBase
	}
	base := cfg.baseURL(def)
	// Users often paste the host without /v1 for Ollama/vLLM. Add it when the
	// URL does not already end in a versioned path.
	if cfg.ProviderType != "custom_openai" && !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return &openAICompat{cfg: cfg, base: base}
}

func (p *openAICompat) headers() map[string]string {
	return map[string]string{"Authorization": "Bearer " + p.cfg.APIKey}
}

func (p *openAICompat) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	req.Model = p.cfg.Model
	req.Stream = false
	req.StreamOptions = nil
	var out ChatResponse
	if err := doJSON(ctx, p.cfg, p.base+"/chat/completions", p.headers(), req, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		out.ID = chatID()
	}
	if out.Object == "" {
		out.Object = "chat.completion"
	}
	if out.Created == 0 {
		out.Created = time.Now().Unix()
	}
	// DeepSeek reports its cache split at the top level and some servers
	// omit total_tokens; normalize brings both into the OpenAI shape.
	out.Usage.normalize()
	return &out, nil
}

func (p *openAICompat) ChatStream(ctx context.Context, req ChatRequest, out chan<- StreamChunk) error {
	req.Model = p.cfg.Model
	req.Stream = true
	// Ask for usage in the final chunk; providers that do not understand this
	// field generally ignore it.
	req.StreamOptions = json.RawMessage(`{"include_usage":true}`)
	resp, err := doStream(ctx, p.cfg, p.base+"/chat/completions", p.headers(), req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var streamErr error
	err = readSSE(resp.Body, func(ev sseEvent) bool {
		data := strings.TrimSpace(ev.Data)
		if data == "" {
			return true
		}
		if data == "[DONE]" {
			return false
		}
		var chunk StreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Some servers emit error objects mid-stream.
			var env struct {
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal([]byte(data), &env) == nil && len(env.Error) > 0 {
				streamErr = upstreamError(http.StatusBadGateway, []byte(data))
				return false
			}
			return true
		}
		if chunk.Object == "" {
			chunk.Object = "chat.completion.chunk"
		}
		chunk.Usage.normalize()
		select {
		case out <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	})
	if streamErr != nil {
		return streamErr
	}
	return err
}
