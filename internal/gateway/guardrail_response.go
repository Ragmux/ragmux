package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strconv"

	"github.com/ragmux/ragmux/internal/guardrail"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/provider"
)

// The post_response stage (ADR-034): what the model answered, before the
// client sees it. A JSON answer is scanned whole. A stream goes through a
// sliding window (guardrail.Window), so a match split across deltas is found
// before any part of it is written.
//
// The text of a model's thinking is scanned too, but never rewritten: its
// signature covers the exact text, and a client that sends the block back would
// be refused. A redact rule that matches in thinking therefore counts as a flag
// there (G5).

// guardHeader is the response header that says a JSON answer was emptied.
const guardHeader = "x-ragmux-guardrail"

// postGuard is the post_response state of one answer.
type postGuard struct {
	g *Gateway
	c *call
	// body scans what the client reads; think scans thinking, where a redact
	// rule can only flag.
	body, think *guardrail.Scan
	window      int
	// rewrites is set when a rule of the stage blocks or redacts; only then
	// are the token log-probabilities of an answer withheld.
	rewrites bool
}

// postGuard returns the post_response guard of the call, or nil when the
// project has no rule for the stage: nothing then changes about the answer,
// byte for byte (ADR-028 holds).
func (g *Gateway) postGuard(c *call) *postGuard {
	if c.guard == nil || !c.guard.set.HasStage(guardrail.StagePostResponse) {
		return nil
	}
	return &postGuard{g: g, c: c,
		body:   c.guard.set.Scan(guardrail.StagePostResponse, false),
		think:  c.guard.set.Scan(guardrail.StagePostResponse, true),
		window: c.guard.set.Window(), rewrites: c.guard.set.Rewrites(guardrail.StagePostResponse)}
}

// finish adds what the guard found to the request's totals and the metrics. It
// is called once, when the answer is settled.
func (p *postGuard) finish() {
	if p == nil {
		return
	}
	gs := p.c.guard
	blocked := p.body.Blocked() || p.think.Blocked()
	for _, sc := range []*guardrail.Scan{p.body, p.think} {
		gs.absorb(sc)
		p.g.Metrics.RecordGuardrailEvents(p.c.projectID, obs.GuardrailStagePostResponse, obs.GuardrailRedact, sc.Redactions())
		p.g.Metrics.RecordGuardrailEvents(p.c.projectID, obs.GuardrailStagePostResponse, obs.GuardrailFlag, sc.Flags())
	}
	if blocked {
		// The last stage to refuse something is the one the row names: an answer
		// emptied after a skipped retrieval is a post_response block.
		gs.blockedStage = guardrail.StagePostResponse
		p.g.Metrics.RecordGuardrailEvents(p.c.projectID, obs.GuardrailStagePostResponse, obs.GuardrailBlock, 1)
	}
}

// isBlocked reports whether either scan met a block rule.
func (p *postGuard) isBlocked() bool { return p.body.Blocked() || p.think.Blocked() }

// refusalFinish is the finish reason of an answer the guardrails emptied.
const refusalFinish = "content_filter"

// chatResponse applies the stage to a canonical answer. A match of a block rule
// empties every choice: no content, no tool calls, finish_reason
// content_filter, and the guard header. It reports whether it did.
func (p *postGuard) chatResponse(resp *provider.ChatResponse) bool {
	for i := range resp.Choices {
		m := &resp.Choices[i].Message
		if m.Content != nil {
			s := p.body.Apply(*m.Content)
			m.Content = &s
		}
		if m.Refusal != nil {
			s := p.body.Apply(*m.Refusal)
			m.Refusal = &s
		}
		if len(m.ToolCalls) > 0 {
			if out, changed := mapToolCalls(m.ToolCalls, p.body.Apply, p.body.Apply, p.body.ApplyName, p.body.Probe); changed {
				m.ToolCalls = out
			}
		}
		if len(m.Annotations) > 0 {
			if out, changed := mapBlockStrings(m.Annotations, p.body.Apply); changed {
				m.Annotations = out
			}
		}
		// The token log-probabilities spell the answer out one token at a time,
		// and a token cannot be redacted: the answer carries none.
		if p.rewrites {
			resp.Choices[i].Logprobs = nil
		}
		// reasoning_content and the thinking block derived from it are one text.
		if m.ReasoningContent != nil && (len(m.Thinking) != 1 || m.Thinking[0].Thinking != *m.ReasoningContent) {
			p.think.Apply(*m.ReasoningContent)
		}
		for _, tb := range m.Thinking {
			p.think.Apply(tb.Thinking)
		}
	}
	if !p.isBlocked() {
		return false
	}
	reason := refusalFinish
	for i := range resp.Choices {
		ch := &resp.Choices[i]
		empty := ""
		ch.Message.Content = &empty
		ch.Message.ToolCalls, ch.Message.Refusal = nil, nil
		ch.Message.ReasoningContent, ch.Message.Thinking, ch.Message.Annotations = nil, nil, nil
		ch.FinishReason = &reason
		ch.Logprobs = nil
	}
	p.c.w.Header().Set(guardHeader, "blocked")
	return true
}

// messagesResponse applies the stage to a native Messages answer. An answer no
// rule touched is returned as it came. A blocked one keeps its envelope and its
// usage and loses its content: content [] and stop_reason refusal.
func (p *postGuard) messagesResponse(raw []byte) ([]byte, bool) {
	body, err := provider.ParseMessagesBody(raw)
	if err != nil {
		return raw, false
	}
	content, ok := body.Get("content")
	if !ok {
		return raw, false
	}
	out, changed := mapBlocks(content, func(block map[string]json.RawMessage) bool {
		return p.messagesBlock(block)
	})
	if p.isBlocked() {
		body.Set("content", json.RawMessage("[]"))
		body.Set("stop_reason", json.RawMessage(`"refusal"`))
		p.c.w.Header().Set(guardHeader, "blocked")
		return body.Bytes(), true
	}
	if !changed {
		return raw, false
	}
	body.Set("content", out)
	return body.Bytes(), false
}

// messagesBlock scans one block of a native answer in place and reports whether
// it changed. Thinking is only flagged; a tool call's input is read as data;
// every other block -- text with its citations, a search result a server tool
// returned, a type the gateway has not met -- is read for all its content
// strings, so an answer cannot carry a match in a field the code did not name.
func (p *postGuard) messagesBlock(block map[string]json.RawMessage) bool {
	var typ string
	_ = json.Unmarshal(block["type"], &typ)
	switch {
	case typ == "thinking":
		var s string
		_ = json.Unmarshal(block["thinking"], &s)
		p.think.Apply(s)
		return false
	case unreadBlocks[typ]:
		return false
	case toolInputBlocks[typ]:
		named := mapStringField(block, "name", p.body.ApplyName)
		in, ok := block["input"]
		if !ok {
			return named
		}
		res, ch := mapToolInput(in, p.body.Apply, p.body.Apply, p.body.Probe)
		if ch {
			block["input"] = res
		}
		return ch || named
	}
	return mapBlockContent(block, p.body.Apply)
}

// ---------------------------------------------------------------- chat stream

// streamPiece is a window kept for one kind of text in one choice. A choice
// streams one kind at a time -- reasoning, then thinking blocks, then text,
// then tool calls one after the other -- so one window is open per choice; when
// the kind changes the old window is closed and what it held back is written
// first, which keeps the order the client reads and the Messages encoder (which
// does not reopen a block it has closed) needs.
type streamPiece struct {
	key string
	win *guardrail.Window
}

// streamGuard filters the chunks of a canonical stream.
type streamGuard struct {
	p       *postGuard
	pieces  map[int]*streamPiece
	last    provider.StreamChunk
	blocked bool
	// folded is set once the upstream is seen to send reasoning_content together
	// with the same text as a thinking block (provider.foldReasoningDelta): the
	// two are one text, scanned once and written in both places.
	folded bool
}

func (p *postGuard) stream() *streamGuard {
	return &streamGuard{p: p, pieces: map[int]*streamPiece{}}
}

// open returns the window for key in choice ci, closing and flushing the one of
// another kind first. thinking selects the scan that cannot rewrite.
func (sg *streamGuard) open(ci int, key string, thinking bool, flush func(key, rest string)) *guardrail.Window {
	if cur := sg.pieces[ci]; cur != nil {
		if cur.key == key {
			return cur.win
		}
		sg.closePiece(ci, flush)
	}
	scan := sg.p.body
	if thinking {
		scan = sg.p.think
	}
	pc := &streamPiece{key: key, win: scan.Window(sg.p.window)}
	sg.pieces[ci] = pc
	return pc.win
}

// push adds text to a window. A block rule that matches stops the stream and
// nothing of the window is returned.
func (sg *streamGuard) push(w *guardrail.Window, text string) string {
	out := w.Push(text)
	if w.Blocked() {
		sg.blocked = true
		return ""
	}
	return out
}

// closePiece ends choice ci's window and hands what it held back to flush.
func (sg *streamGuard) closePiece(ci int, flush func(key, rest string)) {
	cur := sg.pieces[ci]
	if cur == nil {
		return
	}
	delete(sg.pieces, ci)
	rest := cur.win.Close()
	if cur.win.Blocked() {
		sg.blocked = true
		return
	}
	if rest != "" && flush != nil {
		flush(cur.key, rest)
	}
}

// piece keys: a letter for the kind, then the index of a thinking block or tool
// call.
func pieceKey(kind byte, idx int) string { return string(kind) + strconv.Itoa(idx) }

// flushChunk is a chunk of its own carrying text a window held back, for the
// choice and kind in key.
func flushChunk(like provider.StreamChunk, ci int, key, rest string, folded bool) provider.StreamChunk {
	out := provider.StreamChunk{ID: like.ID, Object: like.Object, Created: like.Created, Model: like.Model,
		SystemFingerprint: like.SystemFingerprint}
	d := provider.Delta{}
	idx, _ := strconv.Atoi(key[1:])
	switch key[0] {
	case 'c':
		d.Content = &rest
	case 'u':
		d.Refusal = &rest
	case 'r':
		d.ReasoningContent = &rest
		if folded {
			zero := 0
			d.Thinking = []provider.ThinkingBlock{{Index: &zero, Type: "thinking", Thinking: rest}}
		}
	case 't':
		d.Thinking = []provider.ThinkingBlock{{Index: &idx, Type: "thinking", Thinking: rest}}
	case 'f':
		raw, _ := marshalPlain([]map[string]any{{"index": idx, "function": map[string]any{"arguments": rest}}})
		d.ToolCalls = raw
	}
	out.Choices = []provider.StreamChoice{{Index: ci, Delta: d}}
	return out
}

// refusalChunk ends every open choice with finish_reason content_filter.
func (sg *streamGuard) refusalChunk(like provider.StreamChunk, indexes []int) provider.StreamChunk {
	out := provider.StreamChunk{ID: like.ID, Object: like.Object, Created: like.Created, Model: like.Model,
		SystemFingerprint: like.SystemFingerprint}
	reason := refusalFinish
	sort.Ints(indexes)
	for _, ci := range indexes {
		out.Choices = append(out.Choices, provider.StreamChoice{Index: ci, Delta: provider.Delta{}, FinishReason: &reason})
	}
	return out
}

// choiceIndexes lists the choices a chunk and the open windows know of.
func (sg *streamGuard) choiceIndexes(chunk provider.StreamChunk) []int {
	seen := map[int]bool{}
	for _, ch := range chunk.Choices {
		seen[ch.Index] = true
	}
	for ci := range sg.pieces {
		seen[ci] = true
	}
	out := make([]int, 0, len(seen))
	for ci := range seen {
		out = append(out, ci)
	}
	return out
}

// process filters one chunk. It returns the chunks to write in its place, in
// order. When a block rule matched, nothing of the chunk is written: the
// result is the single chunk that ends the stream with content_filter, and
// blocked is true (the caller then stops the upstream and drops the rest).
func (sg *streamGuard) process(chunk provider.StreamChunk) (out []provider.StreamChunk, blocked bool) {
	if len(chunk.Choices) == 0 {
		return []provider.StreamChunk{chunk}, false // a usage trailer
	}
	sg.last = chunk
	var before, after []provider.StreamChunk
	choices := append([]provider.StreamChoice(nil), chunk.Choices...)
	for k := range choices {
		ch := &choices[k]
		ci := ch.Index
		flush := func(key, rest string) { before = append(before, flushChunk(chunk, ci, key, rest, sg.folded)) }
		d := ch.Delta

		folded := foldedDelta(d)
		if folded {
			// One text in two fields: it goes through the window once and comes
			// out in both.
			sg.folded = true
			w := sg.open(ci, pieceKey('r', 0), true, flush)
			s := sg.push(w, *d.ReasoningContent)
			d.ReasoningContent = textOrNil(s)
			d.Thinking = nil
			if s != "" {
				zero := 0
				d.Thinking = []provider.ThinkingBlock{{Index: &zero, Type: "thinking", Thinking: s}}
			}
		} else if d.ReasoningContent != nil {
			w := sg.open(ci, pieceKey('r', 0), true, flush)
			s := sg.push(w, *d.ReasoningContent)
			d.ReasoningContent = textOrNil(s)
		}
		if len(d.Thinking) > 0 && !folded {
			blocks := make([]provider.ThinkingBlock, 0, len(d.Thinking))
			for _, tb := range d.Thinking {
				idx := 0
				if tb.Index != nil {
					idx = *tb.Index
				}
				if tb.Type == "thinking" {
					w := sg.open(ci, pieceKey('t', idx), true, flush)
					tb.Thinking = sg.push(w, tb.Thinking)
					if tb.Signature != "" {
						// The signature closes the block: the text goes out first.
						tb.Thinking += sg.pieceRest(ci)
					}
				}
				if tb.Thinking == "" && tb.Signature == "" && tb.Data == "" {
					continue
				}
				blocks = append(blocks, tb)
			}
			d.Thinking = blocks
		}
		if d.Content != nil {
			w := sg.open(ci, pieceKey('c', 0), false, flush)
			d.Content = textOrNil(sg.push(w, *d.Content))
		}
		if d.Refusal != nil {
			w := sg.open(ci, pieceKey('u', 0), false, flush)
			d.Refusal = textOrNil(sg.push(w, *d.Refusal))
		}
		if len(d.ToolCalls) > 0 {
			d.ToolCalls = sg.toolCalls(ci, d.ToolCalls, flush)
		}
		if len(d.Annotations) > 0 {
			// Citations arrive whole, not in pieces: no window.
			d.Annotations, _ = mapBlockStrings(d.Annotations, sg.p.body.Apply)
			if sg.p.body.Blocked() {
				sg.blocked = true
			}
		}
		// Token log-probabilities spell the answer out, and a token cannot be
		// redacted: a guarded answer carries none.
		if sg.p.rewrites {
			ch.Logprobs = nil
		}
		if sg.blocked {
			break
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			// Everything the window held belongs before the finish reason: the
			// chunk is written without it, then the rest, then a chunk that
			// carries it.
			before2 := len(before)
			sg.closePiece(ci, flush)
			if sg.blocked {
				break
			}
			if len(before) > before2 {
				fin := *ch
				fin.Delta = provider.Delta{}
				ch.FinishReason = nil
				tail := provider.StreamChunk{ID: chunk.ID, Object: chunk.Object, Created: chunk.Created, Model: chunk.Model,
					SystemFingerprint: chunk.SystemFingerprint, Choices: []provider.StreamChoice{fin}}
				after = append(after, before[before2:]...)
				before = before[:before2]
				after = append(after, tail)
			}
		}
		ch.Delta = d
	}
	if sg.blocked {
		return []provider.StreamChunk{sg.refusalChunk(chunk, sg.choiceIndexes(chunk))}, true
	}
	chunk.Choices = choices
	out = append(out, before...)
	if !emptyChunk(chunk) {
		out = append(out, chunk)
	}
	return append(out, after...), false
}

// foldedDelta reports whether a delta carries one reasoning text twice: as
// reasoning_content and as the thinking block the provider derived from it.
func foldedDelta(d provider.Delta) bool {
	return d.ReasoningContent != nil && len(d.Thinking) == 1 && d.Thinking[0].Type == "thinking" &&
		d.Thinking[0].Signature == "" && d.Thinking[0].Thinking == *d.ReasoningContent
}

// pieceRest closes the open window of choice ci (the same kind that just
// received text) and returns what it held.
func (sg *streamGuard) pieceRest(ci int) string {
	cur := sg.pieces[ci]
	if cur == nil {
		return ""
	}
	delete(sg.pieces, ci)
	rest := cur.win.Close()
	if cur.win.Blocked() {
		sg.blocked = true
		return ""
	}
	return rest
}

// textOrNil is a string pointer for text, nil for none: an empty delta says
// nothing and some clients read it as an end.
func textOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// emptyChunk reports whether a filtered chunk has nothing left to say: no
// delta fields, no finish reason, no usage.
func emptyChunk(c provider.StreamChunk) bool {
	if c.Usage != nil {
		return false
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		if ch.FinishReason != nil || d.Role != "" || d.Content != nil || d.Refusal != nil || len(d.ToolCalls) > 0 ||
			d.ReasoningContent != nil || len(d.Thinking) > 0 || len(d.Annotations) > 0 || len(ch.Logprobs) > 0 {
			return false
		}
	}
	return true
}

// toolCalls filters the arguments of a tool_calls delta: each call has its own
// window, and a call that starts closes the one before it.
func (sg *streamGuard) toolCalls(ci int, raw json.RawMessage, flush func(key, rest string)) json.RawMessage {
	var calls []map[string]json.RawMessage
	if json.Unmarshal(raw, &calls) != nil {
		return raw
	}
	kept := calls[:0]
	for _, call := range calls {
		idx := 0
		_ = json.Unmarshal(call["index"], &idx)
		var fn map[string]json.RawMessage
		_ = json.Unmarshal(call["function"], &fn)
		var args string
		if fn != nil {
			_ = json.Unmarshal(fn["arguments"], &args)
		}
		w := sg.open(ci, pieceKey('f', idx), false, flush)
		out := sg.push(w, args)
		var name string
		if fn != nil {
			_ = json.Unmarshal(fn["name"], &name)
		}
		// The name of a call arrives whole, in the call's first delta.
		renamed := false
		if name != "" {
			if nn := sg.p.body.ApplyName(name); nn != name {
				name, renamed = nn, true
				fn["name"], _ = marshalPlain(nn)
			}
			if sg.p.body.Blocked() {
				sg.blocked = true
			}
		}
		if fn != nil {
			if _, had := fn["arguments"]; had {
				fn["arguments"], _ = marshalPlain(out)
				renamed = true
			}
			if renamed {
				call["function"], _ = marshalPlain(fn)
			}
		}
		_, hasID := call["id"]
		if out == "" && !hasID && name == "" {
			continue // nothing left in this entry
		}
		kept = append(kept, call)
	}
	if len(kept) == 0 {
		return nil
	}
	b, err := marshalPlain(kept)
	if err != nil {
		return raw
	}
	return b
}

// end flushes what the windows still hold when the stream is over. It returns
// the chunks to write, or the refusal chunk when the held text blocked.
func (sg *streamGuard) end() (out []provider.StreamChunk, blocked bool) {
	if sg.blocked {
		return nil, true
	}
	indexes := make([]int, 0, len(sg.pieces))
	for ci := range sg.pieces {
		indexes = append(indexes, ci)
	}
	sort.Ints(indexes)
	for _, ci := range indexes {
		sg.closePiece(ci, func(key, rest string) { out = append(out, flushChunk(sg.last, ci, key, rest, sg.folded)) })
	}
	if sg.blocked {
		return []provider.StreamChunk{sg.refusalChunk(sg.last, indexes)}, true
	}
	return out, false
}

// ----------------------------------------------------- native Messages stream

// nativeStream is the io.Writer a native Messages stream is relayed to when the
// project has post_response rules. The relay writes one whole event at a time;
// this reads it, runs the text of the deltas through the windows, and writes
// the event again (ADR-034: the one case where the native stream is not a copy
// of the upstream's bytes). Events it has nothing to do with pass unchanged.
type nativeStream struct {
	w io.Writer
	p *postGuard
	// kinds and wins are keyed by the index of the content block.
	kinds map[int]string
	wins  map[int]*guardrail.Window
	open  map[int]bool
	// stop, when set, tells the upstream to stop (the stream was refused).
	stop    func()
	blocked bool
	// err is the first write failure.
	err error
}

func (p *postGuard) nativeStream(w io.Writer, stop func()) *nativeStream {
	return &nativeStream{w: w, p: p, kinds: map[int]string{}, wins: map[int]*guardrail.Window{}, open: map[int]bool{}, stop: stop}
}

func (n *nativeStream) emit(b []byte) {
	if n.err != nil {
		return
	}
	if _, err := n.w.Write(b); err != nil {
		n.err = err
	}
}

// sseEvent renders one event.
func sseEvent(name string, data map[string]json.RawMessage) []byte {
	body, _ := marshalPlain(data)
	var b bytes.Buffer
	if name != "" {
		b.WriteString("event: " + name + "\n")
	}
	b.WriteString("data: ")
	b.Write(body)
	b.WriteString("\n\n")
	return b.Bytes()
}

// parseEvent reads the event name and data of one SSE event.
func parseEvent(ev []byte) (name string, data []byte) {
	for _, line := range bytes.Split(ev, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			name = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			v := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, v...)
		}
	}
	return name, data
}

// nameOr is the event name, or the event's type when the stream sent none.
func nameOr(name, typ string) string {
	if name != "" {
		return name
	}
	return typ
}

// Write receives one event from the relay.
func (n *nativeStream) Write(ev []byte) (int, error) {
	if n.err != nil {
		return 0, n.err
	}
	if n.blocked {
		return len(ev), nil
	}
	name, data := parseEvent(ev)
	var top map[string]json.RawMessage
	if data == nil || json.Unmarshal(data, &top) != nil {
		n.emit(ev)
		return len(ev), n.err
	}
	var typ string
	_ = json.Unmarshal(top["type"], &typ)
	idx := 0
	_ = json.Unmarshal(top["index"], &idx)
	switch typ {
	case "content_block_start":
		var cb map[string]json.RawMessage
		_ = json.Unmarshal(top["content_block"], &cb)
		var kind string
		_ = json.Unmarshal(cb["type"], &kind)
		// A block can arrive whole in its start event (a search result a server
		// tool returned): its strings are read here, like the deltas' are.
		changed := cb != nil && n.p.messagesBlock(cb)
		if n.p.body.Blocked() || n.p.think.Blocked() {
			n.refuse()
			return len(ev), n.err
		}
		n.kinds[idx], n.open[idx] = kind, true
		if changed {
			if b, err := marshalPlain(cb); err == nil {
				top["content_block"] = b
				n.emit(sseEvent(nameOr(name, typ), top))
				return len(ev), n.err
			}
		}
		n.emit(ev)
	case "content_block_delta":
		n.delta(ev, name, top, idx)
	case "content_block_stop":
		n.flushBlock(idx)
		if !n.blocked {
			delete(n.open, idx)
			n.emit(ev)
		}
	case "message_start":
		// The message may arrive with content already in it.
		var msg map[string]json.RawMessage
		if json.Unmarshal(top["message"], &msg) != nil || len(msg["content"]) <= 2 {
			n.emit(ev)
			break
		}
		out, changed := mapBlocks(msg["content"], n.p.messagesBlock)
		blocked := n.p.body.Blocked() || n.p.think.Blocked()
		switch {
		case blocked:
			msg["content"] = json.RawMessage("[]")
		case changed:
			msg["content"] = out
		default:
			n.emit(ev)
			return len(ev), n.err
		}
		if b, err := marshalPlain(msg); err == nil {
			top["message"] = b
			n.emit(sseEvent(nameOr(name, typ), top))
		}
		if blocked {
			n.refuse()
		}
	case "message_delta", "message_stop":
		for _, i := range n.openWindows() {
			n.flushBlock(i)
		}
		if !n.blocked {
			n.emit(ev)
		}
	default:
		n.emit(ev)
	}
	return len(ev), n.err
}

// openWindows lists the blocks that still hold text, in index order.
func (n *nativeStream) openWindows() []int {
	idx := make([]int, 0, len(n.wins))
	for i := range n.wins {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	return idx
}

// delta runs a content_block_delta through its block's window.
func (n *nativeStream) delta(ev []byte, name string, top map[string]json.RawMessage, idx int) {
	var d map[string]json.RawMessage
	if json.Unmarshal(top["delta"], &d) != nil {
		n.emit(ev)
		return
	}
	var dt string
	_ = json.Unmarshal(d["type"], &dt)
	field := ""
	switch dt {
	case "text_delta":
		field = "text"
	case "thinking_delta":
		field = "thinking"
	case "input_json_delta":
		field = "partial_json"
	case "citations_delta":
		// A citation arrives whole: its strings are read, not windowed.
		out, changed := mapBlockStrings(d["citation"], n.p.body.Apply)
		if n.p.body.Blocked() {
			n.refuse()
			return
		}
		if !changed {
			n.emit(ev)
			return
		}
		d["citation"] = out
		if b, err := marshalPlain(d); err == nil {
			top["delta"] = b
			n.emit(sseEvent(nameOr(name, "content_block_delta"), top))
			return
		}
		n.emit(ev)
		return
	case "signature_delta":
		// The signature ends the thinking text; what the window holds goes first.
		n.flushBlock(idx)
		if !n.blocked {
			n.emit(ev)
		}
		return
	default:
		// A delta type the gateway has not met: its strings are read, like a
		// block of an unknown type is.
		out, changed := mapBlockStrings(top["delta"], n.p.body.Apply)
		if n.p.body.Blocked() {
			n.refuse()
			return
		}
		if !changed {
			n.emit(ev)
			return
		}
		top["delta"] = out
		n.emit(sseEvent(nameOr(name, "content_block_delta"), top))
		return
	}
	var text string
	if json.Unmarshal(d[field], &text) != nil {
		n.emit(ev)
		return
	}
	w := n.wins[idx]
	if w == nil {
		scan := n.p.body
		if dt == "thinking_delta" {
			scan = n.p.think
		}
		w = scan.Window(n.p.window)
		n.wins[idx] = w
	}
	out := w.Push(text)
	if w.Blocked() {
		n.refuse()
		return
	}
	if out == "" {
		return
	}
	n.emitDelta(name, idx, dt, field, out)
}

// emitDelta writes a content_block_delta of the given kind carrying text.
func (n *nativeStream) emitDelta(name string, idx int, dt, field, text string) {
	d := map[string]json.RawMessage{}
	d["type"], _ = marshalPlain(dt)
	d[field], _ = marshalPlain(text)
	top := map[string]json.RawMessage{}
	top["type"], _ = marshalPlain("content_block_delta")
	top["index"], _ = marshalPlain(idx)
	top["delta"], _ = marshalPlain(d)
	if name == "" {
		name = "content_block_delta"
	}
	n.emit(sseEvent(name, top))
}

// flushBlock closes the window of block idx and writes what it held.
func (n *nativeStream) flushBlock(idx int) {
	w := n.wins[idx]
	if w == nil {
		return
	}
	delete(n.wins, idx)
	rest := w.Close()
	if w.Blocked() {
		n.refuse()
		return
	}
	if rest == "" {
		return
	}
	dt, field := "text_delta", "text"
	switch n.kinds[idx] {
	case "thinking":
		dt, field = "thinking_delta", "thinking"
	case "tool_use", "server_tool_use", "mcp_tool_use":
		dt, field = "input_json_delta", "partial_json"
	}
	n.emitDelta("content_block_delta", idx, dt, field, rest)
}

// refuse ends the answer: the blocks still open are closed, the message ends
// with stop_reason refusal, and everything the upstream sends after is dropped.
func (n *nativeStream) refuse() {
	if n.blocked {
		return
	}
	n.blocked = true
	open := make([]int, 0, len(n.open))
	for i := range n.open {
		open = append(open, i)
	}
	sort.Ints(open)
	for _, i := range open {
		top := map[string]json.RawMessage{}
		top["type"], _ = marshalPlain("content_block_stop")
		top["index"], _ = marshalPlain(i)
		n.emit(sseEvent("content_block_stop", top))
	}
	md := map[string]json.RawMessage{}
	md["type"], _ = marshalPlain("message_delta")
	md["delta"] = json.RawMessage(`{"stop_reason":"refusal","stop_sequence":null}`)
	md["usage"] = json.RawMessage(`{"output_tokens":0}`)
	n.emit(sseEvent("message_delta", md))
	ms := map[string]json.RawMessage{}
	ms["type"], _ = marshalPlain("message_stop")
	n.emit(sseEvent("message_stop", ms))
	if n.stop != nil {
		n.stop()
	}
}

// end writes what the windows still hold when the stream ends without having
// said so itself. It does nothing after a refusal.
func (n *nativeStream) end() {
	if n.blocked {
		return
	}
	for _, i := range n.openWindows() {
		n.flushBlock(i)
	}
}
