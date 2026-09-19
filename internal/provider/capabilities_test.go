package provider

import "testing"

// providerTypes is every type New and NewEmbedder know about.
var providerTypes = []string{"openai", "deepseek", "custom_openai", "anthropic", "gemini", "ollama"}

// The table is written by hand, so it has to be checked against the code it
// describes: Embeddings must agree with NewEmbedder, and Streaming holds for
// every type New builds an adapter for.
func TestCapabilitiesMatchRegistry(t *testing.T) {
	// DeepSeek is the one deliberate narrowing: the adapter builds, because
	// DeepSeek speaks the OpenAI wire format, but there is no embeddings
	// endpoint behind it and /admin/api/provider-types has always said so.
	for _, pt := range providerTypes {
		caps := CapabilitiesFor(pt)
		_, err := NewEmbedder(Config{ProviderType: pt})
		switch {
		case caps.Embeddings && err != nil:
			t.Errorf("%s: Embeddings claimed but NewEmbedder err = %v", pt, err)
		case !caps.Embeddings && err == nil && pt != "deepseek":
			t.Errorf("%s: NewEmbedder builds an adapter the table does not report", pt)
		}
		if _, err := New(Config{ProviderType: pt}); err != nil {
			t.Errorf("%s: New = %v", pt, err)
		} else if !caps.Streaming {
			t.Errorf("%s: every adapter streams", pt)
		}
		if caps.ToolStreaming && !caps.Tools {
			t.Errorf("%s: ToolStreaming without Tools", pt)
		}
		if caps.Rerank {
			t.Errorf("%s: no provider type has a rerank adapter yet", pt)
		}
	}
	if (CapabilitiesFor("nope") != Capabilities{}) {
		t.Error("an unknown provider type should report nothing")
	}
	if !SupportsEmbeddings("openai") || SupportsEmbeddings("anthropic") || SupportsEmbeddings("nope") {
		t.Error("SupportsEmbeddings disagrees with the table")
	}
}
