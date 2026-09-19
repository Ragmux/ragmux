package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const geminiBase = "https://generativelanguage.googleapis.com/v1beta"

type gemini struct {
	cfg  Config
	base string
}

func newGemini(cfg Config) *gemini {
	return &gemini{cfg: cfg, base: cfg.baseURL(geminiBase)}
}

// geminiModelPath returns the model name as a single, escaped URL path
// segment (a leading "models/" is dropped) so a crafted model_name cannot
// change the endpoint the request is sent to.
func geminiModelPath(model string) string {
	return url.PathEscape(strings.TrimPrefix(model, "models/"))
}

func (p *gemini) headers() map[string]string {
	return map[string]string{"x-goog-api-key": p.cfg.APIKey}
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiInline           `json:"inlineData,omitempty"`
	FileData         *geminiFileData         `json:"fileData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

// Gemini correlates tool calls by function name; there are no call ids in
// either direction.
type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// geminiTool holds every declaration: the API takes a list of tools, but
// function declarations all belong to the one function-calling tool.
type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiToolConfig struct {
	FunctionCallingConfig geminiFunctionCallingConfig `json:"functionCallingConfig"`
}

type geminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type geminiInline struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	Contents          []geminiContent   `json:"contents"`
	GenerationConfig  map[string]any    `json:"generationConfig,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
		TotalTokenCount         int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func translateGemini(ctx context.Context, cfg Config, req ChatRequest) (geminiRequest, error) {
	out := geminiRequest{}
	images := cfg.imageBudget(ctx)
	var system []string
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, m.Text())
		case "user":
			parts, err := openAIPartsToGemini(images, m.Content)
			if err != nil {
				return out, err
			}
			out.Contents = appendGemini(out.Contents, "user", parts)
		case "assistant":
			var parts []geminiPart
			if t := m.Text(); t != "" {
				parts = append(parts, geminiPart{Text: t})
			}
			parts = append(parts, geminiToolCallParts(m.ToolCalls)...)
			if len(parts) == 0 {
				continue
			}
			out.Contents = appendGemini(out.Contents, "model", parts)
		case "tool":
			name := geminiToolName(req.Messages, i, m.ToolCallID, m.Name)
			if name == "" {
				// Gemini attributes a result by function name, so a result
				// whose call is not in the history has nowhere to go. Dropping
				// it beats a 400 over one stray message.
				cfg.logger().Debug("dropping tool result without a matching tool call", "provider", "gemini",
					"tool_call_id", m.ToolCallID)
				continue
			}
			// Role "user": Content.role only takes user or model, and
			// appendGemini merges consecutive same-role contents, which is
			// exactly what parallel tool results need.
			out.Contents = appendGemini(out.Contents, "user", []geminiPart{{
				FunctionResponse: &geminiFunctionResponse{Name: name, Response: geminiToolResponse(m.Text())},
			}})
		}
	}
	if len(system) > 0 {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: strings.Join(system, "\n\n")}}}
	}
	gc := map[string]any{}
	if req.Temperature != nil {
		gc["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		gc["topP"] = *req.TopP
	}
	if n := req.MaxOutputTokens(); n > 0 {
		gc["maxOutputTokens"] = n
	}
	if stops := req.StopSequences(); len(stops) > 0 {
		gc["stopSequences"] = stops
	}
	if len(req.ResponseFormat) > 0 {
		var rf struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(req.ResponseFormat, &rf) == nil && strings.HasPrefix(rf.Type, "json") {
			gc["responseMimeType"] = "application/json"
		}
	}
	if len(gc) > 0 {
		out.GenerationConfig = gc
	}
	translateGeminiTools(cfg, req, &out)
	if len(out.Contents) == 0 {
		return out, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "messages must contain at least one user message"}
	}
	return out, nil
}

// translateGeminiTools fills in functionDeclarations and toolConfig. A tool
// whose schema Gemini cannot take is still declared: the sanitiser degrades
// the parameters rather than dropping the tool.
func translateGeminiTools(cfg Config, req ChatRequest, out *geminiRequest) {
	if len(req.Tools) == 0 {
		return
	}
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if json.Unmarshal(req.Tools, &tools) != nil {
		return
	}
	var decls []geminiFunctionDeclaration
	for _, t := range tools {
		if t.Function.Name == "" {
			continue
		}
		d := geminiFunctionDeclaration{Name: t.Function.Name, Description: t.Function.Description}
		if len(t.Function.Parameters) > 0 {
			schema, dropped := sanitizeGeminiSchema(t.Function.Parameters)
			if len(dropped) > 0 {
				cfg.logger().Debug("tool schema keywords dropped for gemini", "provider", "gemini",
					"tool", t.Function.Name, "dropped", dropped)
			}
			// A parameterless declaration is sent without the field at all:
			// several model versions reject an empty properties object.
			if !geminiSchemaEmpty(schema) {
				d.Parameters = schema
			}
		}
		decls = append(decls, d)
	}
	if len(decls) == 0 {
		return
	}
	out.Tools = []geminiTool{{FunctionDeclarations: decls}}
	out.ToolConfig = geminiToolChoice(req.ToolChoice)
}

// geminiToolChoice maps OpenAI's tool_choice to a functionCallingConfig mode.
// Unlike the Anthropic path, "none" keeps the declarations: Gemini has an
// exact equivalent, so the model still knows the tools exist.
func geminiToolChoice(raw json.RawMessage) *geminiToolConfig {
	if len(raw) == 0 {
		return nil
	}
	cfg := func(mode string, names ...string) *geminiToolConfig {
		return &geminiToolConfig{FunctionCallingConfig: geminiFunctionCallingConfig{Mode: mode, AllowedFunctionNames: names}}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return cfg("AUTO")
		case "required":
			return cfg("ANY")
		case "none":
			return cfg("NONE")
		}
		return nil
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Function.Name != "" {
		return cfg("ANY", obj.Function.Name)
	}
	return nil
}

// geminiToolCallParts turns assistant tool_calls into functionCall parts.
func geminiToolCallParts(raw json.RawMessage) []geminiPart {
	if len(raw) == 0 {
		return nil
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &calls) != nil {
		return nil
	}
	var parts []geminiPart
	for _, c := range calls {
		args := json.RawMessage(c.Function.Arguments)
		if !json.Valid(args) || !strings.HasPrefix(strings.TrimSpace(c.Function.Arguments), "{") {
			args = json.RawMessage("{}")
		}
		parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{Name: c.Function.Name, Args: args}})
	}
	return parts
}

// geminiToolName recovers the function name a tool result belongs to by
// scanning back through the assistant tool_calls that preceded it, because
// Gemini has no call ids to echo. fallback is the deprecated "name" field
// clients may still send; an empty return means the call is unknown.
func geminiToolName(msgs []Message, upTo int, callID, fallback string) string {
	for i := upTo - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		var calls []struct {
			ID       string `json:"id"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(msgs[i].ToolCalls, &calls) != nil {
			continue
		}
		for _, c := range calls {
			if c.ID == callID && c.Function.Name != "" {
				return c.Function.Name
			}
		}
	}
	return fallback
}

// geminiToolResponse wraps a tool result: Gemini requires a JSON object here
// and tool results are usually plain text.
func geminiToolResponse(text string) json.RawMessage {
	if raw := json.RawMessage(strings.TrimSpace(text)); len(raw) > 0 && raw[0] == '{' && json.Valid(raw) {
		return raw
	}
	b, _ := json.Marshal(map[string]string{"result": text})
	return b
}

func appendGemini(cs []geminiContent, role string, parts []geminiPart) []geminiContent {
	if n := len(cs); n > 0 && cs[n-1].Role == role {
		cs[n-1].Parts = append(cs[n-1].Parts, parts...)
		return cs
	}
	return append(cs, geminiContent{Role: role, Parts: parts})
}

// geminiFilesPrefix is the Files API namespace; together with gs:// it is
// everything fileData accepts. An ordinary web URL sent here is rejected
// upstream, so it has to be inlined instead.
const geminiFilesPrefix = "https://generativelanguage.googleapis.com/v1beta/files/"

func geminiFileURI(u string) bool {
	return strings.HasPrefix(u, "gs://") || strings.HasPrefix(u, geminiFilesPrefix)
}

func openAIPartsToGemini(images *imageBudget, raw json.RawMessage) ([]geminiPart, error) {
	parts, err := parseContent(raw)
	if err != nil {
		return nil, err
	}
	var out []geminiPart
	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, geminiPart{Text: p.Text})
			continue
		}
		ref := p.Image
		if geminiFileURI(ref.URL) {
			out = append(out, geminiPart{FileData: &geminiFileData{FileURI: ref.URL}})
			continue
		}
		if geminiInlineImages {
			if ref, err = images.inline(ref); err != nil {
				return nil, err
			}
		}
		switch {
		case ref.Base64 != "":
			out = append(out, geminiPart{InlineData: &geminiInline{MimeType: ref.MediaType, Data: ref.Base64}})
		case ref.URL != "":
			return nil, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error",
				Message: "gemini accepts inline base64 images (data: URLs), Files API URIs and gs:// URIs; remote image fetching is disabled"}
		}
	}
	if len(out) == 0 {
		out = []geminiPart{{Text: ""}}
	}
	return out, nil
}

func geminiFinish(reason string) *string {
	switch reason {
	case "STOP":
		return strPtr("stop")
	case "MAX_TOKENS":
		return strPtr("length")
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return strPtr("content_filter")
	case "":
		return nil
	}
	return strPtr("stop")
}

func geminiText(c geminiContent) string {
	var b strings.Builder
	for _, p := range c.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// geminiToolCalls collects the functionCall parts of one candidate.
func geminiToolCalls(c geminiContent) []toolCall {
	var calls []toolCall
	for _, p := range c.Parts {
		if p.FunctionCall != nil {
			calls = append(calls, toolCall{Name: p.FunctionCall.Name, Arguments: string(p.FunctionCall.Args)})
		}
	}
	return calls
}

func (p *gemini) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := translateGemini(ctx, p.cfg, req)
	if err != nil {
		return nil, err
	}
	var gr geminiResponse
	url := p.base + "/models/" + geminiModelPath(p.cfg.Model) + ":generateContent"
	if err := doJSON(ctx, p.cfg, url, p.headers(), body, &gr); err != nil {
		return nil, err
	}
	resp := &ChatResponse{ID: chatID(), Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Usage: geminiUsage(gr)}
	if len(gr.Candidates) == 0 {
		resp.Choices = []Choice{{Index: 0, Message: ResponseMessage{Role: "assistant", Content: strPtr("")}, FinishReason: strPtr("content_filter")}}
		return resp, nil
	}
	for i, c := range gr.Candidates {
		msg := ResponseMessage{Role: "assistant", Content: strPtr(geminiText(c.Content))}
		finish := geminiFinish(c.FinishReason)
		if msg.ToolCalls = toolCallsJSON(geminiToolCalls(c.Content)); msg.ToolCalls != nil {
			// Gemini reports STOP for a turn that only called tools; clients
			// key their tool loop off the finish reason.
			finish = strPtr("tool_calls")
		}
		resp.Choices = append(resp.Choices, Choice{Index: i, Message: msg, FinishReason: finish})
	}
	return resp, nil
}

func (p *gemini) ChatStream(ctx context.Context, req ChatRequest, out chan<- StreamChunk) error {
	body, err := translateGemini(ctx, p.cfg, req)
	if err != nil {
		return err
	}
	url := p.base + "/models/" + geminiModelPath(p.cfg.Model) + ":streamGenerateContent?alt=sse"
	resp, err := doStream(ctx, p.cfg, url, p.headers(), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	id := chatID()
	created := time.Now().Unix()
	usage := &Usage{}
	sentRole := false
	var tools toolCallStream
	emit := func(c StreamChunk) bool {
		c.ID, c.Object, c.Created, c.Model = id, "chat.completion.chunk", created, req.Model
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	err = readSSE(resp.Body, func(ev sseEvent) bool {
		var gr geminiResponse
		if json.Unmarshal([]byte(ev.Data), &gr) != nil {
			return true
		}
		if gr.UsageMetadata.TotalTokenCount > 0 {
			usage = geminiUsage(gr)
		}
		for _, c := range gr.Candidates {
			text := geminiText(c.Content)
			calls := geminiToolCalls(c.Content)
			delta := Delta{Content: strPtr(text)}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			if text != "" || len(calls) == 0 {
				if !emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: delta}}}) {
					return false
				}
				delta = Delta{}
			}
			// A functionCall always arrives complete inside one chunk, so each
			// one is a single whole delta; Gemini never streams arguments.
			for _, call := range calls {
				delta.ToolCalls = tools.Whole("", call.Name, call.Arguments)
				if !emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: delta}}}) {
					return false
				}
				delta = Delta{}
			}
			if c.FinishReason != "" {
				finish := geminiFinish(c.FinishReason)
				if tools.Len() > 0 {
					finish = strPtr("tool_calls")
				}
				if !emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{}, FinishReason: finish}}}) {
					return false
				}
			}
		}
		return true
	})
	if err != nil {
		return err
	}
	emit(StreamChunk{Choices: []StreamChoice{}, Usage: usage})
	return nil
}

// geminiUsage maps usageMetadata to the OpenAI shape. Reasoning ("thoughts")
// tokens are billed as output, so they count as completion tokens, which keeps
// prompt + completion == total like the other providers, and are reported
// again in the completion breakdown. promptTokenCount already contains the
// cached prefix, so caching only adds a breakdown, never changes the total.
func geminiUsage(gr geminiResponse) *Usage {
	u := gr.UsageMetadata
	out := &Usage{PromptTokens: u.PromptTokenCount, CompletionTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
		TotalTokens: u.TotalTokenCount}
	if u.CachedContentTokenCount > 0 {
		out.PromptTokensDetails = &PromptTokensDetails{CachedTokens: u.CachedContentTokenCount}
	}
	if u.ThoughtsTokenCount > 0 {
		out.CompletionTokensDetails = &CompletionTokensDetails{ReasoningTokens: u.ThoughtsTokenCount}
	}
	out.normalize()
	return out
}
