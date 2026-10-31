package provider

import (
	"reflect"
	"testing"

	"github.com/ragmux/ragmux/internal/glob"
)

// providerTypes is every type New and NewEmbedder know about.
var providerTypes = []string{"openai", "deepseek", "custom_openai", "anthropic", "gemini", "ollama", "azure_openai"}

// The table is written by hand, so it has to be checked against the code it
// describes: Embeddings must agree with NewEmbedder, and Streaming holds for
// every type New builds an adapter for.
func TestCapabilitiesMatchRegistry(t *testing.T) {
	for _, pt := range providerTypes {
		caps := CapabilitiesFor(pt, "")
		_, err := NewEmbedder(Config{ProviderType: pt})
		switch {
		case caps.Embeddings && err != nil:
			t.Errorf("%s: Embeddings claimed but NewEmbedder err = %v", pt, err)
		case !caps.Embeddings && err == nil:
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
	if !reflect.DeepEqual(CapabilitiesFor("nope", "claude-opus-5-5"), Capabilities{}) {
		t.Error("an unknown provider type should report nothing")
	}
	if !SupportsEmbeddings("openai") || SupportsEmbeddings("anthropic") || SupportsEmbeddings("nope") {
		t.Error("SupportsEmbeddings disagrees with the table")
	}
}

// The point of the model-level flags: one provider, two models, two answers.
func TestCapabilitiesForModelsDiffer(t *testing.T) {
	opus := CapabilitiesFor("anthropic", "claude-opus-5-5-20260301")
	sonnet := CapabilitiesFor("anthropic", "claude-sonnet-4-5-20250929")
	if reflect.DeepEqual(opus, sonnet) {
		t.Fatal("two models of one provider reported the same capabilities")
	}
	if opus.ForcedToolChoice || opus.Thinking != ThinkingAdaptive || opus.ThinkingOff != ThinkingOffUnsupported ||
		!opus.ThinkingPrefixBound || !opus.NativeJSONSchema {
		t.Errorf("opus 5.5 = %+v", opus)
	}
	if !sonnet.ForcedToolChoice || sonnet.Thinking != ThinkingBudget || sonnet.ThinkingPrefixBound || len(sonnet.EffortLevels) != 0 {
		t.Errorf("sonnet 4.5 = %+v", sonnet)
	}
	// The provider-level flags do not move with the model.
	opus.NativeJSONSchema, opus.ForcedToolChoice, opus.Thinking, opus.ThinkingOff, opus.EffortLevels, opus.ThinkingPrefixBound =
		sonnet.NativeJSONSchema, sonnet.ForcedToolChoice, sonnet.Thinking, sonnet.ThinkingOff, sonnet.EffortLevels, sonnet.ThinkingPrefixBound
	if !reflect.DeepEqual(opus, sonnet) {
		t.Errorf("provider-level flags differ between models:\n%+v\n%+v", opus, sonnet)
	}
}

func TestCapabilitiesForAnthropicTable(t *testing.T) {
	cases := []struct {
		model    string
		native   bool
		forced   bool
		thinking ThinkingMode
		off      string
		efforts  string
		bound    bool
	}{
		{"claude-3-5-sonnet-20241022", false, true, ThinkingNone, "disabled", "", false},
		{"claude-3-haiku-20240307", false, true, ThinkingNone, "disabled", "", false},
		{"claude-3-7-sonnet-20250219", false, true, ThinkingBudget, "disabled", "", false},
		{"claude-sonnet-4-20250514", false, true, ThinkingBudget, "disabled", "", false},
		{"claude-opus-4-20250514", false, true, ThinkingBudget, "disabled", "", false},
		{"claude-opus-4-1-20250805", false, true, ThinkingBudget, "disabled", "", false},
		{"claude-haiku-4-5-20251001", true, true, ThinkingBudget, "disabled", "", false},
		{"claude-opus-4-5-20251101", true, true, ThinkingBudget, "disabled", "low,medium,high", false},
		{"claude-opus-4-6", true, true, ThinkingAdaptive, "disabled", "low,medium,high,max", false},
		{"claude-sonnet-4-6", true, true, ThinkingAdaptive, "disabled", "low,medium,high,max", false},
		{"claude-opus-4-7", true, true, ThinkingAdaptive, "disabled", "low,medium,high,xhigh,max", false},
		{"claude-opus-4-8", true, true, ThinkingAdaptive, "disabled", "low,medium,high,xhigh,max", false},
		{"claude-opus-5", true, true, ThinkingAdaptive, "disabled", "low,medium,high,xhigh,max", false},
		{"claude-sonnet-5", true, true, ThinkingAdaptive, "disabled", "low,medium,high,xhigh,max", false},
		{"claude-opus-5-20261001", true, true, ThinkingAdaptive, "disabled", "low,medium,high,xhigh,max", false},
		{"claude-fable-5-20261001", true, true, ThinkingAdaptive, "none", "low,medium,high,xhigh,max", false},
		{"claude-opus-5-5", true, false, ThinkingAdaptive, "none", "low,medium,high,xhigh,max", true},
		{"claude-sonnet-5-5", true, false, ThinkingAdaptive, "between_tools", "low,medium,high,xhigh,max", true},
		{"claude-haiku-5-5", true, true, ThinkingAdaptive, "disabled", "low,medium,high,xhigh,max", true},
		{"claude-fable-5", true, true, ThinkingAdaptive, "none", "low,medium,high,xhigh,max", false},
		{"claude-mythos-5", true, true, ThinkingAdaptive, "none", "low,medium,high,xhigh,max", false},
		{"claude-fable-5-1", true, false, ThinkingAdaptive, "none", "low,medium,high,xhigh,max", true},
		{"claude-mythos-5-1", true, false, ThinkingAdaptive, "none", "low,medium,high,xhigh,max", true},
		{"claude-mythos-preview", true, true, ThinkingAdaptive, "none", "low,medium,high,max", false},
		// Not in the table: the newest generation's most restrictive row.
		{"claude-opus-5-6", true, false, ThinkingAdaptive, "none", "low,medium,high", true},
		{"claude-sonnet-5-6", true, false, ThinkingAdaptive, "none", "low,medium,high", true},
		{"claude-fable-5-2", true, false, ThinkingAdaptive, "none", "low,medium,high", true},
		{"claude-mythos-5-2", true, false, ThinkingAdaptive, "none", "low,medium,high", true},
		{"claude-opus-9-9", true, false, ThinkingAdaptive, "none", "low,medium,high", true},
		{"", true, false, ThinkingAdaptive, "none", "low,medium,high", true},
	}
	for _, c := range cases {
		got := CapabilitiesFor("anthropic", c.model)
		efforts := ""
		for i, e := range got.EffortLevels {
			if i > 0 {
				efforts += ","
			}
			efforts += e
		}
		if got.NativeJSONSchema != c.native || got.ForcedToolChoice != c.forced || got.Thinking != c.thinking ||
			got.ThinkingOff != c.off || efforts != c.efforts || got.ThinkingPrefixBound != c.bound {
			t.Errorf("%q = native=%v forced=%v thinking=%s off=%s efforts=%s bound=%v", c.model,
				got.NativeJSONSchema, got.ForcedToolChoice, got.Thinking, got.ThinkingOff, efforts, got.ThinkingPrefixBound)
		}
	}
}

func TestCapabilitiesForOtherProviders(t *testing.T) {
	if c := CapabilitiesFor("gemini", "gemini-2.5-flash"); c.Thinking != ThinkingBudget {
		t.Errorf("gemini 2.5 thinking = %s", c.Thinking)
	}
	if c := CapabilitiesFor("gemini", "gemini-3-pro-preview"); c.Thinking != ThinkingLevel {
		t.Errorf("gemini 3 thinking = %s", c.Thinking)
	}
	if c := CapabilitiesFor("gemini", "gemini-1.5-pro"); c.Thinking != ThinkingNone {
		t.Errorf("gemini 1.5 thinking = %s", c.Thinking)
	}
	if c := CapabilitiesFor("ollama", "gpt-oss:20b"); c.Thinking != ThinkingLevel || len(c.EffortLevels) != 3 {
		t.Errorf("gpt-oss = %+v", c)
	}
	if c := CapabilitiesFor("ollama", "llama3.2"); c.Thinking != ThinkingToggle || c.ForcedToolChoice {
		t.Errorf("ollama default = %+v", c)
	}
	// Azure names a deployment, not a model, so no row can apply: a model
	// string that looks like a Claude ID changes nothing.
	for _, pt := range []string{"openai", "deepseek", "custom_openai", AzureProviderType} {
		if got, base := CapabilitiesFor(pt, "claude-opus-5-5"), CapabilitiesFor(pt, ""); !reflect.DeepEqual(got, base) {
			t.Errorf("%s: model changed capabilities: %+v vs %+v", pt, got, base)
		}
		if c := CapabilitiesFor(pt, ""); c.Thinking != ThinkingPassthrough {
			t.Errorf("%s: thinking = %s", pt, c.Thinking)
		}
	}
}

// The table is hand-written, so its shape is checked: no pattern twice, and
// every row is reachable (a shorter pattern never hides a longer one, because
// the longer prefix is tried first).
func TestModelRulesWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for i, r := range modelRules {
		key := r.providerType + "\x00" + r.pattern
		if seen[key] {
			t.Errorf("%s/%s listed twice", r.providerType, r.pattern)
		}
		seen[key] = true
		if i > 0 && glob.Compare(modelRules[i-1].pattern, r.pattern) > 0 {
			t.Errorf("rules out of order at %s", r.pattern)
		}
		if r.flags.thinking == "" {
			t.Errorf("%s/%s has no thinking mode", r.providerType, r.pattern)
		}
		if r.flags.thinking != ThinkingNone && r.flags.thinking != ThinkingBudget && r.flags.thinking != ThinkingAdaptive &&
			r.flags.thinking != ThinkingLevel && r.flags.thinking != ThinkingToggle && r.flags.thinking != ThinkingPassthrough {
			t.Errorf("%s/%s: unknown thinking mode %q", r.providerType, r.pattern, r.flags.thinking)
		}
		switch r.flags.thinkingOff {
		case "", ThinkingOffDisabled, ThinkingOffBetweenTools, ThinkingOffUnsupported:
		default:
			t.Errorf("%s/%s: unknown thinking_off %q", r.providerType, r.pattern, r.flags.thinkingOff)
		}
	}
}

// A caller that appends to the effort slice it was handed must not change the
// answer the next caller gets.
func TestCapabilitiesForReturnsOwnedSlice(t *testing.T) {
	a := CapabilitiesFor("anthropic", "claude-opus-5-5")
	a.EffortLevels[0] = "changed"
	if b := CapabilitiesFor("anthropic", "claude-opus-5-5"); b.EffortLevels[0] != "low" {
		t.Errorf("table mutated through a returned slice: %v", b.EffortLevels)
	}
}
