package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicBase    = "https://api.anthropic.com"
	anthropicVersion = "2023-06-01"
	defaultMaxTokens = 4096
)

type anthropic struct {
	cfg  Config
	base string
}

func newAnthropic(cfg Config) *anthropic {
	base := cfg.baseURL(anthropicBase)
	base = strings.TrimSuffix(base, "/v1")
	return &anthropic{cfg: cfg, base: base}
}

func (p *anthropic) headers() map[string]string {
	return map[string]string{
		"x-api-key":         p.cfg.APIKey,
		"anthropic-version": anthropicVersion,
	}
}

// AnthropicRequest is the Messages API payload as the adapter builds it from a
// canonical request. System is raw because the field is either a plain string
// or an array of blocks; see anthropicSystem.
//
// The Anthropic* wire types are exported because the /v1/messages endpoint
// (ADR-028) reads them in the other direction: the same blocks that carry a
// canonical request to the Messages API carry a client's Messages request into
// the canonical form and a canonical answer back out, so there is one
// definition of the wire format.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	// Thinking and OutputConfig carry reasoning_effort (effort.go).
	Thinking     *AnthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *AnthropicOutputConfig `json:"output_config,omitempty"`
}

// AnthropicMessage is one turn of a Messages request or response.
type AnthropicMessage struct {
	Role    string             `json:"role"`
	Content []AnthropicContent `json:"content"`
}

// AnthropicContent is one content block. Which fields are set depends on Type.
type AnthropicContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	// Content is a tool_result's payload: a JSON string, or an array of text
	// and image blocks when the tool returned pictures.
	Content json.RawMessage `json:"content,omitempty"`
	// IsError marks a tool_result whose tool call failed.
	IsError bool            `json:"is_error,omitempty"`
	Source  *AnthropicImage `json:"source,omitempty"`
	// CacheControl is the client's prompt-caching breakpoint, relayed
	// verbatim: the gateway does not interpret its contents.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	// Thinking, Signature and Data are the fields of thinking and
	// redacted_thinking blocks. Thinking is a pointer so a block whose text
	// is empty (Claude 5 omits it) is still sent with "thinking":"", which
	// the API expects next to the signature.
	Thinking  *string `json:"thinking,omitempty"`
	Signature string  `json:"signature,omitempty"`
	Data      string  `json:"data,omitempty"`
}

// AnthropicImage is the source of an image block.
type AnthropicImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthropicTool is a client-defined tool.
type AnthropicTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// AnthropicUsageBlock is the Messages API usage object. input_tokens counts
// only what was neither read from nor written to the cache.
type AnthropicUsageBlock struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// usage maps Anthropic's counters onto the OpenAI shape. Anthropic excludes
// both cache counters from input_tokens while OpenAI's prompt_tokens
// includes cached_tokens, so they are added in: prompt_tokens then compares
// across providers and prompt + completion stays equal to total.
func (b AnthropicUsageBlock) usage() *Usage {
	prompt := b.InputTokens + b.CacheCreationInputTokens + b.CacheReadInputTokens
	u := &Usage{PromptTokens: prompt, CompletionTokens: b.OutputTokens, TotalTokens: prompt + b.OutputTokens}
	if b.CacheReadInputTokens > 0 || b.CacheCreationInputTokens > 0 {
		u.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens:     b.CacheReadInputTokens,
			CacheWriteTokens: b.CacheCreationInputTokens,
		}
	}
	return u
}

// merge copies the non-zero counters of other over b. Newer API versions
// repeat the cache fields in message_delta; an omitted (zero) field there
// must not erase what message_start already reported.
func (b *AnthropicUsageBlock) merge(other AnthropicUsageBlock) {
	for _, f := range []struct{ dst, src *int }{
		{&b.InputTokens, &other.InputTokens},
		{&b.OutputTokens, &other.OutputTokens},
		{&b.CacheCreationInputTokens, &other.CacheCreationInputTokens},
		{&b.CacheReadInputTokens, &other.CacheReadInputTokens},
	} {
		if *f.src != 0 {
			*f.dst = *f.src
		}
	}
}

// AnthropicResponse is a Messages API answer.
type AnthropicResponse struct {
	ID         string              `json:"id"`
	Type       string              `json:"type"`
	Role       string              `json:"role"`
	Model      string              `json:"model"`
	Content    []AnthropicContent  `json:"content"`
	StopReason string              `json:"stop_reason"`
	Usage      AnthropicUsageBlock `json:"usage"`
}

// anthropicThinkingSetup is every decision the adapter takes about thinking
// for one request. translateAnthropic applies it and RequestWarnings reports
// it, from this one computation, so the two cannot disagree.
type anthropicThinkingSetup struct {
	plan anthropicEffortPlan
	// stripHistory: earlier turns' thinking blocks are not sent back, because
	// the model binds them to a prefix that retrieval changes (ADR-031).
	stripHistory bool
	// dropTopP: the API takes top_p with thinking only from 0.95 up.
	dropTopP bool
	// format is how response_format is carried (anthropic_format.go).
	format anthropicFormatPlan
	// autoToolChoice: the client asked for a forced tool (required, or one
	// named) and the model cannot take that, so tool_choice becomes auto.
	autoToolChoice bool
	warnings       []string
}

// The Warning texts of the rules the adapter applies so that a request that
// worked before thinking was translated does not turn into an upstream 400
// (ADR-031). The /v1/messages native path applies the same rules to the
// client's own body and says the same things.
const (
	warnForcedToolAuto        = "tool_choice required or naming a tool is not supported by this model; auto is used"
	warnThinkingForcedTool    = "thinking disabled for this request: a forced tool_choice cannot be combined with thinking on this model"
	warnThinkingToolRound     = "thinking disabled for this request: it continues a tool round whose assistant turn has no thinking block to send back, which this model requires while thinking is on; send back ragmux_thinking to keep thinking across tool calls"
	warnTopPThinking          = "top_p below 0.95 is not supported while thinking is enabled; ignored"
	warnThinkingPrefixDropped = "thinking blocks of earlier turns were dropped: this model binds them to the prompt prefix, which retrieval changes"
)

// anthropicMinThinkingTopP is the lowest top_p the API accepts while thinking
// is on.
const anthropicMinThinkingTopP = 0.95

// forcedToolChoice reports a tool_choice that makes the model call a tool
// (required, or one named), which only counts when there are tools to call.
func forcedToolChoice(req ChatRequest) bool {
	if len(req.Tools) == 0 || len(req.ToolChoice) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(req.ToolChoice, &s) == nil {
		return s == "required"
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	return json.Unmarshal(req.ToolChoice, &obj) == nil && obj.Function.Name != ""
}

// toolTurnWithoutThinking reports a request that ends in a tool result whose
// assistant turn carries no thinking block the adapter could send back. The
// messages were validated before.
func toolTurnWithoutThinking(msgs []Message) bool {
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "tool" {
		return false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		blocks, err := parseThinkingBlocks(msgs[i].Thinking)
		return err != nil || len(replayableThinking(blocks)) == 0
	}
	return false
}

func setupAnthropicThinking(model string, req ChatRequest) (anthropicThinkingSetup, error) {
	var su anthropicThinkingSetup
	if err := ValidateRequestThinking(req.Messages); err != nil {
		return su, err
	}
	caps := CapabilitiesFor("anthropic", model)
	effort, set := req.ReasoningEffort()
	plan, err := effortPlan(caps, model, effort, set, req.MaxOutputTokens())
	if err != nil {
		return su, err
	}
	if su.format, err = planAnthropicFormat(caps, req); err != nil {
		return su, err
	}
	// The forced ragmux_response call is a forced tool_choice, which the API
	// refuses next to thinking. Only the tool path forces; the native schema
	// and the instruction leave thinking alone.
	// Thinking already switched off ("disabled", from reasoning_effort none)
	// is what the request asked for and sits next to a forced tool.
	if su.format.toolMode() && plan.Thinking != nil && plan.Thinking.Type != "disabled" {
		plan.dropThinking(req.MaxOutputTokens(), anthropicFormatThinkingWarning)
	}
	if !caps.ForcedToolChoice && forcedToolChoice(req) {
		su.autoToolChoice = true
		su.warnings = append(su.warnings, warnForcedToolAuto)
	}
	su.warnings = append(su.warnings, su.format.warnings...)
	if plan.Thinking != nil && plan.Thinking.Type == "enabled" {
		// Manual (budget) thinking has two rules the adaptive mode does not:
		// the API refuses a forced tool_choice next to it, and a tool round
		// must continue an assistant turn that starts with a thinking block.
		// Either would turn a request that worked before reasoning_effort was
		// translated into a 400, so thinking is the part that gives way.
		switch {
		case forcedToolChoice(req):
			plan.dropThinking(req.MaxOutputTokens(), warnThinkingForcedTool)
		case toolTurnWithoutThinking(req.Messages):
			plan.dropThinking(req.MaxOutputTokens(), warnThinkingToolRound)
		}
	}
	su.plan = plan
	su.warnings = append(su.warnings, plan.Warnings...)
	if plan.DropTemperature && req.Temperature != nil {
		su.warnings = append(su.warnings, "temperature is not supported while thinking is enabled; ignored")
	}
	if plan.DropTemperature && req.TopP != nil && *req.TopP < anthropicMinThinkingTopP {
		su.dropTopP = true
		su.warnings = append(su.warnings, warnTopPThinking)
	}
	if req.RAGActive && caps.ThinkingPrefixBound && hasReplayableThinking(req.Messages) {
		su.stripHistory = true
		su.warnings = append(su.warnings, warnThinkingPrefixDropped)
	}
	if hasReasoningContent(req.Messages) {
		su.warnings = append(su.warnings, reasoningContentDropped("anthropic"))
	}
	return su, nil
}

// translateAnthropic converts an OpenAI request into the Messages format.
func translateAnthropic(ctx context.Context, cfg Config, req ChatRequest) (AnthropicRequest, error) {
	out := AnthropicRequest{Model: cfg.Model, MaxTokens: defaultMaxTokens}
	su, err := setupAnthropicThinking(cfg.Model, req)
	if err != nil {
		return out, err
	}
	images := cfg.imageBudget(ctx)
	out.MaxTokens = su.plan.MaxTokens
	out.Thinking = su.plan.Thinking
	if su.plan.Effort != "" {
		out.OutputConfig = &AnthropicOutputConfig{Effort: su.plan.Effort}
	}
	if su.format.mode == formatNative {
		if out.OutputConfig == nil {
			out.OutputConfig = &AnthropicOutputConfig{}
		}
		out.OutputConfig.Format = &AnthropicOutputFormat{Type: "json_schema", Schema: su.format.schema}
	}
	if !su.plan.DropTemperature {
		out.Temperature = req.Temperature
	}
	if !su.dropTopP {
		out.TopP = req.TopP
	}
	out.StopSequences = req.StopSequences()

	var system []AnthropicContent
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, AnthropicContent{Type: "text", Text: m.Text(), CacheControl: contentCacheControl(m.Content)})
		case "user":
			parts, err := openAIPartsToAnthropic(images, m.Content)
			if err != nil {
				return out, err
			}
			out.Messages = appendAnthropic(out.Messages, "user", parts)
		case "assistant":
			var parts []AnthropicContent
			if !su.stripHistory {
				// Signed blocks go first, in their order and unchanged: the
				// API refuses a tool-use turn whose thinking was altered or
				// left out. Blocks without a signature are another
				// provider's and are not sent.
				blocks, _ := parseThinkingBlocks(m.Thinking) // validated above
				for _, b := range replayableThinking(blocks) {
					c := AnthropicContent{Type: b.Type, Signature: b.Signature, Data: b.Data}
					if b.Type == thinkingBlockThinking {
						text := b.Thinking
						c.Thinking = &text
					}
					parts = append(parts, c)
				}
			}
			replayed := len(parts)
			if t := m.Text(); t != "" {
				parts = append(parts, AnthropicContent{Type: "text", Text: t})
			}
			if len(m.ToolCalls) > 0 {
				var calls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				}
				if err := json.Unmarshal(m.ToolCalls, &calls); err == nil {
					for _, c := range calls {
						args := json.RawMessage(c.Function.Arguments)
						if !json.Valid(args) {
							args = json.RawMessage("{}")
						}
						parts = append(parts, AnthropicContent{Type: "tool_use", ID: c.ID, Name: c.Function.Name, Input: args})
					}
				}
			}
			if len(parts) == replayed {
				// Nothing but thinking is not a turn.
				continue
			}
			out.Messages = appendAnthropic(out.Messages, "assistant", parts)
		case "tool":
			result, err := toolResultContent(images, m)
			if err != nil {
				return out, err
			}
			out.Messages = appendAnthropic(out.Messages, "user", []AnthropicContent{result})
		}
	}
	if su.format.mode == formatInstruction {
		system = append(system, AnthropicContent{Type: "text", Text: anthropicJSONInstruction})
	}
	if len(system) > 0 {
		sys, err := anthropicSystem(system)
		if err != nil {
			return out, err
		}
		out.System = sys
	}
	if len(req.Tools) > 0 {
		var tools []struct {
			Type         string          `json:"type"`
			CacheControl json.RawMessage `json:"cache_control"`
			Function     struct {
				Name         string          `json:"name"`
				Description  string          `json:"description"`
				Parameters   json.RawMessage `json:"parameters"`
				CacheControl json.RawMessage `json:"cache_control"`
			} `json:"function"`
		}
		if err := json.Unmarshal(req.Tools, &tools); err == nil {
			for _, t := range tools {
				schema := t.Function.Parameters
				if len(schema) == 0 {
					schema = json.RawMessage(`{"type":"object","properties":{}}`)
				}
				// Clients spell the breakpoint either on the tool object or
				// inside function, depending on which SDK wrote the request.
				cc := t.CacheControl
				if len(cc) == 0 {
					cc = t.Function.CacheControl
				}
				out.Tools = append(out.Tools, AnthropicTool{Name: t.Function.Name, Description: t.Function.Description,
					InputSchema: schema, CacheControl: cc})
			}
		}
		if len(req.ToolChoice) > 0 {
			var s string
			if json.Unmarshal(req.ToolChoice, &s) == nil {
				switch s {
				case "auto":
					out.ToolChoice = json.RawMessage(`{"type":"auto"}`)
				case "required":
					if su.autoToolChoice {
						out.ToolChoice = json.RawMessage(`{"type":"auto"}`)
					} else {
						out.ToolChoice = json.RawMessage(`{"type":"any"}`)
					}
				case "none":
					out.Tools = nil
				}
			} else {
				var obj struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				if json.Unmarshal(req.ToolChoice, &obj) == nil && obj.Function.Name != "" {
					choice := map[string]string{"type": "tool", "name": obj.Function.Name}
					if su.autoToolChoice {
						choice = map[string]string{"type": "auto"}
					}
					b, _ := json.Marshal(choice)
					out.ToolChoice = b
				}
			}
		}
	}
	if su.format.toolMode() {
		if err := userToolClash(out.Tools); err != nil {
			return out, err
		}
		su.format.applyToolChoice(&out)
	}
	if len(out.Messages) == 0 {
		return out, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "messages must contain at least one user message"}
	}
	return out, nil
}

// RequestWarnings implements RequestWarner.
func (p *anthropic) RequestWarnings(req ChatRequest) []string {
	// A request the adapter will reject has nothing to warn about.
	if su, err := setupAnthropicThinking(p.cfg.Model, req); err == nil {
		return su.warnings
	}
	return nil
}

// appendAnthropic merges consecutive same-role messages, which the Messages
// API requires.
func appendAnthropic(msgs []AnthropicMessage, role string, parts []AnthropicContent) []AnthropicMessage {
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, parts...)
		return msgs
	}
	return append(msgs, AnthropicMessage{Role: role, Content: parts})
}

// anthropicSystem renders the system blocks. Without a single cache_control
// the value is the joined plain string Anthropic has always been sent, so a
// request that does not ask for caching keeps its previous wire shape byte
// for byte; one breakpoint switches to the block array that can carry it.
func anthropicSystem(blocks []AnthropicContent) (json.RawMessage, error) {
	texts := make([]string, len(blocks))
	cached := false
	for i, b := range blocks {
		texts[i] = b.Text
		if len(b.CacheControl) > 0 {
			cached = true
		}
	}
	if !cached {
		return json.Marshal(strings.Join(texts, "\n\n"))
	}
	return json.Marshal(blocks)
}

// contentCacheControl returns the cache_control a client attached to a
// parts-array content, or nil. The last marker wins: Anthropic caches the
// prefix up to and including the marked block, so with several markers in
// one message the final one is the breakpoint that covers all of it.
func contentCacheControl(raw json.RawMessage) json.RawMessage {
	parts, err := parseContent(raw, imageMediaTypesFor("anthropic"))
	if err != nil {
		return nil
	}
	var cc json.RawMessage
	for _, p := range parts {
		if len(p.CacheControl) > 0 {
			cc = p.CacheControl
		}
	}
	return cc
}

func openAIPartsToAnthropic(images *imageBudget, raw json.RawMessage) ([]AnthropicContent, error) {
	parts, err := parseContent(raw, images.accepted())
	if err != nil {
		return nil, err
	}
	var out []AnthropicContent
	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, AnthropicContent{Type: "text", Text: p.Text, CacheControl: p.CacheControl})
			continue
		}
		ref := p.Image
		if anthropicInlineImages {
			if ref, err = images.inline(ref); err != nil {
				return nil, err
			}
		}
		switch {
		case ref.Base64 != "":
			out = append(out, AnthropicContent{Type: "image", CacheControl: p.CacheControl,
				Source: &AnthropicImage{Type: "base64", MediaType: ref.MediaType, Data: ref.Base64}})
		case ref.URL != "":
			out = append(out, AnthropicContent{Type: "image", CacheControl: p.CacheControl,
				Source: &AnthropicImage{Type: "url", URL: ref.URL}})
		}
	}
	if len(out) == 0 {
		out = []AnthropicContent{{Type: "text", Text: ""}}
	}
	return out, nil
}

// toolResultContent builds the tool_result block for a role "tool" message.
// The content stays a plain string, byte for byte what it was before this
// carried anything else, unless the tool returned images: then it is the
// array of text and image blocks the Messages API takes there. The failure
// flag (Message.IsError) becomes is_error.
func toolResultContent(images *imageBudget, m Message) (AnthropicContent, error) {
	block := AnthropicContent{Type: "tool_result", ToolUseID: m.ToolCallID, IsError: m.IsError}
	if bytes.HasPrefix(bytes.TrimSpace(m.Content), []byte("[")) {
		parts, err := openAIPartsToAnthropic(images, m.Content)
		if err != nil {
			return block, err
		}
		hasImage := false
		for i := range parts {
			hasImage = hasImage || parts[i].Type == "image"
			// A breakpoint on a nested block is not one the API takes there;
			// before images were carried at all it was dropped with the rest.
			parts[i].CacheControl = nil
		}
		if hasImage {
			raw, err := json.Marshal(parts)
			if err != nil {
				return block, err
			}
			block.Content = raw
			return block, nil
		}
	}
	if t := m.Text(); t != "" {
		raw, err := json.Marshal(t)
		if err != nil {
			return block, err
		}
		block.Content = raw
	}
	return block, nil
}

// maxLoggedStopReason bounds an unknown stop_reason in the log line; the
// value comes from the upstream and is not otherwise trusted.
const maxLoggedStopReason = 64

// anthropicFinish maps a stop_reason to an OpenAI finish_reason. A value it
// does not know becomes "stop" -- OpenAI clients switch on a closed set, and
// an unfamiliar string there breaks them -- and is logged at Warn so a new
// upstream reason is noticed. The log line carries only the value, never
// content.
func anthropicFinish(log *slog.Logger, stop string) *string {
	switch stop {
	case "end_turn", "stop_sequence":
		return strPtr("stop")
	case "max_tokens", "model_context_window_exceeded":
		return strPtr("length")
	case "refusal":
		return strPtr("content_filter")
	case "tool_use":
		return strPtr("tool_calls")
	case "":
		return nil
	}
	if len(stop) > maxLoggedStopReason {
		stop = stop[:maxLoggedStopReason]
	}
	log.Warn("unknown stop_reason mapped to stop", "provider", "anthropic", "stop_reason", stop)
	return strPtr("stop")
}

// anthropicFormatOf recomputes the response_format plan Chat and ChatStream
// read the answer with. translateAnthropic already planned the same request
// and accepted it, so an error here cannot happen.
func anthropicFormatOf(model string, req ChatRequest) anthropicFormatPlan {
	plan, _ := planAnthropicFormat(CapabilitiesFor("anthropic", model), req)
	return plan
}

func (p *anthropic) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := translateAnthropic(ctx, p.cfg, req)
	if err != nil {
		return nil, err
	}
	var ar AnthropicResponse
	if err := doJSON(ctx, p.cfg, opChat, p.base+"/v1/messages", p.headers(), body, &ar); err != nil {
		return nil, err
	}
	format := anthropicFormatOf(p.cfg.Model, req)
	var structured *string
	var text strings.Builder
	var calls []toolCall
	var thinking []ThinkingBlock
	for _, c := range ar.Content {
		switch c.Type {
		case thinkingBlockThinking:
			b := ThinkingBlock{Type: thinkingBlockThinking, Signature: c.Signature}
			if c.Thinking != nil {
				b.Thinking = *c.Thinking
			}
			thinking = append(thinking, b)
		case thinkingBlockRedacted:
			thinking = append(thinking, ThinkingBlock{Type: thinkingBlockRedacted, Data: c.Data})
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			if format.toolMode() && c.Name == ragmuxResponseTool {
				// The structured answer is the message content, not a call.
				structured = strPtr(format.answer(c.Input))
				continue
			}
			calls = append(calls, toolCall{ID: c.ID, Name: c.Name, Arguments: string(c.Input)})
		}
	}
	content, finish := strPtr(text.String()), anthropicFinish(p.cfg.logger(), ar.StopReason)
	if structured != nil {
		content = structured
		if len(calls) == 0 && ar.StopReason == "tool_use" {
			finish = strPtr("stop")
		}
	}
	msg := ResponseMessage{Role: "assistant", Content: content, ToolCalls: toolCallsJSON(calls), Thinking: thinking}
	id := ar.ID
	if id == "" {
		id = chatID()
	}
	return &ChatResponse{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage:   ar.Usage.usage(),
	}, nil
}

func (p *anthropic) ChatStream(ctx context.Context, req ChatRequest, out chan<- StreamChunk) error {
	body, err := translateAnthropic(ctx, p.cfg, req)
	if err != nil {
		return err
	}
	body.Stream = true
	resp, err := doStream(ctx, p.cfg, p.base+"/v1/messages", p.headers(), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	id := chatID()
	created := time.Now().Unix()
	var usage AnthropicUsageBlock
	emit := func(c StreamChunk) bool {
		c.ID, c.Object, c.Created, c.Model = id, "chat.completion.chunk", created, req.Model
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// emitThinking sends one ragmux_thinking update. The index is Anthropic's
	// own content-block index, so the client can merge by it.
	emitThinking := func(b ThinkingBlock) bool {
		return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{Thinking: []ThinkingBlock{b}}}}})
	}
	// Tool_use blocks are tracked by content index so argument deltas map to
	// the right OpenAI tool_calls index.
	var tools toolCallStream
	var streamErr error
	sentRole := false
	// A ragmux_response call is the answer itself: its argument fragments go
	// out as content, by content-block index.
	format := anthropicFormatOf(p.cfg.Model, req)
	answers := map[int]*formatStream{}
	answered := false
	emitContent := func(text string) bool {
		if text == "" {
			return true
		}
		return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{Content: strPtr(text)}}}})
	}

	err = readSSE(resp.Body, func(ev sseEvent) bool {
		var base struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(ev.Data), &base) != nil {
			return true
		}
		switch base.Type {
		case "message_start":
			var ms struct {
				Message struct {
					ID    string              `json:"id"`
					Usage AnthropicUsageBlock `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(ev.Data), &ms) == nil {
				if ms.Message.ID != "" {
					id = ms.Message.ID
				}
				usage.merge(ms.Message.Usage)
			}
			sentRole = true
			return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{Role: "assistant", Content: strPtr("")}}}})
		case "content_block_start":
			var cb struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
					Data string `json:"data"`
				} `json:"content_block"`
			}
			if json.Unmarshal([]byte(ev.Data), &cb) != nil {
				return true
			}
			switch cb.ContentBlock.Type {
			case "tool_use":
				if format.toolMode() && cb.ContentBlock.Name == ragmuxResponseTool {
					answers[cb.Index] = newFormatStream(format.wrapped)
					answered = true
					return true
				}
				tc := tools.Open(cb.Index, cb.ContentBlock.ID, cb.ContentBlock.Name)
				return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{ToolCalls: tc}}}})
			case thinkingBlockThinking:
				return emitThinking(ThinkingBlock{Index: &cb.Index, Type: thinkingBlockThinking})
			case thinkingBlockRedacted:
				// A redacted block arrives whole; there are no deltas for it.
				return emitThinking(ThinkingBlock{Index: &cb.Index, Type: thinkingBlockRedacted, Data: cb.ContentBlock.Data})
			}
			return true
		case "content_block_delta":
			var d struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
					Thinking    string `json:"thinking"`
					Signature   string `json:"signature"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(ev.Data), &d) != nil {
				return true
			}
			switch d.Delta.Type {
			case "text_delta":
				delta := Delta{Content: strPtr(d.Delta.Text)}
				if !sentRole {
					delta.Role = "assistant"
					sentRole = true
				}
				return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: delta}}})
			case "thinking_delta":
				return emitThinking(ThinkingBlock{Index: &d.Index, Type: thinkingBlockThinking, Thinking: d.Delta.Thinking})
			case "signature_delta":
				return emitThinking(ThinkingBlock{Index: &d.Index, Type: thinkingBlockThinking, Signature: d.Delta.Signature})
			case "input_json_delta":
				if a, ok := answers[d.Index]; ok {
					return emitContent(a.Feed(d.Delta.PartialJSON))
				}
				tc := tools.Args(d.Index, d.Delta.PartialJSON)
				if tc == nil {
					return true
				}
				return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{ToolCalls: tc}}}})
			}
			return true
		case "content_block_stop":
			var cs struct {
				Index int `json:"index"`
			}
			if json.Unmarshal([]byte(ev.Data), &cs) != nil {
				return true
			}
			if a, ok := answers[cs.Index]; ok {
				delete(answers, cs.Index)
				return emitContent(a.Flush())
			}
			return true
		case "message_delta":
			var md struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage AnthropicUsageBlock `json:"usage"`
			}
			if json.Unmarshal([]byte(ev.Data), &md) != nil {
				return true
			}
			usage.merge(md.Usage)
			finish := anthropicFinish(p.cfg.logger(), md.Delta.StopReason)
			if answered && tools.Len() == 0 && md.Delta.StopReason == "tool_use" {
				finish = strPtr("stop")
			}
			return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{}, FinishReason: finish}}})
		case "message_stop":
			// Final usage-only chunk, mirroring OpenAI's include_usage behaviour.
			return emit(StreamChunk{Choices: []StreamChoice{}, Usage: usage.usage()})
		case "error":
			var e struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal([]byte(ev.Data), &e) // best effort; an empty message is still an error
			streamErr = &Error{Status: http.StatusBadGateway, Type: e.Error.Type, Message: RedactWith(e.Error.Message, p.cfg.APIKey)}
			return false
		}
		return true
	})
	if streamErr != nil {
		return streamErr
	}
	return err
}
