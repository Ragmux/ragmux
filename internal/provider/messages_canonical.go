package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// The canonical half of /v1/messages (ADR-028): a Messages request headed for
// a connection that does not speak Messages is read into the OpenAI-shaped
// ChatRequest every adapter takes, and the adapter's answer is written back as
// a Messages answer (messages_response.go). What the canonical form cannot
// carry is dropped and named in a Warning (PRD rule 12), never refused.

// MessagesTranslation is a Messages request read into canonical form.
type MessagesTranslation struct {
	Request ChatRequest
	// ToolTurn reports that the last message is a user turn carrying at least
	// one tool_result. Retrieval is skipped for such a turn (ADR-024), a
	// decision taken on the original message: the canonical form splits it
	// into tool messages and a user message, and the last of those is no longer
	// a tool message.
	ToolTurn bool
	// Warnings are the Warning texts for what could not be carried over.
	Warnings []string
}

// handledMessagesFields are the top-level fields the translation reads or has
// a named reason to ignore.
var handledMessagesFields = map[string]bool{
	"model": true, "messages": true, "system": true, "max_tokens": true, "temperature": true, "top_p": true,
	"stop_sequences": true, "stream": true, "tools": true, "tool_choice": true, "thinking": true,
	"metadata": true, "output_config": true, "ragmux": true,
}

// safeFieldName is the shape of a field or block type that may be echoed in a
// Warning; anything else is counted without being named.
var safeFieldName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// MessagesToChat reads a Messages request into the canonical request. The body
// must already have had its "ragmux" field removed and its tools governed.
func MessagesToChat(b *MessagesBody) (*MessagesTranslation, error) {
	t := &MessagesTranslation{}
	c := &messagesReader{out: t, notes: map[string]*noteSet{}}
	req := &t.Request
	req.Extra = map[string]json.RawMessage{}

	if err := c.scalar(b, "model", &req.Model); err != nil {
		return nil, err
	}
	if err := c.scalar(b, "stream", &req.Stream); err != nil {
		return nil, err
	}
	if err := c.scalar(b, "max_tokens", &req.MaxTokens); err != nil {
		return nil, err
	}
	if err := c.scalar(b, "temperature", &req.Temperature); err != nil {
		return nil, err
	}
	if err := c.scalar(b, "top_p", &req.TopP); err != nil {
		return nil, err
	}
	var stops []string
	if err := c.scalar(b, "stop_sequences", &stops); err != nil {
		return nil, err
	}
	if len(stops) > 0 {
		req.Stop, _ = marshalNoEscape(stops)
	}
	if raw, ok := b.Get("metadata"); ok && !isNull(raw) {
		var md struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(raw, &md); err != nil {
			return nil, badMessages("metadata must be an object")
		}
		req.User = md.UserID
	}

	if err := c.system(b); err != nil {
		return nil, err
	}
	if err := c.messages(b); err != nil {
		return nil, err
	}
	if err := c.tools(b); err != nil {
		return nil, err
	}
	if err := c.reasoning(b); err != nil {
		return nil, err
	}

	for _, k := range b.Keys() {
		if !handledMessagesFields[k] {
			c.note("fields", k)
		}
	}
	c.flush()
	return t, nil
}

func badMessages(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: fmt.Sprintf(format, args...)}
}

func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// noteSet collects the distinct names of one kind of dropped thing.
type noteSet struct {
	names  []string
	hidden int
}

type messagesReader struct {
	out   *MessagesTranslation
	notes map[string]*noteSet
}

// noteTexts maps the kind of a dropped thing to the Warning that names them.
var noteTexts = map[string]string{
	"fields":  "request fields not supported by this connection were ignored",
	"blocks":  "content blocks not supported by this connection were dropped",
	"tools":   "server tools not supported by this connection were dropped",
	"toolfld": "tool fields not supported by this connection were ignored",
	"cite":    "citations are not supported by this connection and were ignored",
}

// noteOrder fixes the order the warnings are written in.
var noteOrder = []string{"fields", "blocks", "tools", "toolfld", "cite"}

// note records that a field, block type or tool was dropped.
func (c *messagesReader) note(kind, name string) {
	n := c.notes[kind]
	if n == nil {
		n = &noteSet{}
		c.notes[kind] = n
	}
	if !safeFieldName.MatchString(name) {
		n.hidden++
		return
	}
	for _, have := range n.names {
		if have == name {
			return
		}
	}
	n.names = append(n.names, name)
}

func (c *messagesReader) warn(text string) { c.out.Warnings = append(c.out.Warnings, text) }

// flush turns the collected notes into Warning texts.
func (c *messagesReader) flush() {
	for _, kind := range noteOrder {
		n := c.notes[kind]
		if n == nil {
			continue
		}
		sort.Strings(n.names)
		text := noteTexts[kind]
		if len(n.names) > 0 {
			text += ": " + strings.Join(n.names, ", ")
			if n.hidden > 0 {
				text += ", ..."
			}
		}
		c.warn(text)
	}
}

// scalar decodes one optional field; a value of the wrong type is a 400.
func (c *messagesReader) scalar(b *MessagesBody, key string, dst any) error {
	raw, ok := b.Get(key)
	if !ok || isNull(raw) {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return badMessages("%s has the wrong type", key)
	}
	return nil
}

// system turns the system prompt into system messages, one per block, so each
// block keeps its own cache breakpoint and the adapters see the structure the
// client wrote.
func (c *messagesReader) system(b *MessagesBody) error {
	raw, ok := b.Get("system")
	if !ok || isNull(raw) {
		return nil
	}
	req := &c.out.Request
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s != "" {
			req.Messages = append(req.Messages, Message{Role: "system", Content: TextContent(s)})
		}
		return nil
	}
	var blocks []struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
		Citations    json.RawMessage `json:"citations"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return badMessages("system must be a string or an array of text blocks")
	}
	for _, blk := range blocks {
		if blk.Type != "text" {
			c.note("blocks", blk.Type)
			continue
		}
		if len(blk.Citations) > 0 && !isNull(blk.Citations) {
			c.note("cite", "citations")
		}
		req.Messages = append(req.Messages, Message{Role: "system", Content: textContentWithCache(blk.Text, blk.CacheControl)})
	}
	return nil
}

// textContentWithCache is a text content that keeps a cache breakpoint: a
// plain string when there is none, a one-part array when there is.
func textContentWithCache(text string, cc json.RawMessage) json.RawMessage {
	if len(cc) == 0 || isNull(cc) {
		return TextContent(text)
	}
	part, _ := marshalNoEscape(struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}{"text", text, cc})
	return json.RawMessage("[" + string(part) + "]")
}

// messageBlock is one content block of a Messages request, read for every
// type the translation knows.
type messageBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Input        json.RawMessage `json:"input"`
	ToolUseID    string          `json:"tool_use_id"`
	Content      json.RawMessage `json:"content"`
	IsError      bool            `json:"is_error"`
	Source       json.RawMessage `json:"source"`
	CacheControl json.RawMessage `json:"cache_control"`
	Citations    json.RawMessage `json:"citations"`
	Thinking     *string         `json:"thinking"`
	Signature    string          `json:"signature"`
	Data         string          `json:"data"`
}

// translatedBlockTypes are the block types the translation reads fields of; a
// block of one of them that does not decode is malformed, not unknown.
var translatedBlockTypes = map[string]bool{
	"text": true, "image": true, "tool_use": true, "tool_result": true,
	thinkingBlockThinking: true, thinkingBlockRedacted: true,
}

// decodeBlocks reads a content array block by block. A block the struct above
// cannot hold (a field of a type this translation does not know: the source of
// a search_result is a string, the source of an image an object) is kept by
// its type alone, so the translation drops it with a Warning instead of
// refusing a request the provider itself accepts. ok is false only when raw is
// no array of objects.
func decodeBlocks(raw json.RawMessage) (blocks []messageBlock, ok bool) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	for _, item := range items {
		var blk messageBlock
		if err := json.Unmarshal(item, &blk); err != nil {
			var t struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(item, &t) != nil || translatedBlockTypes[t.Type] {
				// Not an object, or a type the translation reads whose own
				// fields are malformed: the provider would refuse it too.
				return nil, false
			}
			blk = messageBlock{Type: t.Type}
		}
		blocks = append(blocks, blk)
	}
	return blocks, true
}

func (c *messagesReader) messages(b *MessagesBody) error {
	raw, ok := b.Get("messages")
	if !ok || isNull(raw) {
		return badMessages("messages is required")
	}
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return badMessages("messages must be an array of messages")
	}
	if len(msgs) == 0 {
		return badMessages("messages is required")
	}
	req := &c.out.Request
	for i, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			return badMessages("messages[%d].role must be user or assistant", i)
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			req.Messages = append(req.Messages, Message{Role: m.Role, Content: TextContent(s)})
			continue
		}
		blocks, ok := decodeBlocks(m.Content)
		if !ok {
			return badMessages("messages[%d].content must be a string or an array of content blocks", i)
		}
		if m.Role == "user" {
			if err := c.userTurn(i, blocks); err != nil {
				return err
			}
			if i == len(msgs)-1 {
				for _, blk := range blocks {
					if blk.Type == "tool_result" {
						c.out.ToolTurn = true
					}
				}
			}
		} else {
			c.assistantTurn(blocks)
		}
	}
	return nil
}

// part is an OpenAI content part.
type part struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ImageURL     *partImage      `json:"image_url,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type partImage struct {
	URL string `json:"url"`
}

// imagePart converts an image block. ok is false for a source the canonical
// form cannot carry (an uploaded file id).
func imagePart(blk messageBlock) (part, bool) {
	var src AnthropicImage
	if len(blk.Source) == 0 || json.Unmarshal(blk.Source, &src) != nil {
		return part{}, false
	}
	switch src.Type {
	case "base64":
		if src.Data == "" || src.MediaType == "" {
			return part{}, false
		}
		return part{Type: "image_url", ImageURL: &partImage{URL: "data:" + src.MediaType + ";base64," + src.Data},
			CacheControl: nilIfNull(blk.CacheControl)}, true
	case "url":
		if src.URL == "" {
			return part{}, false
		}
		return part{Type: "image_url", ImageURL: &partImage{URL: src.URL}, CacheControl: nilIfNull(blk.CacheControl)}, true
	}
	return part{}, false
}

func nilIfNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || isNull(raw) {
		return nil
	}
	return raw
}

// partsContent renders content parts: a plain string for text without a cache
// breakpoint, an array otherwise.
func partsContent(parts []part) json.RawMessage {
	if len(parts) == 1 && parts[0].Type == "text" && len(parts[0].CacheControl) == 0 {
		return TextContent(parts[0].Text)
	}
	b, _ := marshalNoEscape(parts)
	return b
}

func (c *messagesReader) userTurn(i int, blocks []messageBlock) error {
	req := &c.out.Request
	var parts []part
	var results []Message
	for _, blk := range blocks {
		if len(blk.Citations) > 0 && !isNull(blk.Citations) {
			c.note("cite", "citations")
		}
		switch blk.Type {
		case "text":
			parts = append(parts, part{Type: "text", Text: blk.Text, CacheControl: nilIfNull(blk.CacheControl)})
		case "image":
			p, ok := imagePart(blk)
			if !ok {
				c.note("blocks", "image")
				continue
			}
			parts = append(parts, p)
		case "tool_result":
			content, err := c.toolResultContent(i, blk)
			if err != nil {
				return err
			}
			results = append(results, Message{Role: "tool", ToolCallID: blk.ToolUseID, Content: content, IsError: blk.IsError})
		default:
			c.note("blocks", blk.Type)
		}
	}
	// The tool results answer the assistant turn before; the text the user
	// added comes after them.
	req.Messages = append(req.Messages, results...)
	if len(parts) > 0 {
		req.Messages = append(req.Messages, Message{Role: "user", Content: partsContent(parts)})
	}
	return nil
}

func (c *messagesReader) toolResultContent(i int, blk messageBlock) (json.RawMessage, error) {
	if len(blk.Content) == 0 || isNull(blk.Content) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(blk.Content, &s) == nil {
		return TextContent(s), nil
	}
	inner, ok := decodeBlocks(blk.Content)
	if !ok {
		return nil, badMessages("messages[%d]: tool_result content must be a string or an array of blocks", i)
	}
	var parts []part
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			parts = append(parts, part{Type: "text", Text: ib.Text})
		case "image":
			p, ok := imagePart(ib)
			if !ok {
				c.note("blocks", "image")
				continue
			}
			parts = append(parts, p)
		default:
			c.note("blocks", ib.Type)
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	b, _ := marshalNoEscape(parts)
	return b, nil
}

func (c *messagesReader) assistantTurn(blocks []messageBlock) {
	req := &c.out.Request
	var text strings.Builder
	var calls []map[string]any
	var thinking []ThinkingBlock
	for _, blk := range blocks {
		if len(blk.Citations) > 0 && !isNull(blk.Citations) {
			c.note("cite", "citations")
		}
		switch blk.Type {
		case "text":
			text.WriteString(blk.Text)
		case "tool_use":
			args := string(bytes.TrimSpace(blk.Input))
			if args == "" || args == "null" {
				args = "{}"
			}
			calls = append(calls, map[string]any{"id": blk.ID, "type": "function",
				"function": map[string]any{"name": blk.Name, "arguments": args}})
		case thinkingBlockThinking:
			tb := ThinkingBlock{Type: thinkingBlockThinking, Signature: blk.Signature}
			if blk.Thinking != nil {
				tb.Thinking = *blk.Thinking
			}
			thinking = append(thinking, tb)
		case thinkingBlockRedacted:
			if blk.Data != "" {
				thinking = append(thinking, ThinkingBlock{Type: thinkingBlockRedacted, Data: blk.Data})
			}
		default:
			c.note("blocks", blk.Type)
		}
	}
	m := Message{Role: "assistant", Thinking: ThinkingField(thinking)}
	if text.Len() > 0 {
		m.Content = TextContent(text.String())
	}
	if len(calls) > 0 {
		m.ToolCalls, _ = marshalNoEscape(calls)
	}
	if m.Content == nil && m.ToolCalls == nil && m.Thinking == nil {
		return // nothing the canonical form can carry
	}
	req.Messages = append(req.Messages, m)
}

// tools translates tools and tool_choice.
func (c *messagesReader) tools(b *MessagesBody) error {
	req := &c.out.Request
	if raw, ok := b.Get("tools"); ok && !isNull(raw) {
		var tools []struct {
			Type         string          `json:"type"`
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			InputSchema  json.RawMessage `json:"input_schema"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		var rawTools []map[string]json.RawMessage
		if json.Unmarshal(raw, &tools) != nil || json.Unmarshal(raw, &rawTools) != nil {
			return badMessages("tools must be an array of tools")
		}
		var out []map[string]any
		for i, t := range tools {
			if t.Type != "" && t.Type != "custom" {
				// A tool Anthropic runs itself (web search, code execution,
				// the computer-use set): there is nothing to call elsewhere.
				c.note("tools", t.Type)
				continue
			}
			for _, f := range []string{"strict", "defer_loading", "eager_input_streaming", "allowed_callers", "input_examples"} {
				if _, ok := rawTools[i][f]; ok {
					c.note("toolfld", f)
				}
			}
			schema := t.InputSchema
			if len(schema) == 0 || isNull(schema) {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			entry := map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": schema}}
			if cc := nilIfNull(t.CacheControl); cc != nil {
				entry["cache_control"] = cc
			}
			out = append(out, entry)
		}
		if len(out) > 0 {
			req.Tools, _ = marshalNoEscape(out)
		}
	}
	raw, ok := b.Get("tool_choice")
	if !ok || isNull(raw) {
		return nil
	}
	var tc struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return badMessages("tool_choice must be an object")
	}
	if tc.DisableParallelToolUse {
		req.Extra["parallel_tool_calls"] = json.RawMessage("false")
	}
	if len(req.Tools) == 0 {
		// Every tool was a server tool: a tool_choice has nothing to govern and
		// is an error at some upstreams.
		return nil
	}
	switch tc.Type {
	case "auto":
		req.ToolChoice = json.RawMessage(`"auto"`)
	case "any":
		req.ToolChoice = json.RawMessage(`"required"`)
	case "none":
		req.ToolChoice = json.RawMessage(`"none"`)
	case "tool":
		req.ToolChoice, _ = marshalNoEscape(map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}})
	}
	return nil
}

// budgetEffort is the reasoning_effort closest to a thinking budget. The
// levels are the budgets the gateway itself uses for each (effort.go).
func budgetEffort(budget int) string {
	switch {
	case budget <= 1024:
		return "minimal"
	case budget <= 2048:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 16384:
		return "high"
	}
	return "xhigh"
}

// reasoning translates thinking and output_config: thinking becomes
// reasoning_effort (ADR-031), a JSON schema becomes response_format.
func (c *messagesReader) reasoning(b *MessagesBody) error {
	req := &c.out.Request
	var effort string
	if raw, ok := b.Get("output_config"); ok && !isNull(raw) {
		var oc struct {
			Effort string          `json:"effort"`
			Format json.RawMessage `json:"format"`
		}
		if json.Unmarshal(raw, &oc) != nil {
			return badMessages("output_config must be an object")
		}
		effort = oc.Effort
		if f := nilIfNull(oc.Format); f != nil {
			var format struct {
				Type   string          `json:"type"`
				Schema json.RawMessage `json:"schema"`
			}
			if json.Unmarshal(f, &format) != nil || format.Type != "json_schema" || len(format.Schema) == 0 {
				c.note("fields", "output_config.format")
			} else {
				req.ResponseFormat, _ = marshalNoEscape(map[string]any{"type": "json_schema",
					"json_schema": map[string]any{"name": "response", "schema": format.Schema}})
			}
		}
	}
	if raw, ok := b.Get("thinking"); ok && !isNull(raw) {
		var th struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		}
		if json.Unmarshal(raw, &th) != nil {
			return badMessages("thinking must be an object")
		}
		switch th.Type {
		case "enabled":
			effort = budgetEffort(th.BudgetTokens)
			c.warn(fmt.Sprintf("thinking.budget_tokens is approximated by reasoning_effort %q on this connection", effort))
		case "adaptive":
			if effort == "" {
				effort = "high"
			}
		case "disabled":
			effort = "none"
		}
	}
	if effort != "" {
		req.Extra["reasoning_effort"], _ = marshalNoEscape(effort)
	}
	return nil
}
