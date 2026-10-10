package admin

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ragmux/ragmux/internal/provider"
)

// ---- provider presets ----
//
// A preset is a ready-made custom_openai connection for a hosted provider
// that speaks the OpenAI wire format: a base URL and the models worth
// offering. Choosing one creates an ordinary custom_openai connection through
// POST /admin/api/models; there is no preset type in storage and nothing for
// the gateway to resolve at request time.
//
// No preset carries a price. custom_openai ships no built-in price row
// (migration 0015, pricing_test.go): the type is a URL, not a place, so a
// row keyed on the type would price every self-hosted server as if it were
// the hosted provider. Prices for a preset's models are added in the
// dashboard's price table under provider type custom_openai.

// providerPreset is one entry of GET /admin/api/provider-presets.
type providerPreset struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	ProviderType string `json:"provider_type"`
	BaseURL      string `json:"base_url"`
	// Models are suggestions for model_name; the field still accepts any
	// name the provider serves.
	Models         []string `json:"models"`
	EmbeddingModel string   `json:"embedding_model,omitempty"`
	RequiresKey    bool     `json:"requires_api_key"`
}

// providerPresets is the fixed preset table. Model names are the ones the
// providers publish as stable aliases; they change slowly, and a stale one is
// a suggestion the admin overrides, not a failure.
var providerPresets = []providerPreset{
	{ID: "mistral", Label: "Mistral AI", ProviderType: "custom_openai", BaseURL: "https://api.mistral.ai/v1",
		Models:         []string{"mistral-large-latest", "mistral-medium-latest", "mistral-small-latest", "codestral-latest"},
		EmbeddingModel: "mistral-embed", RequiresKey: true},
	{ID: "xai", Label: "xAI", ProviderType: "custom_openai", BaseURL: "https://api.x.ai/v1",
		Models: []string{"grok-4", "grok-3", "grok-3-mini"}, RequiresKey: true},
	{ID: "groq", Label: "Groq", ProviderType: "custom_openai", BaseURL: "https://api.groq.com/openai/v1",
		Models:      []string{"llama-3.3-70b-versatile", "llama-3.1-8b-instant", "openai/gpt-oss-120b", "openai/gpt-oss-20b"},
		RequiresKey: true},
}

// connInput returns the create body a preset stands for, with the given model.
func (p providerPreset) connInput(model string) connInput {
	return connInput{Name: p.Label, ProviderType: p.ProviderType, BaseURL: p.BaseURL, ModelName: model}
}

func (a *Admin) providerPresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, providerPresets)
}

// ---- Azure OpenAI ----

// azureDeploymentPattern is what Azure allows in a deployment name. It is
// stricter than modelNamePattern because the name becomes a URL path segment.
var azureDeploymentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// azureAPIVersionPattern covers the shapes Azure publishes: 2024-10-21,
// 2025-04-01-preview, preview, v1.
var azureAPIVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// validateAzure checks the Azure-specific fields: base_url is the endpoint
// and may carry only an api-version query parameter, model_name is the
// deployment. The generic URL checks still run afterwards.
func (in connInput) validateAzure() error {
	if strings.TrimSpace(in.BaseURL) == "" {
		return errors.New("base_url (the Azure endpoint) is required for azure_openai")
	}
	if !azureDeploymentPattern.MatchString(in.ModelName) {
		return errors.New("model_name is the Azure deployment name for azure_openai: letters, digits, . _ - (max 64 characters)")
	}
	u, err := url.Parse(in.BaseURL)
	if err != nil {
		return errors.New("base_url must be an http:// or https:// URL")
	}
	if u.RawQuery == "" {
		return nil
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("base_url query is not valid")
	}
	for k, vs := range q {
		if k != "api-version" {
			return errors.New("base_url may only carry an api-version query parameter for azure_openai")
		}
		if len(vs) != 1 || !azureAPIVersionPattern.MatchString(vs[0]) {
			return errors.New("api-version must be a single version such as 2024-10-21")
		}
	}
	// "?" with a bare "&" or an api-version spelled twice would pass the loop
	// above but not round-trip through the adapter; compare the canonical form.
	if _, v, err := provider.SplitAzureBaseURL(in.BaseURL); err != nil || u.RawQuery != "api-version="+url.QueryEscape(v) {
		return errors.New("base_url query must be exactly api-version=<version>")
	}
	return nil
}
