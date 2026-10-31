package provider

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// ragmuxResponseTool is the name of the synthetic tool that carries a
// structured answer on models without a native response schema (ADR-029).
const ragmuxResponseTool = "ragmux_response"

// anthropicJSONInstruction is the one sentence appended to the system prompt
// when json_object can only be approximated.
const anthropicJSONInstruction = "Respond with a single valid JSON object and nothing else."

const (
	anthropicJSONObjectWarning     = "response_format json_object is approximated by an instruction on this model"
	anthropicFormatThinkingWarning = "thinking disabled for this request: response_format is enforced through a forced tool call, which cannot be combined with thinking on this model"
)

// formatMode is how a response_format reaches the Messages API.
type formatMode int

const (
	// formatNone: no JSON response_format on the request.
	formatNone formatMode = iota
	// formatNative: output_config.format carries the schema.
	formatNative
	// formatTool: a forced ragmux_response tool carries the answer.
	formatTool
	// formatInstruction: a sentence in the system prompt asks for JSON.
	formatInstruction
)

// anthropicFormatPlan is every decision about response_format for one request.
// translateAnthropic builds the request from it and Chat/ChatStream read the
// answer back with it, so the two halves cannot disagree.
type anthropicFormatPlan struct {
	mode formatMode
	// schema is the client's schema as sent (formatNative) or the tool's
	// input_schema (formatTool, wrapped when the root is not an object).
	schema json.RawMessage
	// wrapped: the answer sits under {"value": ...} in the tool input.
	wrapped  bool
	warnings []string
}

// toolMode reports whether the answer comes back as a ragmux_response call.
func (f anthropicFormatPlan) toolMode() bool { return f.mode == formatTool }

// planAnthropicFormat decides how req.ResponseFormat is carried on a model
// with the given capabilities. "text" and unknown types plan nothing: the
// Messages API answers in prose anyway. A json_schema with no usable schema
// is the client's mistake and a 400, as at OpenAI.
func planAnthropicFormat(caps Capabilities, req ChatRequest) (anthropicFormatPlan, error) {
	var plan anthropicFormatPlan
	if len(req.ResponseFormat) == 0 {
		return plan, nil
	}
	var rf struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if json.Unmarshal(req.ResponseFormat, &rf) != nil {
		return plan, nil
	}
	switch rf.Type {
	case "json_schema":
		var schema json.RawMessage
		if rf.JSONSchema != nil {
			schema = rf.JSONSchema.Schema
		}
		root, ok := schemaRoot(schema)
		if !ok {
			return plan, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error",
				Message: "response_format.json_schema.schema must be a JSON object"}
		}
		// A model without a native schema is forced through the tool
		// (ADR-029). The second operand is unreachable: no Anthropic row has
		// neither native_json_schema nor forced_tool_choice, and
		// TestAnthropicModelsCanEnforceJSONSchema fails if one is added. It
		// only keeps this function from ever planning a forced tool the model
		// would answer with a 400.
		if caps.NativeJSONSchema || !caps.ForcedToolChoice {
			plan.mode, plan.schema = formatNative, schema
			return plan, nil
		}
		plan.mode = formatTool
		if isObjectSchema(root) {
			plan.schema = schema
			return plan, nil
		}
		plan.wrapped = true
		plan.schema = wrapSchema(schema)
	case "json_object":
		if caps.ForcedToolChoice {
			plan.mode, plan.schema = formatTool, json.RawMessage(`{"type":"object"}`)
			return plan, nil
		}
		plan.mode = formatInstruction
		plan.warnings = append(plan.warnings, anthropicJSONObjectWarning)
	}
	return plan, nil
}

// schemaRoot parses a schema that must be a JSON object.
func schemaRoot(schema json.RawMessage) (map[string]json.RawMessage, bool) {
	var root map[string]json.RawMessage
	if len(schema) == 0 || json.Unmarshal(schema, &root) != nil || root == nil {
		return nil, false
	}
	return root, true
}

// isObjectSchema reports a root that describes an object: "type":"object", or
// no type but properties. A union ("type":["object","null"]), anyOf and $ref
// roots are wrapped, because the tool's input_schema must itself be an object.
func isObjectSchema(root map[string]json.RawMessage) bool {
	t, ok := root["type"]
	if !ok {
		_, hasProps := root["properties"]
		return hasProps
	}
	var s string
	return json.Unmarshal(t, &s) == nil && s == "object"
}

func wrapSchema(schema json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           map[string]json.RawMessage{"value": schema},
		"required":             []string{"value"},
		"additionalProperties": false,
	})
	return b
}

// tool is the synthetic tool the request carries in formatTool mode.
func (f anthropicFormatPlan) tool() anthropicTool {
	return anthropicTool{Name: ragmuxResponseTool,
		Description: "Return the final answer as this tool's input.", InputSchema: f.schema}
}

// userToolClash is the 400 for a client tool that already uses the name.
func userToolClash(tools []anthropicTool) error {
	for _, t := range tools {
		if t.Name == ragmuxResponseTool {
			return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error",
				Message: "tool name " + ragmuxResponseTool + " is reserved by the gateway for response_format"}
		}
	}
	return nil
}

// applyToolChoice sets the tool_choice of a formatTool request whose client
// tools are already in out.Tools and whose own tool_choice, translated, is in
// out.ToolChoice, then appends ragmux_response. With no client tool left (none
// sent, or tool_choice "none") the synthetic tool is the forced one. A
// client tool the caller named stays forced: that turn answers with the tool
// call and not with the structured output. Otherwise the model must call some
// tool, one at a time, and ragmux_response is how it answers in prose.
func (f anthropicFormatPlan) applyToolChoice(out *anthropicRequest) {
	named := false
	if len(out.ToolChoice) > 0 {
		var c struct {
			Type string `json:"type"`
		}
		named = json.Unmarshal(out.ToolChoice, &c) == nil && c.Type == "tool"
	}
	switch {
	case len(out.Tools) == 0:
		out.ToolChoice = json.RawMessage(`{"type":"tool","name":"` + ragmuxResponseTool + `"}`)
	case named:
	default:
		out.ToolChoice = json.RawMessage(`{"type":"any","disable_parallel_tool_use":true}`)
	}
	out.Tools = append(out.Tools, f.tool())
}

// answer turns the input of a ragmux_response call into message content.
func (f anthropicFormatPlan) answer(input json.RawMessage) string {
	in := bytes.TrimSpace(input)
	if len(in) == 0 {
		return "{}"
	}
	if !f.wrapped {
		return string(in)
	}
	var w struct {
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(in, &w) == nil && len(w.Value) > 0 {
		return string(w.Value)
	}
	return string(in)
}

// formatStream turns the partial_json fragments of one ragmux_response call
// into content deltas. Fragments can be cut anywhere, so the cuts are made on
// the buffer: a wrapper prefix is held until it is complete and dropped, and
// the end of the text is held back until the block stops, because only then is
// it known which closing brace is the wrapper's.
type formatStream struct {
	wrapped bool
	// prefix: the {"value": opener has been consumed (or none is expected).
	prefix bool
	buf    string
	// sent: some content has been handed out, so Flush need not make up an
	// empty object.
	sent bool
}

func newFormatStream(wrapped bool) *formatStream {
	return &formatStream{wrapped: wrapped, prefix: !wrapped}
}

// Feed takes one fragment and returns what can be sent now.
func (s *formatStream) Feed(frag string) string {
	out := s.feed(frag)
	s.sent = s.sent || out != ""
	return out
}

func (s *formatStream) feed(frag string) string {
	if !s.wrapped {
		return frag
	}
	s.buf += frag
	if !s.prefix {
		n, ok := wrapperPrefixLen(s.buf)
		if !ok {
			return ""
		}
		s.buf, s.prefix = s.buf[n:], true
	}
	// Hold trailing whitespace and the brace before it: any of it may be the
	// wrapper's end. Whatever precedes is the value's.
	end := len(strings.TrimRight(s.buf, " \t\r\n"))
	if end > 0 && s.buf[end-1] == '}' {
		end--
	}
	out := s.buf[:end]
	s.buf = s.buf[end:]
	return out
}

// Flush ends the block. The held tail is the wrapper's closing brace and
// whitespace and is dropped. A block that stopped before the opener was
// complete is not a wrapper this code knows; its bytes are sent as they came
// rather than lost.
//
// A call whose input is empty (Anthropic sends no fragment, or an empty one,
// for `{}`) would leave the client with no content at all; Chat answers `{}`
// for it, and so does the stream.
func (s *formatStream) Flush() string {
	out := ""
	if !s.prefix {
		out = s.buf
	}
	s.buf = ""
	if out == "" && !s.sent {
		out = "{}"
	}
	s.sent = s.sent || out != ""
	return out
}

// wrapperPrefixLen returns the length of an opener `{"value":` (whitespace
// allowed between tokens) at the start of s. ok is false while s is still
// shorter than a full opener, or is not one; in the latter case the opener
// can never complete, which Flush then reports by returning the bytes.
func wrapperPrefixLen(s string) (int, bool) {
	i := skipSpace(s, 0)
	for _, tok := range []string{`{`, `"value"`, `:`} {
		if !strings.HasPrefix(s[i:], tok) {
			return 0, false
		}
		i = skipSpace(s, i+len(tok))
	}
	// The value may start right after the colon, so a partial opener is only
	// complete once something follows it.
	if i >= len(s) {
		return 0, false
	}
	return i, true
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
	}
	return i
}
