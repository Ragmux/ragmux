package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

func TestProviderPresetsListed(t *testing.T) {
	e := newAdminEnv(t)
	viewer := e.user("viewer", "viewer")
	status, list := e.doList("/api/provider-presets", viewer.token)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	got := map[string]map[string]any{}
	for _, it := range list {
		m, _ := it.(map[string]any)
		id, _ := m["id"].(string)
		got[id] = m
	}
	for _, id := range []string{"mistral", "xai", "groq"} {
		m := got[id]
		if m == nil {
			t.Errorf("preset %q missing from the admin list", id)
			continue
		}
		if m["provider_type"] != "custom_openai" || m["requires_api_key"] != true {
			t.Errorf("preset %q = %v", id, m)
		}
		if u, _ := m["base_url"].(string); !strings.HasPrefix(u, "https://") {
			t.Errorf("preset %q base_url = %q", id, u)
		}
		if models, _ := m["models"].([]any); len(models) == 0 {
			t.Errorf("preset %q has no default models", id)
		}
	}
	// Anonymous callers do not get the list.
	if status, _ := e.doList("/api/provider-presets", "not-a-token"); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d", status)
	}
}

// A connection built from a preset is an ordinary custom_openai connection:
// it must pass the same validation as one typed by hand, for every model the
// preset offers, and be creatable through the real endpoint.
func TestPresetConnectionsValidateAndCreate(t *testing.T) {
	ctx := context.Background()
	open := &Admin{AllowPrivateUpstreams: true} // skips DNS; the URLs are the point here
	for _, p := range providerPresets {
		if !store.IsValidProviderType(p.ProviderType) {
			t.Errorf("preset %s: provider type %q is not valid", p.ID, p.ProviderType)
		}
		if len(p.Models) == 0 {
			t.Errorf("preset %s: no models", p.ID)
		}
		for _, m := range p.Models {
			if err := p.connInput(m).validate(ctx, open); err != nil {
				t.Errorf("preset %s model %s: %v", p.ID, m, err)
			}
		}
		if p.EmbeddingModel != "" {
			if err := p.connInput(p.EmbeddingModel).validate(ctx, open); err != nil {
				t.Errorf("preset %s embedding model: %v", p.ID, err)
			}
		}
	}

	e := newAdminEnv(t)
	e.adm.AllowPrivateUpstreams = true
	admin := e.user("admin", "admin")
	for _, p := range providerPresets {
		in := p.connInput(p.Models[0])
		in.APIKey = "preset-key-1234567890"
		status, out := e.do(http.MethodPost, "/api/models", in, admin.token)
		if status != http.StatusCreated {
			t.Fatalf("create from preset %s: %d %v", p.ID, status, out)
		}
		if out["provider_type"] != "custom_openai" || out["base_url"] != p.BaseURL || out["model_name"] != p.Models[0] {
			t.Errorf("created from preset %s: %v", p.ID, out)
		}
	}
}

func TestAzureConnectionValidation(t *testing.T) {
	ctx := context.Background()
	open := &Admin{AllowPrivateUpstreams: true}
	base := connInput{Name: "az", ProviderType: provider.AzureProviderType, ModelName: "my-gpt4o", BaseURL: "https://res.openai.azure.com"}
	with := func(f func(*connInput)) connInput { c := base; f(&c); return c }
	cases := []struct {
		name string
		in   connInput
		want string // substring of the error; "" means valid
	}{
		{"endpoint only", base, ""},
		{"with api-version", with(func(c *connInput) { c.BaseURL += "?api-version=2025-04-01-preview" }), ""},
		{"slash and api-version", with(func(c *connInput) { c.BaseURL += "/?api-version=2024-10-21" }), ""},
		{"pasted openai segment", with(func(c *connInput) { c.BaseURL += "/openai" }), ""},
		{"dotted deployment", with(func(c *connInput) { c.ModelName = "gpt-4.1_mini" }), ""},
		{"no endpoint", with(func(c *connInput) { c.BaseURL = "" }), "endpoint"},
		{"no deployment", with(func(c *connInput) { c.ModelName = "" }), "model_name"},
		{"deployment with slash", with(func(c *connInput) { c.ModelName = "a/b" }), "deployment"},
		{"deployment with colon", with(func(c *connInput) { c.ModelName = "a:b" }), "deployment"},
		{"deployment too long", with(func(c *connInput) { c.ModelName = strings.Repeat("a", 65) }), "deployment"},
		{"foreign query key", with(func(c *connInput) { c.BaseURL += "?api-version=2024-10-21&x=1" }), "api-version"},
		{"only foreign query", with(func(c *connInput) { c.BaseURL += "?key=secret" }), "api-version"},
		{"repeated api-version", with(func(c *connInput) { c.BaseURL += "?api-version=a1&api-version=b2" }), "api-version"},
		{"empty api-version", with(func(c *connInput) { c.BaseURL += "?api-version=" }), "api-version"},
		{"api-version with slash", with(func(c *connInput) { c.BaseURL += "?api-version=../x" }), "api-version"},
		{"fragment", with(func(c *connInput) { c.BaseURL += "#frag" }), "query string or fragment"},
		{"bare question mark", with(func(c *connInput) { c.BaseURL += "?" }), "query string or fragment"},
		{"credentials", with(func(c *connInput) { c.BaseURL = "https://u:p@res.openai.azure.com" }), "credentials"},
		{"ftp", with(func(c *connInput) { c.BaseURL = "ftp://res.openai.azure.com" }), "http"},
		{"private host", with(func(c *connInput) { c.BaseURL = "http://169.254.169.254" }), "private or local"},
	}
	for _, c := range cases {
		a := open
		if c.name == "private host" {
			a = &Admin{} // the SSRF guard applies to Azure endpoints like any other
		}
		err := c.in.validate(ctx, a)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}

	// A query string stays refused for every other provider type.
	other := connInput{Name: "n", ProviderType: "custom_openai", ModelName: "m", BaseURL: "https://api.example.com/v1?api-version=2024-10-21"}
	if err := other.validate(ctx, open); err == nil || !strings.Contains(err.Error(), "query string") {
		t.Errorf("custom_openai with api-version: %v", err)
	}
}

func TestAzureAppearsInProviderTypes(t *testing.T) {
	e := newAdminEnv(t)
	viewer := e.user("viewer", "viewer")
	_, list := e.doList("/api/provider-types", viewer.token)
	for _, it := range list {
		m, _ := it.(map[string]any)
		if m["type"] != provider.AzureProviderType {
			continue
		}
		if m["requires_api_key"] != true || m["supports_embeddings"] != true || m["supports_streaming"] != true {
			t.Errorf("azure_openai entry = %v", m)
		}
		return
	}
	t.Error("azure_openai is missing from /api/provider-types")
}
