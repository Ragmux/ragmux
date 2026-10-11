package provider

import (
	"fmt"
	"strings"
)

// New returns the chat adapter for a connection configuration.
func New(cfg Config) (Provider, error) {
	switch cfg.ProviderType {
	case "openai", "deepseek", "custom_openai":
		return newOpenAICompat(cfg), nil
	case "ollama":
		return newOllama(cfg), nil
	case "anthropic":
		return newAnthropic(cfg), nil
	case "gemini":
		return newGemini(cfg), nil
	case AzureProviderType:
		return newAzure(cfg), nil
	case "cohere_rerank", "voyage_rerank":
		return nil, fmt.Errorf("%s connections only offer a rerank API; they cannot back a project's chat model", cfg.ProviderType)
	}
	return nil, fmt.Errorf("unsupported provider type %q", cfg.ProviderType)
}

// NewEmbedder returns the embedding adapter for a connection configuration.
func NewEmbedder(cfg Config) (Embedder, error) {
	switch cfg.ProviderType {
	case "openai", "custom_openai":
		base := cfg.baseURL(openAIBase)
		if cfg.ProviderType != "custom_openai" && !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
		return &openAIEmbedder{cfg: cfg, base: base}, nil
	case "ollama":
		base := strings.TrimSuffix(cfg.baseURL(ollamaNativeBase), "/v1")
		return &ollamaEmbedder{cfg: cfg, base: base}, nil
	case "gemini":
		return &geminiEmbedder{cfg: cfg, base: cfg.baseURL(geminiBase)}, nil
	case AzureProviderType:
		return &azureEmbedder{t: newAzureTarget(cfg)}, nil
	case "anthropic":
		return nil, fmt.Errorf("anthropic does not offer an embeddings API; choose another connection for embeddings")
	case "deepseek":
		return nil, fmt.Errorf("deepseek does not offer an embeddings API; choose another connection for embeddings")
	case "cohere_rerank", "voyage_rerank":
		return nil, fmt.Errorf("%s connections only offer a rerank API; choose another connection for embeddings", cfg.ProviderType)
	}
	return nil, fmt.Errorf("unsupported provider type %q", cfg.ProviderType)
}
