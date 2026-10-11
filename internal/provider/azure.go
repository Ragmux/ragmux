package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
)

// Azure OpenAI addresses a model by deployment, not by model name:
//
//	{endpoint}/openai/deployments/{deployment}/chat/completions?api-version=...
//
// and authenticates with an api-key header; Authorization: Bearer is not
// sent. The connection has no column for the api-version, and a new column
// would need a migration, so the four settings map onto the existing fields:
//
//	endpoint    -> base_url (https://<resource>.openai.azure.com)
//	deployment  -> model_name
//	api-version -> the api-version query parameter of base_url (optional)
//	key         -> the encrypted api_key
//
// Keeping the key in the existing secret field is what puts it under
// `ragmux rotate-key` (ADR-012) with no extra work.
const (
	// AzureProviderType is the provider_type of an Azure OpenAI connection.
	AzureProviderType = "azure_openai"
	// AzureDefaultAPIVersion is used when base_url carries no api-version. It
	// is a GA version that covers chat completions, streaming with usage,
	// tools and embeddings.
	AzureDefaultAPIVersion = "2024-10-21"
	azureAPIVersionParam   = "api-version"
)

// SplitAzureBaseURL separates an Azure base_url into the endpoint (no query,
// no trailing slash, no trailing /openai) and its api-version. The version
// is AzureDefaultAPIVersion when the URL names none. It is exported for the
// admin validation, which must accept exactly what the adapter accepts.
func SplitAzureBaseURL(raw string) (endpoint, apiVersion string, err error) {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("invalid base_url: %w", err)
	}
	q := u.Query()
	apiVersion = q.Get(azureAPIVersionParam)
	if apiVersion == "" {
		apiVersion = AzureDefaultAPIVersion
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
	endpoint = strings.TrimRight(u.String(), "/")
	// Users paste the endpoint with or without the /openai segment the
	// portal shows next to it.
	endpoint = strings.TrimSuffix(endpoint, "/openai")
	return endpoint, apiVersion, nil
}

// azureTarget is where one deployment's operation lives.
type azureTarget struct {
	cfg      Config
	endpoint string
	version  string
}

func newAzureTarget(cfg Config) azureTarget {
	t := azureTarget{cfg: cfg, version: AzureDefaultAPIVersion}
	if cfg.BaseURL != "" {
		if ep, v, err := SplitAzureBaseURL(cfg.BaseURL); err == nil {
			t.endpoint, t.version = ep, v
		}
	}
	return t
}

func (t azureTarget) headers() map[string]string {
	return map[string]string{"api-key": t.cfg.APIKey}
}

// url builds the deployment URL for an operation such as "chat/completions".
// The deployment becomes one escaped path segment, so a name cannot alter the
// request path.
func (t azureTarget) url(op string) (string, error) {
	if t.endpoint == "" {
		return "", &Error{Status: http.StatusBadGateway, Type: "upstream_error",
			Message: "azure_openai connection has no endpoint (base_url)"}
	}
	if t.cfg.Model == "" {
		return "", &Error{Status: http.StatusBadGateway, Type: "upstream_error",
			Message: "azure_openai connection has no deployment (model_name)"}
	}
	return t.endpoint + "/openai/deployments/" + neturl.PathEscape(t.cfg.Model) + "/" + op +
		"?" + azureAPIVersionParam + "=" + neturl.QueryEscape(t.version), nil
}

// azureChat is the chat adapter for Azure OpenAI. The body and the response
// are the OpenAI Chat Completions format, so everything but addressing and
// authentication matches openAICompat.
type azureChat struct {
	t azureTarget
}

func newAzure(cfg Config) *azureChat {
	return &azureChat{t: newAzureTarget(cfg)}
}

func (p *azureChat) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	endpoint, err := p.t.url("chat/completions")
	if err != nil {
		return nil, err
	}
	req.Model = p.t.cfg.Model
	req.Messages = stripToolCallSignatures(req.Messages)
	req.Stream = false
	req.StreamOptions = nil
	var out ChatResponse
	if err := doJSON(ctx, p.t.cfg, opChat, endpoint, p.t.headers(), req, &out); err != nil {
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
	out.Usage.normalize()
	return &out, nil
}

func (p *azureChat) ChatStream(ctx context.Context, req ChatRequest, out chan<- StreamChunk) error {
	endpoint, err := p.t.url("chat/completions")
	if err != nil {
		return err
	}
	req.Model = p.t.cfg.Model
	req.Messages = stripToolCallSignatures(req.Messages)
	req.Stream = true
	req.StreamOptions = json.RawMessage(`{"include_usage":true}`)
	resp, err := doStream(ctx, p.t.cfg, endpoint, p.t.headers(), req)
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
		// Azure reports a mid-stream failure as an {"error":{...}} event, which
		// would also decode into an empty chunk, so look for it first.
		var env struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(data), &env) == nil && len(env.Error) > 0 && string(env.Error) != "null" {
			streamErr = upstreamError(http.StatusBadGateway, []byte(data), p.t.cfg.APIKey)
			return false
		}
		var chunk StreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true
		}
		if chunk.Object == "" {
			chunk.Object = "chat.completion.chunk"
		}
		// With the content filter on, Azure opens every stream with a
		// prompt_filter_results chunk: empty id and model, no choices, no
		// usage. It carries nothing a client can use, a client reading
		// choices[0] would break on it, and as the first chunk it would fix
		// the response model to the deployment name. The usage trailer also
		// has no choices but carries usage, and is kept.
		if len(chunk.Choices) == 0 && chunk.Usage == nil {
			return true
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

// azureEmbedder calls the deployment's /embeddings operation.
type azureEmbedder struct {
	t azureTarget
}

func (e *azureEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, *Usage, error) {
	endpoint, err := e.t.url("embeddings")
	if err != nil {
		return nil, nil, err
	}
	var out openAIEmbeddingsResponse
	// The deployment names the model, so the body carries none.
	body := map[string]any{"input": inputs, "encoding_format": "float"}
	if err := doJSON(ctx, e.t.cfg, opEmbed, endpoint, e.t.headers(), body, &out); err != nil {
		return nil, nil, err
	}
	vecs, err := out.vectors(len(inputs))
	if err != nil {
		return nil, nil, err
	}
	return vecs, out.usage(), nil
}
