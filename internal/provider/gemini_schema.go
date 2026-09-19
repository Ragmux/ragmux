package provider

import (
	"encoding/json"
	"sort"
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

const (
	// geminiSchemaMaxDepth bounds $ref inlining. A schema nested deeper than
	// this is recursive far more often than it is genuinely deep.
	geminiSchemaMaxDepth = 8
	// geminiSchemaMaxNodes caps the whole walk. A hand-written tool schema
	// runs to tens of nodes; hundreds is already unusual. Past this the rest
	// of the tree is elided rather than expanded.
	geminiSchemaMaxNodes  = 4096
	geminiSchemaElided    = "(recursive schema elided)"
	geminiSchemaTruncated = "(schema too large; elided)"
)

// geminiSchemaScalars travel through with their value unchanged.
var geminiSchemaScalars = map[string]bool{
	"description": true, "enum": true, "nullable": true, "minimum": true, "maximum": true,
	"minItems": true, "maxItems": true, "minLength": true, "maxLength": true, "pattern": true,
}

// geminiSchemaFormats is the format whitelist per type; Gemini rejects any
// other format value outright.
var geminiSchemaFormats = map[string]map[string]bool{
	"string":  {"enum": true, "date-time": true},
	"number":  {"float": true, "double": true},
	"integer": {"int32": true, "int64": true},
}

// sanitizeGeminiSchema rewrites a JSON Schema into the subset Gemini accepts
// and reports the keywords it had to drop, for one debug line per request.
func sanitizeGeminiSchema(raw json.RawMessage) (json.RawMessage, []string) {
	w := &geminiSchemaWalk{defs: map[string]json.RawMessage{}, dropped: map[string]bool{},
		budget: geminiSchemaMaxNodes}
	node := w.node(raw, 0, nil)
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

// node sanitises one schema node. visited holds the $ref names already
// inlined on this branch; it is copied per branch so siblings may each use
// the same definition.
func (w *geminiSchemaWalk) node(raw json.RawMessage, depth int, visited map[string]bool) map[string]any {
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
	w.collectDefs(obj)
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
	// allOf and format are resolved after the loop: the first has to merge
	// into properties the loop may not have written yet, the second needs the
	// node's type. Map iteration order would otherwise decide the outcome.
	for k, v := range obj {
		switch {
		case k == "type" || k == "$defs" || k == "definitions" || k == "allOf" || k == "format":
		case geminiSchemaScalars[k]:
			if k == "nullable" && nullable {
				continue
			}
			out[k] = v
		case k == "items":
			out["items"] = w.node(v, depth+1, visited)
		case k == "properties":
			var props map[string]json.RawMessage
			if json.Unmarshal(v, &props) != nil {
				w.drop(k)
				continue
			}
			cleaned := map[string]any{}
			for name, p := range props {
				cleaned[name] = w.node(p, depth+1, visited)
			}
			if len(cleaned) > 0 {
				out["properties"] = cleaned
			}
		case k == "required":
			var req []string
			if json.Unmarshal(v, &req) == nil && len(req) > 0 {
				out["required"] = req
			}
		case k == "anyOf" || k == "oneOf":
			// oneOf's exclusivity has no equivalent; anyOf is the closest
			// Gemini offers and a model rarely tells the difference.
			if b := w.branches(v, depth, visited); len(b) > 0 {
				out["anyOf"] = b
			}
		case k == "const":
			out["enum"] = []json.RawMessage{v}
		case k == "exclusiveMinimum", k == "exclusiveMaximum":
			// Only the draft-06 numeric form carries a bound; the draft-04
			// boolean form only modifies minimum/maximum, which are kept.
			var n json.Number
			if json.Unmarshal(v, &n) != nil {
				w.drop(k)
				continue
			}
			if k == "exclusiveMinimum" {
				out["minimum"] = v
			} else {
				out["maximum"] = v
			}
		default:
			w.drop(k)
		}
	}
	w.mergeAllOf(out, obj["allOf"], depth, visited)
	if raw, ok := obj["format"]; ok {
		var f string
		if json.Unmarshal(raw, &f) == nil && geminiSchemaFormats[typ][f] {
			out["format"] = f
		} else {
			w.drop("format")
		}
	}
	// An enum without a type is rejected; every enum Gemini takes is a string.
	if _, ok := out["enum"]; ok && out["type"] == nil {
		out["type"] = "string"
	}
	return out
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

func (w *geminiSchemaWalk) branches(raw json.RawMessage, depth int, visited map[string]bool) []any {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	out := make([]any, 0, len(list))
	for _, b := range list {
		out = append(out, w.node(b, depth+1, visited))
	}
	return out
}

// mergeAllOf folds an allOf of object schemas into the node itself, which is
// what generators mean by it ("these fields and those"). Branches that are
// not objects fall back to anyOf.
func (w *geminiSchemaWalk) mergeAllOf(out map[string]any, raw json.RawMessage, depth int, visited map[string]bool) {
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
	for _, b := range list {
		n := w.node(b, depth+1, visited)
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
	required, _ := out["required"].([]string)
	for _, n := range nodes {
		for k, v := range n["properties"].(map[string]any) {
			props[k] = v
		}
		if req, ok := n["required"].([]string); ok {
			required = append(required, req...)
		}
	}
	out["type"] = "object"
	out["properties"] = props
	if len(required) > 0 {
		out["required"] = required
	}
}

func (w *geminiSchemaWalk) collectDefs(obj map[string]json.RawMessage) {
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
			w.defs[name] = d
		}
	}
}

func (w *geminiSchemaWalk) inlineRef(raw json.RawMessage, depth int, visited map[string]bool) map[string]any {
	var ref string
	if json.Unmarshal(raw, &ref) != nil {
		return geminiElidedNode()
	}
	name := ref
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	def, ok := w.defs[name]
	if !ok || visited[name] || depth >= geminiSchemaMaxDepth {
		// A cycle, or a definition we never saw: Gemini has no $ref, so there
		// is nothing to defer to and the node degrades to a described string.
		return geminiElidedNode()
	}
	next := make(map[string]bool, len(visited)+1)
	for k := range visited {
		next[k] = true
	}
	next[name] = true
	return w.node(def, depth+1, next)
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

func geminiElidedNode() map[string]any {
	return map[string]any{"type": "string", "description": geminiSchemaElided}
}

// geminiTruncatedNode stands in for a subtree the node budget cut off. It is
// a valid schema, so the tool still reaches the model with the shape it did
// manage to describe.
func geminiTruncatedNode() map[string]any {
	return map[string]any{"type": "string", "description": geminiSchemaTruncated}
}
