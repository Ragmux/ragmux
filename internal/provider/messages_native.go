package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// The native half of /v1/messages (ADR-028): a Messages request that lands on
// an Anthropic connection is sent as the client wrote it. The gateway changes
// only what ADR-028 lists -- model, system, ragmux, the tools a project denies,
// replayed thinking blocks under the RAG x prefix rule, tool_choice -- and the
// answer, JSON or SSE, goes back as the upstream's own bytes.

// MessagesInput is a client's Messages request and what the gateway adds to it.
type MessagesInput struct {
	// Body is the client's request. Tool governance has been applied to it;
	// PrepareMessages leaves it unchanged.
	Body *MessagesBody
	// SystemAppend are text blocks for the end of the system prompt: the
	// project's system prompt, and the retrieved context under the "system"
	// placement. They go last so the client's own blocks -- Claude Code's
	// attribution block among them -- keep their place (ADR-028).
	SystemAppend []string
	// BeforeLastUser is a text block for the last user message (the
	// "before_last_user" placement), put after the message's tool_result
	// blocks, which the API requires to lead.
	BeforeLastUser string
	// RAGActive says retrieval may have changed the prompt of this
	// conversation (see ChatRequest.RAGActive).
	RAGActive bool
	// Version and Beta are the client's anthropic-version and anthropic-beta
	// header values, sent on as they are. An empty Version is the default.
	Version, Beta string
	// Inline is the "ragmux" object the client asked to get back, or nil.
	Inline []byte
	// EchoModel is the model string the client sent. The answer carries it in
	// place of the model the upstream reports (ADR-018); empty leaves the
	// upstream's value.
	EchoModel string
}

// MessagesPrepared is a request ready to send.
type MessagesPrepared struct {
	Body          []byte
	Stream        bool
	Version, Beta string
	Inline        []byte
	EchoModel     string
	// Warnings are the Warning texts of the changes made beyond the project's
	// own.
	Warnings []string
}

// MessagesReply is a non-streaming answer: the upstream's body, plus what the
// gateway reads from it for the request log.
type MessagesReply struct {
	Body []byte
	MessagesSummary
}

// MessagesSummary is what the gateway reads from a Messages answer without
// changing it.
type MessagesSummary struct {
	// Usage is the upstream's token counts, nil when it reported none.
	Usage *Usage
	// Model is the model the upstream reported.
	Model string
	// Finish is the stop reason as an OpenAI finish_reason, nil when none.
	Finish *string
	// ToolNames are the tools the model called, in call order.
	ToolNames []string
	// TextChars is the length of the answer's text, for the usage estimate
	// when the upstream reports no counts.
	TextChars int
	// FailedType is the type of an error event the upstream sent in the
	// stream, empty when there was none; FailedMessage is its message.
	FailedType, FailedMessage string
}

// ErrClientWrite marks a relay that stopped because the client could not be
// written to.
var ErrClientWrite = errors.New("write to client failed")

const defaultMessagesVersion = anthropicVersion

// PrepareMessages implements MessagesPassthrough.
func (p *anthropic) PrepareMessages(in MessagesInput) (MessagesPrepared, error) {
	caps := CapabilitiesFor("anthropic", p.cfg.Model)
	b := in.Body.Clone()
	prep := MessagesPrepared{Version: in.Version, Beta: in.Beta, Inline: in.Inline, EchoModel: in.EchoModel}
	if prep.Version == "" {
		prep.Version = defaultMessagesVersion
	}
	if err := b.SetValue("model", p.cfg.Model); err != nil {
		return prep, err
	}
	b.Delete("ragmux")
	if v, ok := b.Get("stream"); ok {
		_ = json.Unmarshal(v, &prep.Stream)
	}

	turns, turnsOK := parseTurns(b)

	// RAG x prefix (ADR-031): a model that checks a replayed thinking block
	// against everything before it cannot take one back once retrieval
	// changed that prefix, so the earlier turns' blocks are not sent.
	turnsChanged := false
	if turnsOK && in.RAGActive && caps.ThinkingPrefixBound {
		var dropped bool
		turns, dropped = dropThinkingTurns(turns)
		if dropped {
			turnsChanged = true
			prep.Warnings = append(prep.Warnings, warnThinkingPrefixDropped)
		}
	}

	prep.Warnings = append(prep.Warnings, p.applyMessagesThinkingRules(b, caps, turns, turnsOK)...)

	if len(in.SystemAppend) > 0 {
		if err := appendSystem(b, in.SystemAppend); err != nil {
			return prep, err
		}
	}
	if turnsOK && in.BeforeLastUser != "" {
		turns, turnsChanged = leadLastUserTurn(turns, in.BeforeLastUser), true
	}
	if turnsChanged {
		setTurns(b, turns)
	}
	prep.Body = b.Bytes()
	return prep, nil
}

// applyMessagesThinkingRules is the native counterpart of the rules
// setupAnthropicThinking applies to a canonical request, run on the client's
// own thinking, tool_choice and top_p: a forced tool_choice on a model that
// cannot take one becomes auto (ADR-031, S6), and where the API would answer
// 400 for a combination the request did not ask to fail, the part that gives
// way is thinking or top_p, with a Warning.
func (p *anthropic) applyMessagesThinkingRules(b *MessagesBody, caps Capabilities, turns []turn, turnsOK bool) []string {
	var warns []string
	tools := rawArrayLen(b, "tools")
	choice := toolChoiceType(b)
	forced := tools > 0 && (choice == "any" || choice == "tool")
	if forced && !caps.ForcedToolChoice {
		auto := `{"type":"auto"}`
		var obj map[string]json.RawMessage
		if raw, _ := b.Get("tool_choice"); json.Unmarshal(raw, &obj) == nil {
			if d, ok := obj["disable_parallel_tool_use"]; ok {
				auto = `{"type":"auto","disable_parallel_tool_use":` + string(d) + `}`
			}
		}
		b.Set("tool_choice", json.RawMessage(auto))
		warns = append(warns, warnForcedToolAuto)
		forced = false
	}
	thinking := thinkingType(b)
	if thinking == "enabled" && caps.Thinking == ThinkingBudget {
		switch {
		case forced:
			b.Delete("thinking")
			warns = append(warns, warnThinkingForcedTool)
		case turnsOK && endsInToolRoundWithoutThinking(turns):
			b.Delete("thinking")
			warns = append(warns, warnThinkingToolRound)
		}
	}
	if t := thinkingType(b); t == "enabled" || t == "adaptive" {
		if raw, ok := b.Get("top_p"); ok {
			var v float64
			if json.Unmarshal(raw, &v) == nil && v < anthropicMinThinkingTopP {
				b.Delete("top_p")
				warns = append(warns, warnTopPThinking)
			}
		}
	}
	return warns
}

func rawArrayLen(b *MessagesBody, key string) int {
	raw, ok := b.Get(key)
	if !ok {
		return 0
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return 0
	}
	return len(arr)
}

func toolChoiceType(b *MessagesBody) string {
	raw, ok := b.Get("tool_choice")
	if !ok {
		return ""
	}
	var o struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Type
}

func thinkingType(b *MessagesBody) string {
	raw, ok := b.Get("thinking")
	if !ok {
		return ""
	}
	var o struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Type
}

// turn is one message of a Messages request, with its content split into
// blocks when it is an array.
type turn struct {
	msg  *MessagesBody
	role string
	// blocks and types are the content blocks, raw and by type; nil when the
	// content is a plain string.
	blocks []json.RawMessage
	types  []string
	// text is a plain string content.
	text     string
	isString bool
}

func (t turn) has(blockType string) bool {
	for _, ty := range t.types {
		if ty == blockType {
			return true
		}
	}
	return false
}

func (t turn) hasThinking() bool { return t.has(thinkingBlockThinking) || t.has(thinkingBlockRedacted) }

// parseTurns reads the messages array. ok is false when it is not one the
// gateway can read; the body is then sent as it is and the API judges it.
func parseTurns(b *MessagesBody) ([]turn, bool) {
	raw, present := b.Get("messages")
	if !present {
		return nil, false
	}
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil, false
	}
	out := make([]turn, 0, len(msgs))
	for _, m := range msgs {
		body, err := ParseMessagesBody(m)
		if err != nil {
			return nil, false
		}
		t := turn{msg: body}
		if r, ok := body.Get("role"); ok {
			_ = json.Unmarshal(r, &t.role)
		}
		content, _ := body.Get("content")
		if err := json.Unmarshal(content, &t.text); err == nil {
			t.isString = true
		} else {
			if json.Unmarshal(content, &t.blocks) != nil {
				return nil, false
			}
			t.types = make([]string, len(t.blocks))
			for i, blk := range t.blocks {
				var o struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(blk, &o)
				t.types[i] = o.Type
			}
		}
		out = append(out, t)
	}
	return out, true
}

// setTurns writes the (possibly changed) turns back as the messages field.
func setTurns(b *MessagesBody, turns []turn) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, t := range turns {
		if i > 0 {
			buf.WriteByte(',')
		}
		if !t.isString {
			t.msg.Set("content", joinRaw(t.blocks))
		}
		buf.Write(t.msg.Bytes())
	}
	buf.WriteByte(']')
	b.Set("messages", buf.Bytes())
}

// joinRaw renders raw JSON values as an array, bytes untouched.
func joinRaw(items []json.RawMessage) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, it := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(it)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

// dropThinkingTurns removes the thinking and redacted_thinking blocks of every
// assistant turn, and the turns that held nothing else.
func dropThinkingTurns(turns []turn) ([]turn, bool) {
	var out []turn
	dropped := false
	for _, t := range turns {
		if t.role == "assistant" && !t.isString && t.hasThinking() {
			var blocks []json.RawMessage
			var types []string
			for i, ty := range t.types {
				if ty == thinkingBlockThinking || ty == thinkingBlockRedacted {
					continue
				}
				blocks, types = append(blocks, t.blocks[i]), append(types, ty)
			}
			dropped = true
			if len(blocks) == 0 {
				continue
			}
			t.blocks, t.types = blocks, types
		}
		out = append(out, t)
	}
	if out == nil {
		out = []turn{}
	}
	return out, dropped
}

// endsInToolRoundWithoutThinking reports a request whose last turn returns
// tool results to an assistant turn that carries no thinking block. A model
// with manual (budget) thinking refuses such a round while thinking is on.
func endsInToolRoundWithoutThinking(turns []turn) bool {
	if len(turns) == 0 {
		return false
	}
	last := turns[len(turns)-1]
	if last.role != "user" || !last.has("tool_result") {
		return false
	}
	for i := len(turns) - 2; i >= 0; i-- {
		if turns[i].role == "assistant" {
			return !turns[i].hasThinking()
		}
	}
	return false
}

// leadLastUserTurn puts a text block into the last user turn: after its
// tool_result blocks, which the API requires to lead, and in front of the
// rest. A string content becomes the block followed by the original text.
func leadLastUserTurn(turns []turn, text string) []turn {
	last := -1
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].role == "user" {
			last = i
			break
		}
	}
	if last < 0 {
		return turns
	}
	block, _ := marshalNoEscape(map[string]string{"type": "text", "text": text})
	t := turns[last]
	if t.isString {
		orig, _ := marshalNoEscape(map[string]string{"type": "text", "text": t.text})
		t.isString = false
		t.blocks = []json.RawMessage{block, orig}
		t.types = []string{"text", "text"}
	} else {
		at := 0
		for i, ty := range t.types {
			if ty == "tool_result" {
				at = i + 1
			}
		}
		blocks := make([]json.RawMessage, 0, len(t.blocks)+1)
		types := make([]string, 0, len(t.blocks)+1)
		blocks, types = append(blocks, t.blocks[:at]...), append(types, t.types[:at]...)
		blocks, types = append(blocks, block), append(types, "text")
		blocks, types = append(blocks, t.blocks[at:]...), append(types, t.types[at:]...)
		t.blocks, t.types = blocks, types
	}
	out := append([]turn(nil), turns...)
	out[last] = t
	return out
}

// appendSystem puts texts at the end of the request's system prompt. A string
// system prompt becomes the array of blocks, its text as the first block, so
// the client's own text is not edited.
func appendSystem(b *MessagesBody, texts []string) error {
	raw, ok := b.Get("system")
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		b.Set("system", mustMarshal(strings.Join(texts, "\n\n")))
		return nil
	}
	var blocks []json.RawMessage
	var s string
	switch {
	case json.Unmarshal(raw, &s) == nil:
		first, err := marshalNoEscape(map[string]string{"type": "text", "text": s})
		if err != nil {
			return err
		}
		blocks = []json.RawMessage{first}
	case json.Unmarshal(raw, &blocks) == nil:
	default:
		// Not a shape the API takes; it will say so.
		return nil
	}
	for _, t := range texts {
		blk, err := marshalNoEscape(map[string]string{"type": "text", "text": t})
		if err != nil {
			return err
		}
		blocks = append(blocks, blk)
	}
	b.Set("system", joinRaw(blocks))
	return nil
}

func mustMarshal(v any) json.RawMessage {
	b, _ := marshalNoEscape(v)
	return b
}

// messagesHeaders are the request headers of a native call: the connection's
// key, and the client's anthropic-version and anthropic-beta as they came.
func (p *anthropic) messagesHeaders(prep MessagesPrepared) map[string]string {
	return map[string]string{
		"x-api-key":         p.cfg.APIKey,
		"anthropic-version": prep.Version,
		"anthropic-beta":    prep.Beta,
	}
}

// keepUpstreamBody keeps a 4xx answer's body on e, with the connection's key
// masked if the upstream echoed it. Only the literal key is replaced: the
// pattern-based redaction of messages would rewrite text that is not a secret,
// and this body is relayed unchanged.
func keepUpstreamBody(cfg Config, e *Error, resp *http.Response, raw []byte) {
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		return
	}
	if k := cfg.APIKey; k != "" {
		raw = bytes.ReplaceAll(raw, []byte(k), []byte(Redacted))
	}
	e.UpstreamBody = raw
}

// MessagesCall implements MessagesPassthrough.
func (p *anthropic) MessagesCall(ctx context.Context, prep MessagesPrepared) (*MessagesReply, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.timeout())
	defer cancel()
	resp, span, err := doRequest(ctx, p.cfg, opChat, p.base+"/v1/messages", p.messagesHeaders(prep), rawBody(prep.Body))
	if err != nil {
		return nil, err
	}
	defer span.End()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		spanFailed(span, "read_failed")
		return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error",
			Message: "read upstream response: " + RedactWith(err.Error(), p.cfg.APIKey)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := upstreamStatusError(p.cfg, resp, raw)
		keepUpstreamBody(p.cfg, e, resp, raw)
		spanFailed(span, e.Type)
		return nil, e
	}
	span.SetStatusOK()
	var ar struct {
		Model      string              `json:"model"`
		StopReason string              `json:"stop_reason"`
		Usage      AnthropicUsageBlock `json:"usage"`
		Content    []AnthropicContent  `json:"content"`
	}
	if err := json.Unmarshal(raw, &ar); err != nil {
		spanFailed(span, "decode_failed")
		return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "decode upstream response: " + err.Error()}
	}
	reply := &MessagesReply{Body: raw}
	reply.Model = ar.Model
	reply.Usage = ar.Usage.usage()
	reply.Finish = anthropicFinish(p.cfg.logger(), ar.StopReason)
	for _, c := range ar.Content {
		switch c.Type {
		case "text":
			reply.TextChars += len(c.Text)
		case "tool_use":
			reply.ToolNames = append(reply.ToolNames, c.Name)
		}
	}
	if out, err := rewriteMessage(raw, prep.EchoModel, prep.Inline); err == nil {
		reply.Body = out
	}
	return reply, nil
}

// rewriteMessage puts the client's model and the ragmux object into a message
// object, in place when the field is there, at the end otherwise. Every other
// byte of the message is the upstream's. An empty echo and a nil inline leave
// raw as it is.
func rewriteMessage(raw []byte, echo string, inline []byte) ([]byte, error) {
	if echo == "" && len(inline) == 0 {
		return raw, nil
	}
	b, err := ParseMessagesBody(raw)
	if err != nil {
		return nil, err
	}
	if echo != "" {
		if err := b.SetValue("model", echo); err != nil {
			return nil, err
		}
	}
	if len(inline) > 0 {
		b.Set("ragmux", inline)
	}
	return b.Bytes(), nil
}

// MessagesStream is an open streaming answer of a native call.
type MessagesStream struct {
	body   io.ReadCloser
	cfg    Config
	inline []byte
	echo   string
}

// MessagesStream implements MessagesPassthrough.
func (p *anthropic) MessagesStream(ctx context.Context, prep MessagesPrepared) (*MessagesStream, error) {
	resp, err := doStreamKeep(ctx, p.cfg, p.base+"/v1/messages", p.messagesHeaders(prep), rawBody(prep.Body), true)
	if err != nil {
		return nil, err
	}
	return &MessagesStream{body: resp.Body, cfg: p.cfg, inline: prep.Inline, echo: prep.EchoModel}, nil
}

// Close releases the upstream connection.
func (s *MessagesStream) Close() error { return s.body.Close() }

// Relay copies the upstream's events to dst, each exactly as it came and
// flushed as it completes, so ping events and every field reach the client
// untouched. The one exception is the "ragmux" object the client asked for,
// which goes into the message of the message_start event.
//
// begin is called once, before the first write: that is the moment the
// gateway commits its status line and headers. An error returned before it
// was called (the upstream failed before sending an event) leaves the client
// untouched. event, if not nil, is called for every event relayed. A write
// failure is returned as ErrClientWrite.
func (s *MessagesStream) Relay(dst io.Writer, flush func(), begin func(), event func()) (MessagesSummary, error) {
	var sum MessagesSummary
	var usage AnthropicUsageBlock
	sawUsage := false
	started := false
	write := func(b []byte) error {
		if !started {
			started = true
			begin()
		}
		if _, err := dst.Write(b); err != nil {
			return errors.Join(ErrClientWrite, err)
		}
		flush()
		if event != nil {
			event()
		}
		return nil
	}

	sc := bufio.NewScanner(s.body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	sc.Split(scanLinesKeepEnding)
	var ev bytes.Buffer
	var name string
	var data []byte
	haveData := false
	emit := func() error {
		out := ev.Bytes()
		payload := data
		var base struct {
			Type string `json:"type"`
		}
		if haveData && json.Unmarshal(payload, &base) == nil {
			if rewritten, ok := s.observe(base.Type, payload, name, &sum, &usage, &sawUsage); ok {
				out = rewritten
			}
		}
		err := write(out)
		ev.Reset()
		name, data, haveData = "", nil, false
		return err
	}
	for sc.Scan() {
		line := sc.Bytes()
		ev.Write(line)
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 {
			if ev.Len() == len(line) {
				// A blank line with no event before it.
				ev.Reset()
				continue
			}
			if err := emit(); err != nil {
				return sum, err
			}
			continue
		}
		if trimmed[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(trimmed, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			name = string(value)
		case "data":
			if haveData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			haveData = true
		}
	}
	if err := sc.Err(); err != nil {
		var pe *Error
		if errors.As(err, &pe) {
			return sum, err
		}
		return sum, &Error{Status: http.StatusBadGateway, Type: "upstream_error",
			Message: "read upstream stream: " + RedactWith(err.Error(), s.cfg.APIKey)}
	}
	if ev.Len() > 0 {
		// A final event the upstream did not end with a blank line.
		if err := emit(); err != nil {
			return sum, err
		}
	}
	if !started {
		return sum, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "upstream stream carried no events"}
	}
	if sawUsage {
		sum.Usage = usage.usage()
	}
	return sum, nil
}

// observe reads one event's data for the summary. For a message_start event
// of a request that asked for the ragmux object, or whose model is echoed, it
// also returns the event rewritten with the object and the client's model in
// its message.
func (s *MessagesStream) observe(typ string, payload []byte, name string, sum *MessagesSummary, usage *AnthropicUsageBlock, sawUsage *bool) ([]byte, bool) {
	switch typ {
	case "message_start":
		var ms struct {
			Message struct {
				Model string              `json:"model"`
				Usage AnthropicUsageBlock `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(payload, &ms) == nil {
			sum.Model = ms.Message.Model
			usage.merge(ms.Message.Usage)
			*sawUsage = true
		}
		if len(s.inline) == 0 && s.echo == "" {
			return nil, false
		}
		top, err := ParseMessagesBody(payload)
		if err != nil {
			return nil, false
		}
		msg, ok := top.Get("message")
		if !ok {
			return nil, false
		}
		withMsg, err := rewriteMessage(msg, s.echo, s.inline)
		if err != nil {
			return nil, false
		}
		top.Set("message", withMsg)
		var out bytes.Buffer
		if name != "" {
			out.WriteString("event: " + name + "\n")
		}
		out.WriteString("data: ")
		out.Write(top.Bytes())
		out.WriteString("\n\n")
		return out.Bytes(), true
	case "content_block_start":
		var cb struct {
			ContentBlock struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if json.Unmarshal(payload, &cb) == nil && cb.ContentBlock.Type == "tool_use" {
			sum.ToolNames = append(sum.ToolNames, cb.ContentBlock.Name)
		}
	case "content_block_delta":
		var d struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &d) == nil && d.Delta.Type == "text_delta" {
			sum.TextChars += len(d.Delta.Text)
		}
	case "message_delta":
		var md struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage AnthropicUsageBlock `json:"usage"`
		}
		if json.Unmarshal(payload, &md) == nil {
			usage.merge(md.Usage)
			*sawUsage = true
			if md.Delta.StopReason != "" {
				sum.Finish = anthropicFinish(s.cfg.logger(), md.Delta.StopReason)
			}
		}
	case "error":
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &e) == nil {
			sum.FailedType = e.Error.Type
			sum.FailedMessage = RedactWith(e.Error.Message, s.cfg.APIKey)
		}
	}
	return nil, false
}

// scanLinesKeepEnding is bufio.ScanLines that keeps the line ending, so the
// relay can write back the bytes it read.
func scanLinesKeepEnding(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i+1], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
