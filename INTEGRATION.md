# Integration notes for Phase 2 (provider parity)

Everything in this file touches files this branch does not own
(`cmd/ragmux/main.go`, `internal/admin/**`, `internal/e2e/**`, `docs/api.md`,
`web/index.html`). Apply it during the merge. Nothing here is needed to build or
test the branch as it stands: without the `main.go` wiring the gateway simply
runs with image fetching disabled, which is the documented `IMAGE_FETCH=false`
behaviour.

---

## 1. `cmd/ragmux/main.go` — wire the image fetcher

The fetcher must get **the same `httpClient`** the provider adapters get, so
`netguard.SafeDialContext` and `netguard.CheckRedirect(3)` cover image URLs with
no new outbound path.

In `run()`, right after the existing

```go
	httpClient := &http.Client{Transport: transport, CheckRedirect: netguard.CheckRedirect(3)}
	log.Info("upstream policy", "allow_private_upstreams", cfg.AllowPrivateUpstreams,
		"private_upstream_allowlist", len(cfg.PrivateUpstreamAllowlist))
```

insert:

```go
	// Image fetching shares the hardened client on purpose: an image URL comes
	// from the API client, so it needs the same private-address and redirect
	// policy a provider base URL gets.
	var images *provider.ImageFetcher
	if cfg.ImageFetch {
		images = &provider.ImageFetcher{
			Client:        httpClient,
			MaxBytes:      cfg.ImageFetchMaxBytes,
			Timeout:       cfg.ImageFetchTimeout,
			MaxPerRequest: cfg.ImageFetchMaxPerRequest,
			Logger:        log,
		}
		if cfg.ImageCacheEntries > 0 {
			images.Cache = provider.NewImageCache(cfg.ImageCacheEntries, cfg.ImageCacheTTL)
		}
	}
	log.Info("image policy", "fetch", cfg.ImageFetch, "max_mb", cfg.ImageFetchMaxBytes>>20,
		"max_per_request", cfg.ImageFetchMaxPerRequest, "cache_entries", cfg.ImageCacheEntries)
```

and add `Images: images` to the `provider.Config` literal a few lines below:

```go
	provCfg := func(c *store.ModelConnection) provider.Config {
		return provider.Config{ProviderType: c.ProviderType, BaseURL: c.BaseURL, APIKey: c.APIKey,
			Model: c.ModelName, Timeout: cfg.UpstreamTimeout, Client: httpClient,
			StreamMaxDuration: cfg.StreamMaxDuration, StreamMaxBytes: cfg.StreamMaxBytes, Logger: log,
			Images: images}
	}
```

`provider.NewImageCache(entries, ttl)` exists on this branch; the cache type
itself stays unexported because the only thing to do with one is assign it to
`ImageFetcher.Cache`. Its byte ceiling is fixed at 64 MiB and is not a setting.

## 2. `internal/admin/admin.go` — `/admin/api/provider-types`

Keep every existing flat `supports_*` field for compatibility and **add** a
`capabilities` object. `gemini.supports_tools` flips `false` → `true`.

Replace `func (a *Admin) providerTypes` (currently at `admin.go:455`) with:

```go
func (a *Admin) providerTypes(w http.ResponseWriter, r *http.Request) {
	type pt struct {
		Type       string `json:"type"`
		Label      string `json:"label"`
		DefaultURL string `json:"default_base_url"`
		Embeddings bool   `json:"supports_embeddings"`
		NeedsKey   bool   `json:"requires_api_key"`
		// The flat supports_* fields stay for older clients; Capabilities is
		// the full picture, taken from the provider package so this list
		// cannot drift from what the adapters actually do.
		Tools        bool                  `json:"supports_tools"`
		Streaming    bool                  `json:"supports_streaming"`
		Capabilities provider.Capabilities `json:"capabilities"`
	}
	types := []struct {
		typ, label, url string
		needsKey        bool
	}{
		{"openai", "OpenAI", "https://api.openai.com/v1", true},
		{"anthropic", "Anthropic", "https://api.anthropic.com", true},
		{"gemini", "Google Gemini", "https://generativelanguage.googleapis.com/v1beta", true},
		{"deepseek", "DeepSeek", "https://api.deepseek.com/v1", true},
		{"ollama", "Ollama", "http://localhost:11434", false},
		{"custom_openai", "Custom OpenAI-compatible (vLLM, LM Studio, ...)", "http://localhost:8000/v1", false},
	}
	out := make([]pt, 0, len(types))
	for _, t := range types {
		caps := provider.CapabilitiesFor(t.typ)
		out = append(out, pt{Type: t.typ, Label: t.label, DefaultURL: t.url,
			Embeddings: caps.Embeddings, NeedsKey: t.needsKey,
			Tools: caps.Tools, Streaming: caps.Streaming, Capabilities: caps})
	}
	writeJSON(w, http.StatusOK, out)
}
```

Resulting JSON for one entry:

```json
{
  "type": "gemini",
  "label": "Google Gemini",
  "default_base_url": "https://generativelanguage.googleapis.com/v1beta",
  "supports_embeddings": true,
  "requires_api_key": true,
  "supports_tools": true,
  "supports_streaming": true,
  "capabilities": {
    "streaming": true, "embeddings": true, "tools": true, "tool_streaming": false,
    "vision": true, "remote_images": false, "prompt_caching": false,
    "cached_token_usage": true, "rerank": false
  }
}
```

Note that `supports_embeddings` now comes from `CapabilitiesFor`, which reports
**`false` for `deepseek`** — the same value the hand-written list already had, so
the endpoint's output is unchanged apart from `gemini.supports_tools`.

## 3. `internal/e2e/models_test.go` — the assertion that pins the old value

`TestProviderTypeCapabilities` (`models_test.go:177`) asserts
`"gemini": false`. Flip it and, if useful, assert the new object:

```go
	wantTools := map[string]bool{"openai": true, "anthropic": true, "gemini": true, "deepseek": true, "ollama": true, "custom_openai": true}
```

```go
		caps, _ := m["capabilities"].(map[string]any)
		if caps == nil || caps["streaming"] != true || caps["tools"] != want {
			t.Errorf("%s: capabilities = %v", typ, m["capabilities"])
		}
```

## 4. `web/index.html` — optional, the dashboard already works

`capsLine` (line 686) and `#capCard` (line 721) read the flat fields, which
still exist, so nothing breaks. If the capability card should show the new
detail, `t.capabilities` now carries `vision`, `remote_images`, `tool_streaming`,
`prompt_caching`, `cached_token_usage` and `rerank`. One suggestion for
`#capCard`, after the existing Streaming row:

```js
<div class="row between"><span class="dim">Vision</span><span class="pill ${t.capabilities?.vision ? 'ok' : ''}">${t.capabilities?.vision ? (t.capabilities.remote_images ? 'images + URLs' : 'images (URLs fetched by the gateway)') : 'no'}</span></div>
```

## 5. `docs/api.md` — one line

Line 124 says `supports_tools` is "(false for `gemini`)". After the change:

```
| GET | `/provider-types` | viewer | Supported provider types with `type`, `label`, `default_base_url`, `supports_embeddings`, `requires_api_key`, `supports_tools`, `supports_streaming`, and a `capabilities` object (`streaming`, `embeddings`, `tools`, `tool_streaming`, `vision`, `remote_images`, `prompt_caching`, `cached_token_usage`, `rerank`) — see [Capabilities](providers.md#capabilities) |
```

## 6. Behaviour change to be aware of outside these files

`provider.SupportsEmbeddings("deepseek")` now returns **false** (it was `true`:
`NewEmbedder` builds an OpenAI-compatible embedder for DeepSeek even though
DeepSeek has no embeddings endpoint). The one caller is
`ragInput.validateEndpoint` in `internal/admin/admin.go:844`, so a RAG store can
no longer be pointed at a DeepSeek connection — which is what
`/admin/api/provider-types` and `docs/providers.md` have always advertised.
Existing stores are unaffected: `NewEmbedder` itself is unchanged and still
builds the adapter at embed time.
