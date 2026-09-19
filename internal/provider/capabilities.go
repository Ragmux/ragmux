package provider

// Capabilities describes what the gateway's adapter does with a provider
// type. It is about the adapter, not about the upstream model: a model that
// cannot call tools still reports Tools for its provider type, because the
// translation exists and the model's own refusal is relayed as it comes.
//
// It gates nothing. Adapters already answer with a typed *Error for what they
// cannot do, and a second, pre-flight copy of those rules would drift from the
// ones that actually run. This is a description, for the dashboard and the
// docs.
type Capabilities struct {
	// Streaming is SSE (or NDJSON) relayed as OpenAI chunks.
	Streaming bool `json:"streaming"`
	// Embeddings is whether NewEmbedder returns an adapter for the type.
	Embeddings bool `json:"embeddings"`
	// Tools is tool calling in both directions; ToolStreaming is whether tool
	// calls also arrive as deltas inside a stream rather than whole.
	Tools         bool `json:"tools"`
	ToolStreaming bool `json:"tool_streaming"`
	// Vision is image parts in user messages; RemoteImages is whether the
	// upstream fetches an image URL itself. When it does not, the gateway
	// inlines the bytes (see ImageFetcher) instead.
	Vision       bool `json:"vision"`
	RemoteImages bool `json:"remote_images"`
	// PromptCaching is an explicit cache marker on the request;
	// CachedTokenUsage is whether the upstream reports cache hits in usage.
	PromptCaching    bool `json:"prompt_caching"`
	CachedTokenUsage bool `json:"cached_token_usage"`
	// Rerank is a dedicated rerank API; no provider type offers one yet.
	Rerank bool `json:"rerank"`
}

// CapabilitiesFor reports the capabilities of a provider type. It is a free
// function rather than a method on Provider because the question is static:
// the dashboard asks it before a connection with credentials exists, and
// building an adapter just to answer it would be backwards. An unknown type
// reports nothing.
func CapabilitiesFor(providerType string) Capabilities {
	switch providerType {
	case "openai":
		// Prompt caching is automatic upstream, so there is no marker to send.
		return Capabilities{Streaming: true, Embeddings: true, Tools: true, ToolStreaming: true,
			Vision: true, RemoteImages: true, CachedTokenUsage: true}
	case "deepseek":
		return Capabilities{Streaming: true, Tools: true, ToolStreaming: true, CachedTokenUsage: true}
	case "custom_openai":
		// The server behind it is unknown; this is what the OpenAI wire format
		// allows, not a promise about a particular build.
		return Capabilities{Streaming: true, Embeddings: true, Tools: true, ToolStreaming: true,
			Vision: true, RemoteImages: true}
	case "anthropic":
		return Capabilities{Streaming: true, Tools: true, ToolStreaming: true, Vision: true,
			RemoteImages: true, PromptCaching: true, CachedTokenUsage: true}
	case "gemini":
		// Gemini sends a function call whole inside one chunk, never as
		// argument deltas, and takes only Files API / gs:// image URIs.
		return Capabilities{Streaming: true, Embeddings: true, Tools: true, Vision: true,
			CachedTokenUsage: true}
	case "ollama":
		return Capabilities{Streaming: true, Embeddings: true, Tools: true, Vision: true}
	}
	return Capabilities{}
}

// SupportsEmbeddings reports whether a provider type can back a RAG store.
func SupportsEmbeddings(providerType string) bool {
	return CapabilitiesFor(providerType).Embeddings
}
