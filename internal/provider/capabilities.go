package provider

import (
	"sort"

	"github.com/ragmux/ragmux/internal/glob"
)

// ThinkingMode is how a model is asked to think.
type ThinkingMode string

const (
	// ThinkingNone: the model has no thinking switch at all.
	ThinkingNone ThinkingMode = "none"
	// ThinkingBudget: a token budget (Anthropic's legacy "enabled" +
	// budget_tokens, Gemini 2.5's thinkingBudget).
	ThinkingBudget ThinkingMode = "budget"
	// ThinkingAdaptive: Anthropic's "adaptive" thinking steered by effort.
	ThinkingAdaptive ThinkingMode = "adaptive"
	// ThinkingLevel: a named level (Gemini 3+ thinkingLevel, gpt-oss on Ollama).
	ThinkingLevel ThinkingMode = "level"
	// ThinkingToggle: on or off, nothing in between (Ollama's think bool).
	ThinkingToggle ThinkingMode = "toggle"
	// ThinkingPassthrough: the OpenAI-compatible wire format; the gateway
	// forwards reasoning_effort as sent and the upstream judges it.
	ThinkingPassthrough ThinkingMode = "passthrough"
)

// How thinking is switched off, for ThinkingOff. The empty string means the
// question does not apply to the model's ThinkingMode (or has not been
// described yet), and callers must not read it as "cannot be switched off".
const (
	ThinkingOffDisabled     = "disabled"
	ThinkingOffBetweenTools = "between_tools"
	ThinkingOffUnsupported  = "none"
)

// Capabilities describes what the gateway's adapter does with a provider
// type, and for the flags documented as model-level, what the model behind
// the connection accepts. The provider-level flags are about the adapter, not
// about the upstream model: a model that cannot call tools still reports
// Tools for its provider type, because the translation exists and the model's
// own refusal is relayed as it comes.
//
// The provider-level flags gate nothing. Adapters already answer with a typed
// *Error for what they cannot do, and a second, pre-flight copy of those
// rules would drift from the ones that actually run. They are a description,
// for the dashboard and the docs. The model-level flags are different: they
// pick which wire shape an adapter builds, because the models disagree on it
// (budget_tokens is a 400 on Claude 4.7+, adaptive is a 400 on 4.5 and
// earlier).
type Capabilities struct {
	// Chat is whether New returns a chat adapter for the type, i.e. whether
	// a project may route its completions through such a connection. Every
	// type had it until the rerank-only ones arrived, so the pickers that
	// used to list all connections now filter on it.
	Chat bool `json:"chat"`
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
	// Rerank is a dedicated rerank API, reached through NewReranker. The
	// types that offer one offer nothing else.
	Rerank bool `json:"rerank"`

	// The flags below are model-level: CapabilitiesFor fills them from the
	// model table, and from the provider's current generation when the model
	// is empty or not listed (see providerDefaults).
	//
	// NativeJSONSchema is a response schema the upstream enforces itself,
	// rather than one the gateway approximates with a tool or an instruction.
	NativeJSONSchema bool `json:"native_json_schema"`
	// ForcedToolChoice is whether tool_choice may name a tool or require one.
	ForcedToolChoice bool `json:"forced_tool_choice"`
	// Thinking is the mode a reasoning request is translated into.
	Thinking ThinkingMode `json:"thinking"`
	// ThinkingOff is how thinking is switched off: one of the ThinkingOff*
	// values, or empty when it does not apply.
	ThinkingOff string `json:"thinking_off"`
	// EffortLevels is the set of effort values the model accepts, lowest
	// first; empty when the model has no effort parameter.
	EffortLevels []string `json:"effort_levels,omitempty"`
	// ThinkingPrefixBound is whether the upstream checks a replayed thinking
	// block's signature against everything that preceded it (Claude 5.1+), so
	// changing the prefix invalidates the block.
	ThinkingPrefixBound bool `json:"thinking_prefix_bound"`
}

// CapabilitiesFor reports the capabilities of a provider type, refined by the
// model when one is given. It is a free function rather than a method on
// Provider because the question is static: the dashboard asks it before a
// connection with credentials exists, and building an adapter just to answer
// it would be backwards. An unknown type reports nothing.
//
// The provider-level flags never depend on the model. The model-level ones
// (NativeJSONSchema through ThinkingPrefixBound) come from the table below:
// the row whose pattern matches the model with the longest literal prefix
// wins, the same rule as the price table. An empty model, or one no row
// matches, gets the provider's current-generation defaults.
func CapabilitiesFor(providerType, model string) Capabilities {
	c := providerDefaults(providerType)
	if model == "" {
		return c
	}
	for _, r := range modelRules {
		if r.providerType == providerType && glob.Match(r.pattern, model) {
			r.flags.applyTo(&c)
			break
		}
	}
	return c
}

// modelFlags is the model-level part of Capabilities, as one unit so a table
// row replaces all of it and never leaves a stale half behind.
type modelFlags struct {
	nativeJSONSchema    bool
	forcedToolChoice    bool
	thinking            ThinkingMode
	thinkingOff         string
	effortLevels        []string
	thinkingPrefixBound bool
}

// applyTo writes the flags into c. The effort slice is copied: the table is
// shared by every caller and one of them appending to its result must not
// reach into it.
func (f modelFlags) applyTo(c *Capabilities) {
	c.NativeJSONSchema = f.nativeJSONSchema
	c.ForcedToolChoice = f.forcedToolChoice
	c.Thinking = f.thinking
	c.ThinkingOff = f.thinkingOff
	c.EffortLevels = append([]string(nil), f.effortLevels...)
	c.ThinkingPrefixBound = f.thinkingPrefixBound
}

type modelRule struct {
	providerType string
	pattern      string
	flags        modelFlags
}

// Effort level sets, lowest first.
var (
	effortLMH  = []string{"low", "medium", "high"}
	effortLMHM = []string{"low", "medium", "high", "max"}
	effortAll  = []string{"low", "medium", "high", "xhigh", "max"}
)

// providerDefaults is what a provider type reports before any model row is
// applied, and what a model no row matches keeps.
//
// The provider-level flags keep the behavior they always had. The model-level
// ones describe the provider's current generation (ADR-031), so a model
// released after this table was written works until a row is added; for
// Anthropic that is the most restrictive common denominator of the newest
// generation (Opus/Sonnet 5.5, Fable/Mythos 5.1), which no request shape
// gets a 400 from: no forced tool choice, thinking that cannot be switched
// off, and the three effort levels every model of the generation takes.
func providerDefaults(providerType string) Capabilities {
	switch providerType {
	case "openai":
		// Prompt caching is automatic upstream, so there is no marker to send.
		c := Capabilities{Chat: true, Streaming: true, Embeddings: true, Tools: true, ToolStreaming: true,
			Vision: true, RemoteImages: true, CachedTokenUsage: true}
		return withModelFlags(c, modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingPassthrough})
	case "deepseek":
		// json_object only: the upstream has no schema-enforcing response format.
		c := Capabilities{Chat: true, Streaming: true, Tools: true, ToolStreaming: true, CachedTokenUsage: true}
		return withModelFlags(c, modelFlags{forcedToolChoice: true, thinking: ThinkingPassthrough})
	case "custom_openai":
		// The server behind it is unknown; this is what the OpenAI wire format
		// allows, not a promise about a particular build.
		c := Capabilities{Chat: true, Streaming: true, Embeddings: true, Tools: true, ToolStreaming: true,
			Vision: true, RemoteImages: true}
		return withModelFlags(c, modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingPassthrough})
	case AzureProviderType:
		// The OpenAI wire format behind a deployment URL and an api-key
		// header. Azure fetches image URLs itself and reports cached prompt
		// tokens in usage like OpenAI does. The deployment name is not the
		// model name, so no model row can apply here.
		c := Capabilities{Chat: true, Streaming: true, Embeddings: true, Tools: true, ToolStreaming: true,
			Vision: true, RemoteImages: true, CachedTokenUsage: true}
		return withModelFlags(c, modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingPassthrough})
	case "anthropic":
		c := Capabilities{Chat: true, Streaming: true, Tools: true, ToolStreaming: true, Vision: true,
			RemoteImages: true, PromptCaching: true, CachedTokenUsage: true}
		return withModelFlags(c, modelFlags{nativeJSONSchema: true, thinking: ThinkingAdaptive,
			thinkingOff: ThinkingOffUnsupported, effortLevels: effortLMH, thinkingPrefixBound: true})
	case "gemini":
		// Gemini sends a function call whole inside one chunk, never as
		// argument deltas, and takes only Files API / gs:// image URIs.
		c := Capabilities{Chat: true, Streaming: true, Embeddings: true, Tools: true, Vision: true,
			CachedTokenUsage: true}
		return withModelFlags(c, modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingLevel})
	case "ollama":
		// Ollama's API has no tool_choice, so a forced choice cannot be passed.
		c := Capabilities{Chat: true, Streaming: true, Embeddings: true, Tools: true, Vision: true}
		return withModelFlags(c, modelFlags{nativeJSONSchema: true, thinking: ThinkingToggle})
	case "cohere_rerank", "voyage_rerank":
		// Rerank only: New and NewEmbedder reject these types, so a
		// connection of one can back neither a project nor a RAG store's
		// embeddings -- only rag_stores.rerank_connection_id. Chat-shaped
		// flags do not apply and stay at their zero values.
		return Capabilities{Rerank: true}
	}
	return Capabilities{}
}

func withModelFlags(c Capabilities, f modelFlags) Capabilities {
	f.applyTo(&c)
	return c
}

// modelRules is the model table, sorted once by pattern specificity. A row
// only overrides the model-level flags; the provider-level ones are never
// model-dependent.
//
// The Anthropic rows were checked on 2026-10-31 against Anthropic's pages for
// thinking (per-model configuration table), effort, structured outputs and
// tool use (forced tool_choice). Where a page lists a model ID, the pattern
// is the ID without its date suffix, so a dated snapshot and the alias both
// match.
var modelRules = sortedRules([]modelRule{
	// Claude 3 family: no structured output, no effort. Only 3.7 has a
	// thinking budget.
	{"anthropic", "claude-3*", modelFlags{forcedToolChoice: true, thinking: ThinkingNone, thinkingOff: ThinkingOffDisabled}},
	{"anthropic", "claude-3-7*", modelFlags{forcedToolChoice: true, thinking: ThinkingBudget, thinkingOff: ThinkingOffDisabled}},
	// Claude 4.0 and 4.1: extended thinking only (claude-sonnet-4-20250514,
	// claude-opus-4-20250514, claude-opus-4-1-20250805, and the -0 aliases).
	{"anthropic", "claude-sonnet-4-20*", modelFlags{forcedToolChoice: true, thinking: ThinkingBudget, thinkingOff: ThinkingOffDisabled}},
	{"anthropic", "claude-sonnet-4-0*", modelFlags{forcedToolChoice: true, thinking: ThinkingBudget, thinkingOff: ThinkingOffDisabled}},
	{"anthropic", "claude-opus-4-20*", modelFlags{forcedToolChoice: true, thinking: ThinkingBudget, thinkingOff: ThinkingOffDisabled}},
	{"anthropic", "claude-opus-4-0*", modelFlags{forcedToolChoice: true, thinking: ThinkingBudget, thinkingOff: ThinkingOffDisabled}},
	{"anthropic", "claude-opus-4-1*", modelFlags{forcedToolChoice: true, thinking: ThinkingBudget, thinkingOff: ThinkingOffDisabled}},
	// Claude 4.5: extended thinking only; Opus 4.5 alone takes effort.
	{"anthropic", "claude-sonnet-4-5*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingBudget, thinkingOff: ThinkingOffDisabled}},
	{"anthropic", "claude-haiku-4-5*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingBudget, thinkingOff: ThinkingOffDisabled}},
	{"anthropic", "claude-opus-4-5*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingBudget, thinkingOff: ThinkingOffDisabled, effortLevels: effortLMH}},
	// Claude 4.6: adaptive, with budget_tokens deprecated but accepted.
	{"anthropic", "claude-opus-4-6*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffDisabled, effortLevels: effortLMHM}},
	{"anthropic", "claude-sonnet-4-6*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffDisabled, effortLevels: effortLMHM}},
	// Claude 4.7 and 4.8: adaptive only; "enabled" is a 400.
	{"anthropic", "claude-opus-4-7*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffDisabled, effortLevels: effortAll}},
	{"anthropic", "claude-opus-4-8*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffDisabled, effortLevels: effortAll}},
	// Claude 5.0: Opus and Sonnet accept "disabled". Anthropic lists the 5.0
	// IDs as bare names (claude-opus-5); a dated snapshot is -5-YYYYMMDD.
	// The patterns are deliberately not "claude-opus-5*": that would hand a
	// future claude-opus-5-6 the 5.0 flags (forced tool choice, no prefix
	// binding) instead of the restrictive default for unknown models.
	{"anthropic", "claude-opus-5", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffDisabled, effortLevels: effortAll}},
	{"anthropic", "claude-opus-5-20*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffDisabled, effortLevels: effortAll}},
	{"anthropic", "claude-sonnet-5", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffDisabled, effortLevels: effortAll}},
	{"anthropic", "claude-sonnet-5-20*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffDisabled, effortLevels: effortAll}},
	// Claude 5.5 and the 5.1 Fable/Mythos: no forced tool choice, and thinking
	// is bound to its prefix. Opus 5.5 cannot switch thinking off, Sonnet 5.5
	// takes "between_tools" instead of "disabled", Haiku 5.5 takes
	// "disabled" (all three only at effort high or below, which is the
	// effort mapping's concern).
	{"anthropic", "claude-opus-5-5*", modelFlags{nativeJSONSchema: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffUnsupported, effortLevels: effortAll, thinkingPrefixBound: true}},
	{"anthropic", "claude-sonnet-5-5*", modelFlags{nativeJSONSchema: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffBetweenTools, effortLevels: effortAll, thinkingPrefixBound: true}},
	{"anthropic", "claude-haiku-5-5*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffDisabled, effortLevels: effortAll, thinkingPrefixBound: true}},
	// Fable and Mythos: thinking is always on. 5.0 still takes forced tool
	// choice; 5.1 does not and binds thinking to its prefix.
	{"anthropic", "claude-fable-5", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffUnsupported, effortLevels: effortAll}},
	{"anthropic", "claude-fable-5-20*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffUnsupported, effortLevels: effortAll}},
	{"anthropic", "claude-mythos-5", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffUnsupported, effortLevels: effortAll}},
	{"anthropic", "claude-mythos-5-20*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffUnsupported, effortLevels: effortAll}},
	{"anthropic", "claude-fable-5-1*", modelFlags{nativeJSONSchema: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffUnsupported, effortLevels: effortAll, thinkingPrefixBound: true}},
	{"anthropic", "claude-mythos-5-1*", modelFlags{nativeJSONSchema: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffUnsupported, effortLevels: effortAll, thinkingPrefixBound: true}},
	// Mythos Preview also takes extended thinking, but adaptive is the mode
	// to use where both exist; it has no xhigh.
	{"anthropic", "claude-mythos-preview*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingAdaptive, thinkingOff: ThinkingOffUnsupported, effortLevels: effortLMHM}},

	// Gemini 2.5 and earlier: a thinking budget (2.0 and 1.x have none).
	{"gemini", "gemini-1*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingNone}},
	{"gemini", "gemini-2.0*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingNone}},
	{"gemini", "gemini-2.5*", modelFlags{nativeJSONSchema: true, forcedToolChoice: true, thinking: ThinkingBudget}},

	// gpt-oss takes a level (low, medium, high) where other Ollama models
	// take a bool.
	{"ollama", "gpt-oss*", modelFlags{nativeJSONSchema: true, thinking: ThinkingLevel, effortLevels: effortLMH}},
})

// sortedRules orders rules by pattern specificity (exact first, then longest
// literal prefix) with the pattern text as a stable tie-break, so a lookup is
// a scan that stops at the first match.
func sortedRules(rules []modelRule) []modelRule {
	sort.SliceStable(rules, func(i, j int) bool {
		if c := glob.Compare(rules[i].pattern, rules[j].pattern); c != 0 {
			return c < 0
		}
		return rules[i].pattern < rules[j].pattern
	})
	return rules
}

// SupportsEmbeddings reports whether a provider type can back a RAG store.
func SupportsEmbeddings(providerType string) bool {
	return CapabilitiesFor(providerType, "").Embeddings
}

// SupportsChat reports whether a provider type can back a project.
func SupportsChat(providerType string) bool {
	return CapabilitiesFor(providerType, "").Chat
}

// SupportsRerank reports whether a provider type offers a rerank API.
func SupportsRerank(providerType string) bool {
	return CapabilitiesFor(providerType, "").Rerank
}
