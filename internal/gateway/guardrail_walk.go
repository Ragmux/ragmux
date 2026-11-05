package gateway

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/ragmux/ragmux/internal/provider"
)

// What a request offers the guardrails to read (ADR-034, G4): the text parts of
// every message, tool call arguments and tool results; in the Messages shape
// the system prompt and every content block. Not images, not tool definitions,
// not the project's own prompt or the retrieved context (neither is in the
// request yet when pre_request runs), and not the thinking blocks of earlier
// turns, whose signature a change would break.
//
// The Messages shape has block types the gateway does not know by name (a
// document, a search result, whatever Anthropic adds next), and a client picks
// the type. So a block is read unless it is known to hold no text of a person's
// (a thinking block, an image): every string in it is scanned except the ones
// that are structure, not content (opaqueKeys). A new block type is then read
// by default, and a rule cannot be walked around by changing a block's type.

// textWalker calls visit once for every text of a request, in a fixed order,
// with the role the text has (system, user, assistant, tool) and puts back what
// visit returns. The order must not depend on anything but the request: the
// webhook stage walks the same request three times and pairs the texts by their
// position.
type textWalker func(visit func(role, text string) string)

// walkRole maps a chat role onto the four the webhook body names.
func walkRole(role string) string {
	switch role {
	case "system", "developer":
		return "system"
	case "assistant":
		return "assistant"
	case "tool", "function":
		return "tool"
	}
	return "user"
}

// chatWalker walks a canonical chat request in place.
func chatWalker(req *provider.ChatRequest) textWalker {
	return func(visit func(role, text string) string) {
		for i := range req.Messages {
			m := &req.Messages[i]
			role := walkRole(m.Role)
			if out, changed := mapChatContent(m.Content, func(s string) string { return visit(role, s) }); changed {
				m.Content = out
			}
			// reasoning_content is the client's own text (an assistant turn of a
			// compatible upstream) and carries no signature: it is read. The
			// signed ragmux_thinking is not.
			if len(m.ReasoningContent) > 0 {
				if out, changed := mapChatContent(m.ReasoningContent, func(s string) string { return visit("assistant", s) }); changed {
					m.ReasoningContent = out
				}
			}
			// A participant name is limited to letters, digits, _ and -, which
			// is room enough for a number.
			if m.Name != "" {
				m.Name = visit(nameRolePrefix+role, m.Name)
			}
			if len(m.ToolCalls) > 0 {
				if out, changed := mapToolCalls(m.ToolCalls, func(s string) string { return visit("assistant", s) },
					func(s string) string { return visit(numRolePrefix+"assistant", s) },
					func(s string) string { return visit(nameRolePrefix+"assistant", s) },
					func(s string) { visit(rawRolePrefix+"assistant", s) }); changed {
					m.ToolCalls = out
				}
			}
		}
		// A predicted output is text the model is asked to reproduce.
		if raw, ok := req.Extra["prediction"]; ok {
			if out, changed := mapPrediction(raw, func(s string) string { return visit("assistant", s) }); changed {
				req.Extra["prediction"] = out
			}
		}
	}
}

// rawRolePrefix marks a visit that is a second reading of a text already read
// piece by piece (the JSON of tool call arguments): the rules look at it for a
// block and nothing else, and the webhook is not sent it.
const rawRolePrefix = "raw:"

// numRolePrefix marks the visit of a number inside tool call arguments. The
// project's own rules read it like any other text (a number can hold a match);
// the webhook is not sent it: its texts are keys and strings.
const numRolePrefix = "num:"

// nameRolePrefix marks the visit of a name (a tool's, a participant's). The
// rules read it like any text, but a redaction of it is a placeholder that is a
// legal name (guardrail.Scan.ApplyName). The webhook is sent it as the role
// that follows the prefix.
const nameRolePrefix = "name:"

// skipsWebhook reports whether a visit is one the webhook is not sent.
func skipsWebhook(role string) bool {
	return strings.HasPrefix(role, rawRolePrefix) || strings.HasPrefix(role, numRolePrefix)
}

// webhookRole is the role a visit is sent to the webhook with.
func webhookRole(role string) string { return strings.TrimPrefix(role, nameRolePrefix) }

// mapPrediction applies fn to the content of a chat "prediction" object.
func mapPrediction(raw json.RawMessage, fn func(string) string) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return raw, false
	}
	out, changed := mapChatContent(obj["content"], fn)
	if !changed {
		return raw, false
	}
	obj["content"] = out
	b, err := marshalPlain(obj)
	return b, err == nil
}

// messagesWalker walks a Messages request body in place.
func messagesWalker(body *provider.MessagesBody) textWalker {
	return func(visit func(role, text string) string) {
		if raw, ok := body.Get("system"); ok {
			if out, changed := mapMessagesContent(raw, "system", visit); changed {
				body.Set("system", out)
			}
		}
		raw, ok := body.Get("messages")
		if !ok {
			return
		}
		var msgs []json.RawMessage
		if json.Unmarshal(raw, &msgs) != nil {
			return
		}
		edited := false
		for i, mraw := range msgs {
			var m map[string]json.RawMessage
			if json.Unmarshal(mraw, &m) != nil {
				continue
			}
			var role string
			_ = json.Unmarshal(m["role"], &role)
			out, changed := mapMessagesContent(m["content"], walkRole(role), visit)
			if !changed {
				continue
			}
			m["content"] = out
			if b, err := marshalPlain(m); err == nil {
				msgs[i], edited = b, true
			}
		}
		if edited {
			if b, err := marshalPlain(msgs); err == nil {
				body.Set("messages", b)
			}
		}
	}
}

// marshalPlain is json.Marshal without the HTML escaping of <, > and &, which
// would change bytes of a request the guardrails only meant to edit in one
// place.
func marshalPlain(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// firstByte is the first byte of raw that is not JSON whitespace, 0 for none.
func firstByte(raw []byte) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
		default:
			return b
		}
	}
	return 0
}

// unreadChatParts are the content parts that carry an image, audio or a file:
// binary data and the address of an image, which G4 leaves unread.
var unreadChatParts = map[string]bool{"image_url": true, "input_audio": true, "file": true, "image": true}

// mapChatContent applies fn to the text of a chat message content: the string,
// or the parts of the array.
func mapChatContent(raw json.RawMessage, fn func(string) string) (json.RawMessage, bool) {
	if firstByte(raw) == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return raw, false
		}
		if out := fn(s); out != s {
			b, err := marshalPlain(out)
			return b, err == nil
		}
		return raw, false
	}
	// Every part but the ones that carry an image, audio or a file is read,
	// whatever its type is called (text, refusal, input_text, and the next
	// one): a client picks the type. So is a part that is a bare string and a
	// content that is one part and not a list.
	return mapParts(raw, func(block map[string]json.RawMessage) bool {
		var typ string
		_ = json.Unmarshal(block["type"], &typ)
		if unreadChatParts[typ] {
			return false
		}
		return mapBlockContent(block, fn)
	}, fn)
}

// mapParts applies obj to a content that is an object, or to each object of a
// list, and str to each string of a list, and puts back what changed. Anything
// else in a list (a number, a null) passes.
func mapParts(raw json.RawMessage, obj func(map[string]json.RawMessage) bool, str func(string) string) (json.RawMessage, bool) {
	switch firstByte(raw) {
	case '{':
		var o map[string]json.RawMessage
		if json.Unmarshal(raw, &o) != nil || o == nil || !obj(o) {
			return raw, false
		}
		b, err := marshalPlain(o)
		return b, err == nil
	case '[':
		var elems []json.RawMessage
		if json.Unmarshal(raw, &elems) != nil {
			return raw, false
		}
		changed := false
		for i, el := range elems {
			switch firstByte(el) {
			case '"':
				var s string
				if json.Unmarshal(el, &s) != nil {
					continue
				}
				if out := str(s); out != s {
					if b, err := marshalPlain(out); err == nil {
						elems[i], changed = b, true
					}
				}
			case '{':
				var o map[string]json.RawMessage
				if json.Unmarshal(el, &o) != nil || o == nil || !obj(o) {
					continue
				}
				if b, err := marshalPlain(o); err == nil {
					elems[i], changed = b, true
				}
			}
		}
		if !changed {
			return raw, false
		}
		b, err := marshalPlain(elems)
		return b, err == nil
	}
	return raw, false
}

// mapStringField applies fn to a string field of an object and reports whether
// it changed.
func mapStringField(obj map[string]json.RawMessage, key string, fn func(string) string) bool {
	raw, ok := obj[key]
	if !ok || firstByte(raw) != '"' {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return false
	}
	out := fn(s)
	if out == s {
		return false
	}
	b, err := marshalPlain(out)
	if err != nil {
		return false
	}
	obj[key] = b
	return true
}

// mapBlocks decodes a JSON array of objects, lets edit change each one in
// place, and re-encodes the array when any did. Elements that are not objects
// pass untouched.
func mapBlocks(raw json.RawMessage, edit func(map[string]json.RawMessage) bool) (json.RawMessage, bool) {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return raw, false
	}
	changed := false
	for i, b := range blocks {
		var obj map[string]json.RawMessage
		if json.Unmarshal(b, &obj) != nil || obj == nil {
			continue
		}
		if !edit(obj) {
			continue
		}
		if enc, err := marshalPlain(obj); err == nil {
			blocks[i], changed = enc, true
		}
	}
	if !changed {
		return raw, false
	}
	out, err := marshalPlain(blocks)
	return out, err == nil
}

// mapJSONText applies fn to the values of a text that is JSON (tool call
// arguments are): every string and every number in it, so that a number cannot
// hold a match the strings are searched for and what is written back is JSON
// still. A text that is not JSON is read as it is.
//
// The JSON is also looked at as a text, by probe, for a block: a pattern that
// spans a key and its value ("password": "...") is a pattern of the text, and a
// streamed answer is read as a text. probe may be nil.
func mapJSONText(s string, fn, num func(string) string, probe func(string)) string {
	if json.Valid([]byte(s)) {
		if probe != nil {
			probe(s)
		}
		if out, changed := mapJSONStrings(json.RawMessage(s), fn, num); changed {
			return string(out)
		}
		return s
	}
	return fn(s)
}

// mapToolCalls applies fn to the name and the arguments of every call of an
// OpenAI tool_calls array: name is the visitor of names, probe is
// mapJSONText's.
func mapToolCalls(raw json.RawMessage, fn, num, name func(string) string, probe func(string)) (json.RawMessage, bool) {
	return mapBlocks(raw, func(call map[string]json.RawMessage) bool {
		var fnObj map[string]json.RawMessage
		if json.Unmarshal(call["function"], &fnObj) != nil || fnObj == nil {
			return false
		}
		named := mapStringField(fnObj, "name", name)
		var args bool
		switch firstByte(fnObj["arguments"]) {
		case '{', '[':
			// Arguments are a string in the API; a server that takes them as
			// JSON is read as JSON.
			out, changed := mapToolInput(fnObj["arguments"], fn, num, probe)
			if changed {
				fnObj["arguments"], args = out, true
			}
		default:
			args = mapStringField(fnObj, "arguments", func(a string) string { return mapJSONText(a, fn, num, probe) })
		}
		if !named && !args {
			return false
		}
		b, err := marshalPlain(fnObj)
		if err != nil {
			return false
		}
		call["function"] = b
		return true
	})
}

// toolInputBlocks are the Messages blocks whose "input" is arguments the model
// produced for a tool.
var toolInputBlocks = map[string]bool{"tool_use": true, "server_tool_use": true, "mcp_tool_use": true}

// unreadBlocks are the Messages blocks that hold nothing a rule should read:
// the model's signed thinking and images.
var unreadBlocks = map[string]bool{"thinking": true, "redacted_thinking": true, "image": true}

// opaqueKeys are the fields of a block that are a kind, an id, a signature, an
// encrypted payload or a setting: a change to them breaks the block, and none
// is text. Everything else in a block is content, and that includes a name, a
// title and a URL: an address can carry an identity ("?id=10000000146") to a
// third party as well as any sentence can.
var opaqueKeys = map[string]bool{
	"type": true, "id": true, "tool_use_id": true, "media_type": true,
	"signature": true, "encrypted_content": true, "encrypted_index": true, "cache_control": true,
	"file_id": true, "container_id": true,
}

// mapMessagesContent applies visit to the texts of one Messages content (a
// message's, the system prompt's, a tool result's). role is the message's role;
// a tool result is always "tool" and the input of a tool call "assistant".
func mapMessagesContent(raw json.RawMessage, role string, visit func(role, text string) string) (json.RawMessage, bool) {
	if firstByte(raw) == '"' {
		return mapChatContent(raw, func(s string) string { return visit(role, s) })
	}
	return mapParts(raw, func(block map[string]json.RawMessage) bool {
		return mapMessagesBlock(block, role, visit)
	}, func(s string) string { return visit(role, s) })
}

// mapMessagesBlock applies visit to the texts of one content block, in place.
func mapMessagesBlock(block map[string]json.RawMessage, role string, visit func(role, text string) string) bool {
	var typ string
	_ = json.Unmarshal(block["type"], &typ)
	switch {
	case unreadBlocks[typ]:
		return false
	case typ == "tool_result":
		c, ok := block["content"]
		if !ok {
			return false
		}
		out, changed := mapMessagesContent(c, "tool", visit)
		if changed {
			block["content"] = out
		}
		return changed
	case toolInputBlocks[typ]:
		named := mapStringField(block, "name", func(s string) string { return visit(nameRolePrefix+"assistant", s) })
		in, ok := block["input"]
		if !ok {
			return named
		}
		out, changed := mapToolInput(in, func(s string) string { return visit("assistant", s) },
			func(s string) string { return visit(numRolePrefix+"assistant", s) },
			func(s string) { visit(rawRolePrefix+"assistant", s) })
		if changed {
			block["input"] = out
		}
		return changed || named
	}
	// text, document, search_result, and whatever else: every content string.
	return mapBlockContent(block, func(s string) string { return visit(role, s) })
}

// mapBlockContent applies fn to the content strings of a block, in place, and
// reports whether any changed.
func mapBlockContent(block map[string]json.RawMessage, fn func(string) string) bool {
	whole, err := marshalPlain(block)
	if err != nil {
		return false
	}
	out, changed := mapBlockStrings(whole, fn)
	if !changed {
		return false
	}
	var edited map[string]json.RawMessage
	if json.Unmarshal(out, &edited) != nil {
		return false
	}
	for k := range block {
		delete(block, k)
	}
	for k, v := range edited {
		block[k] = v
	}
	return true
}

// mapToolInput is mapJSONStrings for the input of a tool call, with the JSON
// also probed as a text (see mapJSONText).
func mapToolInput(in json.RawMessage, fn, num func(string) string, probe func(string)) (json.RawMessage, bool) {
	if probe != nil {
		probe(string(in))
	}
	return mapJSONStrings(in, fn, num)
}

// mapJSONStrings applies fn to every string and number value inside a JSON
// document, map keys excluded, in a fixed order (object keys sorted), and
// re-encodes the document when any changed. A number that fn changes becomes a
// string, so a match in a number is replaced and the document stays JSON.
// Numbers that fn leaves keep their digits.
func mapJSONStrings(raw json.RawMessage, fn, num func(string) string) (json.RawMessage, bool) {
	return mapJSON(raw, fn, num, jsonValues)
}

// mapBlockStrings is mapJSONStrings for a Messages block: the strings of
// content, without the fields that are structure (opaqueKeys) or base64 data,
// and without numbers (a page number is not text, and turning it into a string
// would break the block).
func mapBlockStrings(raw json.RawMessage, fn func(string) string) (json.RawMessage, bool) {
	return mapJSON(raw, fn, nil, jsonContent)
}

type jsonMode int

const (
	jsonValues  jsonMode = iota // data: every string and number
	jsonContent                 // a block: the strings that are content
)

func mapJSON(raw json.RawMessage, fn, num func(string) string, mode jsonMode) (json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw, false
	}
	out, changed := walkStrings(v, fn, num, mode)
	// A key that appears twice is read by a parser as one of its values, and
	// which one is the parser's choice; the value decoded here is the last. The
	// document goes on with that one alone, so a value the rules never saw does
	// not go with it.
	if !changed && !hasDuplicateKeys(raw) {
		return raw, false
	}
	b, err := marshalPlain(out)
	if err != nil {
		return raw, false
	}
	return b, true
}

func walkStrings(v any, fn, num func(string) string, mode jsonMode) (any, bool) {
	switch t := v.(type) {
	case string:
		out := fn(t)
		return out, out != t
	case json.Number:
		if mode != jsonValues {
			return v, false
		}
		if out := num(t.String()); out != t.String() {
			return out, true
		}
	case []any:
		changed := false
		for i := range t {
			if nv, ch := walkStrings(t[i], fn, num, mode); ch {
				t[i], changed = nv, true
			}
		}
		return t, changed
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			if mode == jsonContent && (opaqueKeys[k] || (k == "data" && t["type"] == "base64")) {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		changed := false
		for _, k := range keys {
			if nv, ch := walkStrings(t[k], fn, num, mode); ch {
				t[k], changed = nv, true
			}
			// In data (a tool's arguments) a key is text too: {"10000000146": 1}.
			if mode == jsonValues {
				if nk := fn(k); nk != k {
					for _, taken := t[nk]; taken; _, taken = t[nk] {
						nk += "_"
					}
					t[nk] = t[k]
					delete(t, k)
					changed = true
				}
			}
		}
		return t, changed
	}
	return v, false
}

// hasDuplicateKeys reports whether any object in a JSON document has the same
// key twice.
func hasDuplicateKeys(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var stack []map[string]bool // one set per open object, nil for an array
	var wantKey []bool
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, map[string]bool{})
				wantKey = append(wantKey, true)
			case '[':
				stack = append(stack, nil)
				wantKey = append(wantKey, false)
			default:
				stack, wantKey = stack[:len(stack)-1], wantKey[:len(wantKey)-1]
				if n := len(wantKey); n > 0 && stack[n-1] != nil {
					wantKey[n-1] = true
				}
			}
			continue
		case string:
			if n := len(stack); n > 0 && stack[n-1] != nil && wantKey[n-1] {
				if stack[n-1][t] {
					return true
				}
				stack[n-1][t] = true
				wantKey[n-1] = false
				continue
			}
		}
		// A value (or a string in an array): in an object the next token is a key.
		if n := len(stack); n > 0 && stack[n-1] != nil {
			wantKey[n-1] = true
		}
	}
}
