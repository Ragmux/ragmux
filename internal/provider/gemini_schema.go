package provider

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Gemini's functionDeclarations take an OpenAPI 3.0 subset, not JSON Schema,
// and unknown keywords are rejected rather than ignored. Every OpenAI
// strict-mode tool carries "additionalProperties": false and most non-trivial
// ones carry $defs/$ref, so forwarding a schema unchanged fails essentially
// every real tool definition with a relayed "Invalid JSON payload received.
// Unknown name \"additionalProperties\"", which tells an SDK author nothing
// about what to change.
//
// So the schema is sanitised instead: keep what Gemini understands, rewrite
// what has an equivalent, drop the rest. It is lossy by design — a tool that
// relies on oneOf to discriminate behaves differently here than on OpenAI —
// and it never fails: the worst case is {"type":"object"}, which costs the
// model the argument shape but still lets it call the tool. Whatever Gemini
// still objects to, upstreamError relays verbatim.
//
// "Keep" means keep a value of the shape Gemini declares, not keep the bytes:
// a schema is client-supplied, so an {"enum":[1,2,3]} or a
// {"description":{"a":"b"}} arrives often enough, and forwarding either one
// verbatim breaks the promise this file exists to make. Every value is
// decoded into the type Gemini's Schema message names for that field, and
// anything that does not decode is dropped like an unknown keyword.

const (
	// geminiSchemaMaxDepth bounds $ref inlining. A schema nested deeper than
	// this is recursive far more often than it is genuinely deep.
	geminiSchemaMaxDepth = 8
	// geminiSchemaMaxNodes caps the whole walk. A hand-written tool schema
	// runs to tens of nodes; hundreds is already unusual. Past this the rest
	// of the tree is elided rather than expanded.
	geminiSchemaMaxNodes   = 4096
	geminiSchemaElided     = "(recursive schema elided)"
	geminiSchemaUnresolved = "(unresolved schema reference elided)"
	geminiSchemaTruncated  = "(schema too large; elided)"
	// geminiSchemaRoot is the JSON Pointer of the schema's own root, which
	// every definition's key is built from.
	geminiSchemaRoot = "#"
)

// geminiScalarKind is the type Gemini's Schema message declares a keyword to
// hold. A keyword whose value is of any other shape is dropped rather than
// forwarded.
type geminiScalarKind uint8

const (
	geminiScalarString geminiScalarKind = iota
	geminiScalarStringList
	geminiScalarNumber
	geminiScalarBool
)

// geminiSchemaScalars are the keywords that travel through, each with the
// type Gemini takes it as.
var geminiSchemaScalars = map[string]geminiScalarKind{
	"description": geminiScalarString,
	"pattern":     geminiScalarString,
	"enum":        geminiScalarStringList,
	"nullable":    geminiScalarBool,
	"minimum":     geminiScalarNumber,
	"maximum":     geminiScalarNumber,
	"minItems":    geminiScalarNumber,
	"maxItems":    geminiScalarNumber,
	"minLength":   geminiScalarNumber,
	"maxLength":   geminiScalarNumber,
}

// geminiSchemaFormats is the format whitelist per type; Gemini rejects any
// other format value outright.
var geminiSchemaFormats = map[string]map[string]bool{
	"string":  {"enum": true, "date-time": true},
	"number":  {"float": true, "double": true},
	"integer": {"int32": true, "int64": true},
}

// geminiScalar decodes one scalar keyword into the Go value that marshals
// back as the type Gemini expects, and reports whether the value had that
// shape at all. A JSON null is nobody's value: it decodes into every Go type
// as a no-op, so it is rejected up front rather than silently becoming "",
// false or 0.
func geminiScalar(kind geminiScalarKind, raw json.RawMessage) (any, bool) {
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, false
	}
	switch kind {
	case geminiScalarString:
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		return s, true
	case geminiScalarStringList:
		// Schema.enum is repeated string. A numeric or object enum has no
		// equivalent, and rewriting its members as their JSON spelling would
		// tell the model to send "1" where the tool wants 1.
		var list []string
		if json.Unmarshal(raw, &list) != nil || len(list) == 0 {
			return nil, false
		}
		return list, true
	case geminiScalarNumber:
		var n json.Number
		if json.Unmarshal(raw, &n) != nil || n == "" {
			return nil, false
		}
		// Schema.minimum and its neighbours are doubles. json.Number checks
		// the spelling and not the range, so 1e400 is a valid literal here
		// and an overflow there; ParseFloat is what says it fits.
		if _, err := strconv.ParseFloat(n.String(), 64); err != nil {
			return nil, false
		}
		return n, true
	case geminiScalarBool:
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return nil, false
		}
		return b, true
	}
	return nil, false
}

// geminiConst reads a const value. Schema.enum is repeated string, so only a
// string const survives as a one-value enum; any other JSON value can still
// say what type the node is, which is more than dropping it outright and
// avoids the {"enum":[7],"type":"string"} this used to invent. A null const
// says neither.
func geminiConst(raw json.RawMessage) (value, typ string, ok bool) {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "" || trimmed == "null":
		return "", "", false
	case trimmed == "true" || trimmed == "false":
		return "", "boolean", true
	case strings.HasPrefix(trimmed, `"`):
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", "", false
		}
		return s, "string", true
	case strings.HasPrefix(trimmed, "["):
		return "", "array", true
	case strings.HasPrefix(trimmed, "{"):
		return "", "object", true
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil || n == "" {
		return "", "", false
	}
	if strings.ContainsAny(n.String(), ".eE") {
		return "", "number", true
	}
	return "", "integer", true
}

// sanitizeGeminiSchema rewrites a JSON Schema into the subset Gemini accepts
// and reports the keywords it had to drop, for one debug line per request.
func sanitizeGeminiSchema(raw json.RawMessage) (json.RawMessage, []string) {
	w := &geminiSchemaWalk{defs: map[string]json.RawMessage{}, dropped: map[string]bool{},
		budget: geminiSchemaMaxNodes}
	node := w.node(raw, 0, nil, geminiSchemaRoot)
	if len(node) == 0 {
		node = map[string]any{"type": "object"}
	}
	out, err := json.Marshal(node)
	if err != nil {
		out = json.RawMessage(`{"type":"object"}`)
	}
	return out, w.droppedKeywords()
}

// geminiSchemaEmpty reports a sanitised schema with no properties left.
// Several Gemini model versions reject {"type":"object","properties":{}}, so
// such a declaration is sent without parameters at all.
func geminiSchemaEmpty(raw json.RawMessage) bool {
	var obj struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	return json.Unmarshal(raw, &obj) != nil || len(obj.Properties) == 0
}

type geminiSchemaWalk struct {
	// defs collects every $defs/definitions map seen on the way down, so a
	// $ref deeper in the tree still resolves against an ancestor's table.
	//
	// The key is the definition's full JSON Pointer ("#/$defs/Foo"), not its
	// name. Keying on the name alone let "#/$defs/Foo", "#/definitions/Foo"
	// and an external "https://…#/definitions/Foo" all land on the same
	// entry, so two same-named definitions at different depths overwrote each
	// other and Go's map iteration order decided which one a $ref got: the
	// same schema sanitised to different bytes from one run to the next.
	defs    map[string]json.RawMessage
	dropped map[string]bool
	// budget is what is left of geminiSchemaMaxNodes. The depth cap bounds
	// how deep the walk goes but says nothing about how wide it gets, and a
	// tool schema arrives from the client: a chain of differently named
	// definitions defeats the per-branch visited set, because each name is
	// new on its own branch, so every level multiplies by the number of
	// properties. A few kilobytes of input reached a gigabyte of output
	// before this counter existed.
	budget int
}

// node sanitises one schema node. visited holds the $ref pointers already
// inlined on this branch; it is copied per branch so siblings may each use
// the same definition. path is the node's own JSON Pointer, which the
// definitions it declares are filed under.
func (w *geminiSchemaWalk) node(raw json.RawMessage, depth int, visited map[string]bool, path string) map[string]any {
	if depth > geminiSchemaMaxDepth {
		return geminiElidedNode()
	}
	if w.budget <= 0 {
		w.drop("(truncated)")
		return geminiTruncatedNode()
	}
	w.budget--
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		// A boolean schema (draft "true"/"false") or anything else that is not
		// an object. Gemini needs a typed node, so give it the widest one.
		return map[string]any{"type": "object"}
	}
	w.collectDefs(obj, path)
	if ref, ok := obj["$ref"]; ok {
		return w.inlineRef(ref, depth, visited)
	}

	out := map[string]any{}
	typ, nullable := w.nodeType(obj)
	if typ != "" {
		out["type"] = typ
	}
	if nullable {
		out["nullable"] = true
	}
	// Keywords are walked in a fixed order. Map iteration order would
	// otherwise decide which subtree the node budget truncates and, through
	// collectDefs, which table a $ref deeper down resolves against.
	//
	// allOf and format are resolved after the loop regardless: the first has
	// to merge into properties the loop may not have written yet, the second
	// needs the node's type.
	for _, k := range geminiSortedKeys(obj) {
		v := obj[k]
		switch k {
		case "type", "$defs", "definitions", "allOf", "format":
		case "items":
			out["items"] = w.node(v, depth+1, visited, path+"/items")
		case "properties":
			var props map[string]json.RawMessage
			if json.Unmarshal(v, &props) != nil {
				w.drop(k)
				continue
			}
			cleaned := map[string]any{}
			for _, name := range geminiSortedKeys(props) {
				cleaned[name] = w.node(props[name], depth+1, visited,
					path+"/properties/"+geminiPointerEscape(name))
			}
			if len(cleaned) > 0 {
				out["properties"] = cleaned
			}
		case "required":
			var req []string
			if json.Unmarshal(v, &req) == nil && len(req) > 0 {
				out["required"] = req
			}
		case "anyOf", "oneOf":
			// oneOf's exclusivity has no equivalent; anyOf is the closest
			// Gemini offers and a model rarely tells the difference.
			if b := w.branches(v, depth, visited, path+"/"+k); len(b) > 0 {
				out["anyOf"] = b
			}
		case "const":
			value, constType, ok := geminiConst(v)
			switch {
			case !ok:
				w.drop(k)
			case constType == "string":
				out["enum"] = []string{value}
			case typ == "":
				// Not expressible as an enum, but it still pins the type.
				typ = constType
				out["type"] = constType
			default:
				w.drop(k)
			}
		case "exclusiveMinimum", "exclusiveMaximum":
			// Only the draft-06 numeric form carries a bound; the draft-04
			// boolean form only modifies minimum/maximum, which are kept.
			n, ok := geminiScalar(geminiScalarNumber, v)
			if !ok {
				w.drop(k)
				continue
			}
			if k == "exclusiveMinimum" {
				out["minimum"] = n
			} else {
				out["maximum"] = n
			}
		default:
			kind, scalar := geminiSchemaScalars[k]
			if !scalar {
				w.drop(k)
				continue
			}
			if k == "nullable" && nullable {
				continue
			}
			value, ok := geminiScalar(kind, v)
			if !ok {
				w.drop(k)
				continue
			}
			out[k] = value
		}
	}
	w.mergeAllOf(out, obj["allOf"], depth, visited, path+"/allOf")
	if rawFormat, ok := obj["format"]; ok {
		var f string
		if json.Unmarshal(rawFormat, &f) == nil && geminiSchemaFormats[typ][f] {
			out["format"] = f
		} else {
			w.drop("format")
		}
	}
	// Schema.enum is repeated string and Gemini reads it only on a string
	// node: an enum without a type gets one, and an enum contradicting the
	// type it does have is what goes, because that contradiction would be the
	// sanitiser's own invention rather than the client's.
	if _, ok := out["enum"]; ok {
		switch out["type"] {
		case nil:
			out["type"] = "string"
		case "string":
		default:
			delete(out, "enum")
			w.drop("enum")
		}
	}
	if len(out) == 0 {
		// Nothing survived. Gemini needs a typed node, and the widest one is
		// what the root degrades to as well.
		out["type"] = "object"
	}
	// Gemini reads an untyped node as an error, and a schema may legitimately
	// carry none -- {"description":"the city"} is valid JSON Schema. The one
	// exception is a pure union: an anyOf node is typed by its branches.
	if out["type"] == nil && out["anyOf"] == nil {
		out["type"] = geminiInferType(out)
	}
	return out
}

// geminiInferType picks a type for a node that declared none, from whatever
// keywords survived. It is a guess, but a typed guess is a schema Gemini
// reads and an untyped node is one it rejects.
func geminiInferType(out map[string]any) string {
	switch {
	case out["properties"] != nil || out["required"] != nil:
		return "object"
	case out["items"] != nil:
		return "array"
	case out["minimum"] != nil || out["maximum"] != nil:
		return "number"
	default:
		return "string"
	}
}

// nodeType resolves the type keyword. A ["string","null"] union is Gemini's
// nullable string; a wider union keeps its first real member, because Gemini
// takes exactly one type per node.
func (w *geminiSchemaWalk) nodeType(obj map[string]json.RawMessage) (string, bool) {
	raw, ok := obj["type"]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "null" {
			return "string", true
		}
		return s, false
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return "", false
	}
	typ, nullable := "", false
	for _, t := range list {
		switch {
		case t == "null":
			nullable = true
		case typ == "":
			typ = t
		}
	}
	return typ, nullable
}

func (w *geminiSchemaWalk) branches(raw json.RawMessage, depth int, visited map[string]bool, path string) []any {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	out := make([]any, 0, len(list))
	for i, b := range list {
		out = append(out, w.node(b, depth+1, visited, path+"/"+strconv.Itoa(i)))
	}
	return out
}

// mergeAllOf folds an allOf of object schemas into the node itself, which is
// what generators mean by it ("these fields and those"). Branches that are
// not objects fall back to anyOf.
func (w *geminiSchemaWalk) mergeAllOf(out map[string]any, raw json.RawMessage, depth int, visited map[string]bool, path string) {
	if len(raw) == 0 {
		return
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil || len(list) == 0 {
		w.drop("allOf")
		return
	}
	nodes := make([]map[string]any, 0, len(list))
	objects := true
	for i, b := range list {
		n := w.node(b, depth+1, visited, path+"/"+strconv.Itoa(i))
		if _, ok := n["properties"]; !ok {
			objects = false
		}
		nodes = append(nodes, n)
	}
	if !objects {
		branches := make([]any, len(nodes))
		for i, n := range nodes {
			branches[i] = n
		}
		out["anyOf"] = branches
		return
	}
	props, _ := out["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	// required is merged as a set. Appending let a name repeat once per
	// branch that mentioned it, and nested allOf chains multiplied that at
	// every level with nothing bounding the result.
	required, _ := out["required"].([]string)
	seen := make(map[string]bool, len(required))
	for _, name := range required {
		seen[name] = true
	}
	for _, n := range nodes {
		for k, v := range n["properties"].(map[string]any) {
			props[k] = v
		}
		req, ok := n["required"].([]string)
		if !ok {
			continue
		}
		for _, name := range req {
			if !seen[name] {
				seen[name] = true
				required = append(required, name)
			}
		}
	}
	out["type"] = "object"
	out["properties"] = props
	if len(required) > 0 {
		out["required"] = required
	}
}

// collectDefs files every definition under its own JSON Pointer, so the two
// spellings of the keyword and the same name at two depths stay distinct.
func (w *geminiSchemaWalk) collectDefs(obj map[string]json.RawMessage, path string) {
	for _, key := range []string{"$defs", "definitions"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var defs map[string]json.RawMessage
		if json.Unmarshal(raw, &defs) != nil {
			continue
		}
		for name, d := range defs {
			w.defs[path+"/"+key+"/"+geminiPointerEscape(name)] = d
		}
	}
}

func (w *geminiSchemaWalk) inlineRef(raw json.RawMessage, depth int, visited map[string]bool) map[string]any {
	var ref string
	if json.Unmarshal(raw, &ref) != nil {
		return geminiElidedNode()
	}
	// Definitions are keyed by pointer, so only a pointer into this document
	// matches. An external "https://example.com/s.json#/definitions/Foo" names
	// a document this process will not fetch, and answering it with a local
	// definition that happens to share a name would hand the model a shape
	// the tool never described.
	def, ok := w.defs[ref]
	if !ok {
		// A definition we never saw, which since references are matched on
		// their full pointer is most often an external one. Gemini has no
		// $ref, so there is nothing to defer to.
		return geminiUnresolvedNode()
	}
	if visited[ref] || depth >= geminiSchemaMaxDepth {
		// A cycle: the node degrades to a described string.
		return geminiElidedNode()
	}
	next := make(map[string]bool, len(visited)+1)
	for k := range visited {
		next[k] = true
	}
	next[ref] = true
	return w.node(def, depth+1, next, ref)
}

func (w *geminiSchemaWalk) drop(keyword string) { w.dropped[keyword] = true }

func (w *geminiSchemaWalk) droppedKeywords() []string {
	if len(w.dropped) == 0 {
		return nil
	}
	out := make([]string, 0, len(w.dropped))
	for k := range w.dropped {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func geminiSortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// geminiPointerEscaper spells one JSON Pointer token (RFC 6901), so a
// definition named "a/b" cannot be mistaken for one nested under "a".
var geminiPointerEscaper = strings.NewReplacer("~", "~0", "/", "~1")

func geminiPointerEscape(s string) string { return geminiPointerEscaper.Replace(s) }

func geminiElidedNode() map[string]any {
	return map[string]any{"type": "string", "description": geminiSchemaElided}
}

func geminiUnresolvedNode() map[string]any {
	return map[string]any{"type": "string", "description": geminiSchemaUnresolved}
}

// geminiTruncatedNode stands in for a subtree the node budget cut off. It is
// a valid schema, so the tool still reaches the model with the shape it did
// manage to describe.
func geminiTruncatedNode() map[string]any {
	return map[string]any{"type": "string", "description": geminiSchemaTruncated}
}
