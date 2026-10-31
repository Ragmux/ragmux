package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// reasoning_effort -> each provider's own thinking setting (ADR-031, design
// section 5). The plans below are pure: they read the capabilities of the
// model and the client's values and return what to send plus the Warning
// texts, so the adapter that builds the request and RequestWarnings, which
// has to report the same things before the call, share one answer.

// effortOrder lists the values reasoning_effort may take, lowest first.
// "none" is below "minimal" and means thinking off; "max" is the gateway's
// own addition to OpenAI's set.
var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// effortRank is a value's position in effortOrder, or -1 for an unknown one.
func effortRank(v string) int {
	for i, e := range effortOrder {
		if e == v {
			return i
		}
	}
	return -1
}

// checkEffort rejects a value no level of the set matches. It is a 400 and not
// a warning: the field is one the gateway translates itself, so a value it
// cannot translate is a request error, and a typo ("hgih") should not quietly
// run at the provider's expensive default.
func checkEffort(v string) error {
	if effortRank(v) >= 0 {
		return nil
	}
	shown := v
	if len(shown) > 64 {
		shown = shown[:64] + "..."
	}
	return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: "invalid_reasoning_effort",
		Message: fmt.Sprintf("reasoning_effort %q is not supported; use one of %s", shown, strings.Join(effortOrder, ", "))}
}

// thinkingUnsupportedWarning is the Warning for a reasoning_effort the model
// has no thinking to apply it to. The field is dropped, not rejected (PRD
// rule 12).
func thinkingUnsupportedWarning(providerType, model string) string {
	return fmt.Sprintf("reasoning_effort is not supported by %s model %q; ignored", providerType, model)
}

// clampEffort picks the level of the model's set that serves the wanted one
// and says whether it had to come down. A level the set holds is used as
// asked; otherwise the highest level below it, and the lowest level if even
// that does not exist. minimal is low for every set here.
func clampEffort(levels []string, want string) (level string, lowered bool) {
	if want == "minimal" {
		want = "low"
	}
	rank := effortRank(want)
	best, bestRank := "", -1
	for _, l := range levels {
		r := effortRank(l)
		if l == want {
			return l, false
		}
		if r < rank && r > bestRank {
			best, bestRank = l, r
		}
	}
	if best != "" {
		return best, true
	}
	lowest := ""
	for _, l := range levels {
		if lowest == "" || effortRank(l) < effortRank(lowest) {
			lowest = l
		}
	}
	return lowest, lowest != want
}

// ---------------------------------------------------------------- Anthropic

// Token budgets of Anthropic's legacy (budget) thinking per effort. They are
// constants of the gateway, not of the model.
var anthropicBudgets = map[string]int{
	"minimal": 1024, "low": 2048, "medium": 8192, "high": 16384, "xhigh": 32768, "max": 32768,
}

const (
	// anthropicMinBudget is the smallest budget_tokens the API takes.
	anthropicMinBudget = 1024
	// anthropicAdaptiveMaxTokens is max_tokens when the client sent none and
	// the effort is high or above: the default 4096 could be spent entirely
	// on thinking.
	anthropicAdaptiveMaxTokens = 16000
)

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
	// Format is the native response schema (response_format json_schema).
	Format *anthropicOutputFormat `json:"format,omitempty"`
}

type anthropicOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

// anthropicEffortPlan is what reasoning_effort turns into on an Anthropic
// request.
type anthropicEffortPlan struct {
	Thinking *anthropicThinking
	Effort   string
	// MaxTokens is the max_tokens to send, with the client's value or the
	// gateway default already folded in.
	MaxTokens int
	// DropTemperature: thinking is on, and the API takes no temperature
	// with it.
	DropTemperature bool
	Warnings        []string
}

// dropThinking takes thinking back out of a plan that was built for it and
// restores what the request would have had without it: the client's own
// max_tokens (or the default) and temperature. The effort stays: it is a
// separate parameter (Opus 4.5 takes it next to the budget) that the reasons
// for dropping thinking do not forbid. The Warning says why. It is used for
// manual (budget) thinking next to a forced tool_choice or a tool round, and
// for any thinking next to the forced ragmux_response tool.
func (p *anthropicEffortPlan) dropThinking(maxTokens int, warning string) {
	p.Thinking, p.DropTemperature = nil, false
	p.MaxTokens = maxTokens
	if p.MaxTokens == 0 {
		p.MaxTokens = defaultMaxTokens
	}
	p.Warnings = append(p.Warnings, warning)
}

// effortPlan maps the client's effort (set says whether there was one) onto
// the thinking mode the model's capabilities describe. maxTokens is the
// client's value, 0 when it sent none.
func effortPlan(caps Capabilities, model, effort string, set bool, maxTokens int) (anthropicEffortPlan, error) {
	plan := anthropicEffortPlan{MaxTokens: maxTokens}
	if plan.MaxTokens == 0 {
		plan.MaxTokens = defaultMaxTokens
	}
	if !set {
		return plan, nil
	}
	if err := checkEffort(effort); err != nil {
		return plan, err
	}
	switch caps.Thinking {
	case ThinkingBudget:
		return budgetPlan(plan, caps, effort, maxTokens), nil
	case ThinkingAdaptive:
		return adaptivePlan(plan, caps, effort, maxTokens), nil
	}
	if effort != "none" {
		plan.Warnings = append(plan.Warnings, thinkingUnsupportedWarning("anthropic", model))
	}
	return plan, nil
}

// budgetPlan is legacy thinking: enabled + budget_tokens, plus effort where
// the model takes one (Opus 4.5) -- both are sent there.
func budgetPlan(plan anthropicEffortPlan, caps Capabilities, effort string, maxTokens int) anthropicEffortPlan {
	if effort == "none" {
		// Thinking is off unless asked for; nothing to send.
		return plan
	}
	if len(caps.EffortLevels) > 0 {
		var lowered bool
		plan.Effort, lowered = clampEffort(caps.EffortLevels, effort)
		if lowered && (effort == "xhigh" || effort == "max") {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("reasoning_effort %s is not supported as an effort by this model; using %s for the effort setting", effort, plan.Effort))
		}
	}
	budget := anthropicBudgets[effort]
	switch {
	case maxTokens == 0:
		plan.MaxTokens = budget + defaultMaxTokens
	case maxTokens <= budget:
		// budget_tokens has to stay under max_tokens; leave the answer room.
		budget = maxTokens - anthropicMinBudget
	}
	if budget < anthropicMinBudget {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"max_tokens %d leaves no room for thinking (budget_tokens must be at least %d and below max_tokens); thinking disabled for this request",
			maxTokens, anthropicMinBudget))
		return plan
	}
	plan.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	plan.DropTemperature = true
	return plan
}

// adaptivePlan is current thinking: adaptive + output_config.effort.
func adaptivePlan(plan anthropicEffortPlan, caps Capabilities, effort string, maxTokens int) anthropicEffortPlan {
	if effort == "none" {
		switch caps.ThinkingOff {
		case ThinkingOffDisabled:
			plan.Thinking = &anthropicThinking{Type: "disabled"}
			return plan
		case ThinkingOffBetweenTools:
			plan.Thinking = &anthropicThinking{Type: "between_tools"}
			return plan
		}
		effort = "low"
		plan.Warnings = append(plan.Warnings, "reasoning_effort none cannot switch thinking off on this model; using low")
	}
	plan.Thinking = &anthropicThinking{Type: "adaptive"}
	plan.DropTemperature = true
	if len(caps.EffortLevels) == 0 {
		plan.Warnings = append(plan.Warnings, "this model takes no effort setting; reasoning_effort ignored")
		return plan
	}
	level, lowered := clampEffort(caps.EffortLevels, effort)
	if lowered && (effort == "xhigh" || effort == "max") {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("reasoning_effort %s is not supported by this model; using %s", effort, level))
	}
	plan.Effort = level
	if maxTokens == 0 && effortRank(level) >= effortRank("high") {
		plan.MaxTokens = anthropicAdaptiveMaxTokens
	}
	return plan
}

// ------------------------------------------------------------------- Gemini

// Gemini 2.5 thinkingBudget per effort. xhigh and max take the model's upper
// bound; the others are clipped to the model's bounds.
var geminiBudgets = map[string]int{"minimal": 512, "low": 1024, "medium": 8192, "high": 24576}

// geminiBudgetBounds are the thinkingBudget limits of a 2.5 model and whether
// 0 (thinking off) is allowed. Pro cannot be switched off.
func geminiBudgetBounds(model string) (lo, hi int, canDisable bool) {
	m := strings.TrimPrefix(model, "models/")
	switch {
	case strings.HasPrefix(m, "gemini-2.5-pro"):
		return 128, 32768, false
	case strings.HasPrefix(m, "gemini-2.5-flash-lite"):
		return 512, 24576, true
	}
	return 0, 24576, true
}

// geminiLevelOrder lists thinkingLevel values, lowest first.
var geminiLevelOrder = []string{"minimal", "low", "medium", "high"}

func geminiLevelRank(l string) int {
	for i, v := range geminiLevelOrder {
		if v == l {
			return i
		}
	}
	return -1
}

// geminiThinkingPlan is the thinkingConfig of a Gemini request, nil when none
// is to be sent.
type geminiThinkingPlan struct {
	Config   map[string]any
	Warnings []string
}

// planGeminiThinking maps reasoning_effort onto thinkingConfig. A level model
// gets thinkingLevel, a 2.5 model thinkingBudget, never both. Thought
// summaries (includeThoughts) are requested whenever thinking is, so the
// answer can carry them as ragmux_thinking; Gemini returns none otherwise.
func planGeminiThinking(caps Capabilities, model, effort string, set bool) (geminiThinkingPlan, error) {
	var plan geminiThinkingPlan
	if !set {
		return plan, nil
	}
	if err := checkEffort(effort); err != nil {
		return plan, err
	}
	switch caps.Thinking {
	case ThinkingLevel:
		levels := caps.EffortLevels
		want := effort
		if want == "xhigh" || want == "max" {
			want = "high"
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("reasoning_effort %s is not supported by gemini; using high", effort))
		}
		if want == "none" {
			level, found := geminiPickLevel(levels, "minimal")
			if !found || level != "minimal" {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("reasoning_effort none cannot switch thinking off on this model; using %s", level))
			}
			plan.Config = map[string]any{"thinkingLevel": level}
			return plan, nil
		}
		level, _ := geminiPickLevel(levels, want)
		plan.Config = map[string]any{"thinkingLevel": level, "includeThoughts": true}
	case ThinkingBudget:
		lo, hi, canDisable := geminiBudgetBounds(model)
		if effort == "none" {
			if canDisable {
				plan.Config = map[string]any{"thinkingBudget": 0}
				return plan, nil
			}
			plan.Warnings = append(plan.Warnings, fmt.Sprintf(
				"reasoning_effort none cannot switch thinking off on this model; using the minimum budget %d", lo))
			plan.Config = map[string]any{"thinkingBudget": lo}
			return plan, nil
		}
		budget, ok := geminiBudgets[effort]
		if !ok { // xhigh, max
			budget = hi
		}
		if budget < lo {
			budget = lo
		}
		if budget > hi {
			budget = hi
		}
		plan.Config = map[string]any{"thinkingBudget": budget, "includeThoughts": true}
	default:
		if effort != "none" {
			plan.Warnings = append(plan.Warnings, thinkingUnsupportedWarning("gemini", model))
		}
	}
	return plan, nil
}

// geminiPickLevel returns the level of the set that serves want: the level
// itself when the set holds it, else the next one up, else the highest there
// is. found is false when want itself was not in the set.
func geminiPickLevel(levels []string, want string) (level string, found bool) {
	wantRank := geminiLevelRank(want)
	best, bestRank, top, topRank := "", 99, "", -1
	for _, l := range levels {
		r := geminiLevelRank(l)
		if r < 0 {
			continue
		}
		if l == want {
			return l, true
		}
		if r > wantRank && r < bestRank {
			best, bestRank = l, r
		}
		if r > topRank {
			top, topRank = l, r
		}
	}
	if best != "" {
		return best, false
	}
	if top != "" {
		return top, false
	}
	return want, false
}

// ------------------------------------------------------------------- Ollama

// ollamaThinkPlan is the think value of an Ollama request, nil when none is
// to be sent.
type ollamaThinkPlan struct {
	Think    json.RawMessage
	Warnings []string
}

// planOllamaThink maps reasoning_effort onto think: a bool for most models, a
// level for those that take one (gpt-oss). none is false in both; a model
// that ignores it (gpt-oss cannot stop thinking) keeps thinking, and the
// thinking still reaches the client.
func planOllamaThink(caps Capabilities, model, effort string, set bool) (ollamaThinkPlan, error) {
	var plan ollamaThinkPlan
	if !set {
		return plan, nil
	}
	if err := checkEffort(effort); err != nil {
		return plan, err
	}
	switch caps.Thinking {
	case ThinkingToggle:
		if effort == "none" {
			plan.Think = json.RawMessage("false")
		} else {
			plan.Think = json.RawMessage("true")
		}
	case ThinkingLevel:
		if effort == "none" {
			plan.Think = json.RawMessage("false")
			return plan, nil
		}
		level, lowered := clampEffort(caps.EffortLevels, effort)
		if lowered && (effort == "xhigh" || effort == "max") {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("reasoning_effort %s is not supported by this model; using %s", effort, level))
		}
		plan.Think, _ = json.Marshal(level)
	default:
		if effort != "none" {
			plan.Warnings = append(plan.Warnings, thinkingUnsupportedWarning("ollama", model))
		}
	}
	return plan, nil
}
