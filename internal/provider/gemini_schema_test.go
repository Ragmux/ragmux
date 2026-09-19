package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// geminiSchemaTypes is the Type enum of Gemini's Schema message.
var geminiSchemaTypes = map[string]bool{
	"string": true, "number": true, "integer": true, "boolean": true, "array": true, "object": true,
}

// assertGeminiSubset walks a sanitised schema and fails on anything Gemini's
// Schema message cannot hold: an unknown keyword, a value of the wrong JSON
// type, or an enum on a node that is not a string. It is the assertion the
// sanitiser's whole reason for existing comes down to, so every case in the
// table runs through it rather than only comparing bytes.
func assertGeminiSubset(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("sanitised schema is not an object: %v (%s)", err, raw)
	}
	assertGeminiNode(t, "#", node)
}

func assertGeminiNode(t *testing.T, path string, node map[string]any) {
	t.Helper()
	if len(node) == 0 {
		// Gemini needs a typed node; the worst case this file may produce is
		// {"type":"object"}, never nothing at all.
		t.Errorf("%s: empty schema node", path)
	}
	typ, _ := node["type"].(string)
	// Every node is typed, or is a union of typed ones. Only checking for an
	// empty node left {"description":"hi"} -- valid JSON Schema, and an error
	// from Gemini -- passing the assertion that carries this file's promise.
	if typ == "" && node["anyOf"] == nil {
		t.Errorf("%s: node has no type: %#v", path, node)
	}
	// What this does not check: whether the type and the keywords agree.
	// {"type":"string","minItems":2} is a shape Gemini accepts and a
	// contradiction all the same, and an inferred type can produce one --
	// see geminiInferType. Gemini takes every field of Schema independently,
	// so there is no single rule to assert here; the inference's own cases in
	// the table are what pin its choices.
	str := func(k string, v any) {
		if _, ok := v.(string); !ok {
			t.Errorf("%s/%s = %#v, want a string", path, k, v)
		}
	}
	num := func(k string, v any) {
		if _, ok := v.(float64); !ok {
			t.Errorf("%s/%s = %#v, want a number", path, k, v)
		}
	}
	child := func(k string, v any) {
		sub, ok := v.(map[string]any)
		if !ok {
			t.Errorf("%s/%s = %#v, want a schema object", path, k, v)
			return
		}
		assertGeminiNode(t, path+"/"+k, sub)
	}
	for k, v := range node {
		switch k {
		case "type":
			if s, ok := v.(string); !ok || !geminiSchemaTypes[s] {
				t.Errorf("%s/type = %#v, not a Gemini Type", path, v)
			}
		case "description", "pattern":
			str(k, v)
		case "format":
			s, ok := v.(string)
			if !ok || !geminiSchemaFormats[typ][s] {
				t.Errorf("%s/format = %#v on a %q node", path, v, typ)
			}
		case "nullable":
			if _, ok := v.(bool); !ok {
				t.Errorf("%s/nullable = %#v, want a bool", path, v)
			}
		case "minimum", "maximum", "minItems", "maxItems", "minLength", "maxLength":
			num(k, v)
		case "enum":
			// Schema.enum is repeated string and Gemini reads it only on a
			// STRING node.
			list, ok := v.([]any)
			if !ok || len(list) == 0 {
				t.Errorf("%s/enum = %#v, want a non-empty array", path, v)
				continue
			}
			if typ != "string" {
				t.Errorf("%s/enum is set on a %q node", path, typ)
			}
			for i, e := range list {
				str(fmt.Sprintf("enum/%d", i), e)
			}
		case "required":
			list, ok := v.([]any)
			if !ok {
				t.Errorf("%s/required = %#v, want an array", path, v)
				continue
			}
			for i, e := range list {
				str(fmt.Sprintf("required/%d", i), e)
			}
		case "items":
			child(k, v)
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				t.Errorf("%s/properties = %#v, want an object", path, v)
				continue
			}
			for name, p := range props {
				child("properties/"+name, p)
			}
		case "anyOf":
			list, ok := v.([]any)
			if !ok {
				t.Errorf("%s/anyOf = %#v, want an array", path, v)
				continue
			}
			for i, b := range list {
				child(fmt.Sprintf("anyOf/%d", i), b)
			}
		default:
			t.Errorf("%s: keyword %q is not in Gemini's subset", path, k)
		}
	}
}

func TestGeminiSchemaSanitize(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		dropped []string
	}{
		{
			name:    "strict mode keywords",
			in:      `{"$schema":"https://json-schema.org/draft/2020-12/schema","title":"Args","type":"object","additionalProperties":false,"properties":{"city":{"type":"string","description":"where","default":"Ankara"}},"required":["city"]}`,
			want:    `{"properties":{"city":{"description":"where","type":"string"}},"required":["city"],"type":"object"}`,
			dropped: []string{"$schema", "additionalProperties", "default", "title"},
		},
		{
			name: "ref inlined from $defs",
			in:   `{"type":"object","$defs":{"Point":{"type":"object","properties":{"x":{"type":"integer"}}}},"properties":{"p":{"$ref":"#/$defs/Point"}}}`,
			want: `{"properties":{"p":{"properties":{"x":{"type":"integer"}},"type":"object"}},"type":"object"}`,
		},
		{
			name: "cyclic ref elided",
			in:   `{"type":"object","$defs":{"Node":{"type":"object","properties":{"next":{"$ref":"#/$defs/Node"}}}},"properties":{"root":{"$ref":"#/$defs/Node"}}}`,
			want: `{"properties":{"root":{"properties":{"next":{"description":"(recursive schema elided)","type":"string"}},"type":"object"}},"type":"object"}`,
		},
		{
			// A reference matched on its full pointer is unresolvable far more
			// often than it is recursive -- every external one is -- so it
			// says which of the two happened.
			name: "unresolvable ref elided",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/components/schemas/Missing"}}}`,
			want: `{"properties":{"a":{"description":"(unresolved schema reference elided)","type":"string"}},"type":"object"}`,
		},
		{
			name: "nullable union",
			in:   `{"type":"object","properties":{"a":{"type":["string","null"]}}}`,
			want: `{"properties":{"a":{"nullable":true,"type":"string"}},"type":"object"}`,
		},
		{
			name: "const becomes a one-value enum",
			in:   `{"type":"object","properties":{"kind":{"const":"circle"}}}`,
			want: `{"properties":{"kind":{"enum":["circle"],"type":"string"}},"type":"object"}`,
		},
		{
			name: "oneOf becomes anyOf",
			in:   `{"type":"object","properties":{"a":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}`,
			want: `{"properties":{"a":{"anyOf":[{"type":"string"},{"type":"integer"}]}},"type":"object"}`,
		},
		{
			name: "allOf of objects is merged",
			in:   `{"allOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"integer"}}}]}`,
			want: `{"properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["a"],"type":"object"}`,
		},
		{
			name: "exclusive bounds become inclusive ones",
			in:   `{"type":"object","properties":{"n":{"type":"number","exclusiveMinimum":0,"exclusiveMaximum":10}}}`,
			want: `{"properties":{"n":{"maximum":10,"minimum":0,"type":"number"}},"type":"object"}`,
		},
		{
			name:    "format kept only where gemini knows it",
			in:      `{"type":"object","properties":{"at":{"type":"string","format":"date-time"},"mail":{"type":"string","format":"email"},"n":{"type":"integer","format":"int64"}}}`,
			want:    `{"properties":{"at":{"format":"date-time","type":"string"},"mail":{"type":"string"},"n":{"format":"int64","type":"integer"}},"type":"object"}`,
			dropped: []string{"format"},
		},
		{
			name:    "unknown keywords dropped",
			in:      `{"type":"object","properties":{"a":{"type":"string"}},"patternProperties":{"^x":{"type":"string"}},"not":{"type":"null"},"if":{"type":"object"},"then":{"type":"object"},"unevaluatedProperties":false,"examples":[1],"$comment":"hi"}`,
			want:    `{"properties":{"a":{"type":"string"}},"type":"object"}`,
			dropped: []string{"$comment", "examples", "if", "not", "patternProperties", "then", "unevaluatedProperties"},
		},
		{
			name: "array items",
			in:   `{"type":"object","properties":{"xs":{"type":"array","items":{"type":"string"},"minItems":1}}}`,
			want: `{"properties":{"xs":{"items":{"type":"string"},"minItems":1,"type":"array"}},"type":"object"}`,
		},
		{name: "not an object degrades", in: `"nonsense"`, want: `{"type":"object"}`},
		{name: "boolean schema degrades", in: `true`, want: `{"type":"object"}`},

		// The four inputs the v0.4 review measured going through untouched.
		// Every one of them left the sanitiser holding a schema Gemini
		// rejects, which is the one thing this file promises not to produce.
		{
			// Schema.enum is repeated string, so a numeric enum has no
			// equivalent; spelling the members as "1","2","3" would tell the
			// model to send strings where the tool wants integers.
			name:    "numeric enum dropped",
			in:      `{"type":"integer","enum":[1,2,3]}`,
			want:    `{"type":"integer"}`,
			dropped: []string{"enum"},
		},
		{
			// This used to sanitise to {"enum":[7],"type":"string"} -- a
			// type/value contradiction the sanitiser invented itself. A
			// non-string const can still pin the node's type.
			name: "non-string const pins the type instead of inventing an enum",
			in:   `{"const":7}`,
			want: `{"type":"integer"}`,
		},
		{
			name:    "object description dropped",
			in:      `{"description":{"nested":"object"}}`,
			want:    `{"type":"object"}`,
			dropped: []string{"description"},
		},
		{
			name:    "non-numeric bound dropped",
			in:      `{"type":"integer","minimum":"not-a-number"}`,
			want:    `{"type":"integer"}`,
			dropped: []string{"minimum"},
		},

		{
			name: "const of every other shape",
			in: `{"type":"object","properties":{"f":{"const":1.5},"b":{"const":true},` +
				`"a":{"const":[1]},"o":{"const":{"x":1}},"n":{"const":null},"kept":{"type":"integer","const":3}}}`,
			want: `{"properties":{"a":{"type":"array"},"b":{"type":"boolean"},"f":{"type":"number"},` +
				`"kept":{"type":"integer"},"n":{"type":"object"},"o":{"type":"object"}},"type":"object"}`,
			dropped: []string{"const"},
		},
		{
			// An enum Gemini would read on a node that is not a string is the
			// same contradiction from the other direction.
			name:    "string enum on a non-string node dropped",
			in:      `{"type":"object","properties":{"n":{"type":"integer","enum":["a","b"]}}}`,
			want:    `{"properties":{"n":{"type":"integer"}},"type":"object"}`,
			dropped: []string{"enum"},
		},
		{
			name: "scalars of the wrong shape dropped",
			in: `{"type":"object","properties":{"s":{"type":"string","pattern":["x"],"maxLength":{},` +
				`"nullable":"yes","description":null,"minLength":2}}}`,
			want:    `{"properties":{"s":{"minLength":2,"type":"string"}},"type":"object"}`,
			dropped: []string{"description", "maxLength", "nullable", "pattern"},
		},
		{
			// Valid JSON Schema, and an error from Gemini: an untyped node
			// takes the type its surviving keywords imply.
			name: "untyped nodes are typed from what is left",
			in: `{"properties":{"note":{"description":"free text"},"n":{"minimum":1},` +
				`"xs":{"items":{"type":"string"}},"o":{"properties":{"a":{"type":"string"}}}}}`,
			want: `{"properties":{"n":{"minimum":1,"type":"number"},` +
				`"note":{"description":"free text","type":"string"},` +
				`"o":{"properties":{"a":{"type":"string"}},"type":"object"},` +
				`"xs":{"items":{"type":"string"},"type":"array"}},"type":"object"}`,
		},
		{
			// Schema.minimum is a double; json.Number validates the spelling
			// and not the range, so this used to travel as a literal Gemini
			// cannot hold.
			name:    "out-of-range bound dropped",
			in:      `{"type":"number","minimum":1e400,"maximum":2}`,
			want:    `{"maximum":2,"type":"number"}`,
			dropped: []string{"minimum"},
		},
		{
			// Two branches naming the same field used to repeat it once per
			// branch, and nested allOf chains multiplied that at every level.
			name: "merged required is a set",
			in: `{"allOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a","b"]},` +
				`{"type":"object","properties":{"b":{"type":"string"}},"required":["b","a"]}]}`,
			want: `{"properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"],"type":"object"}`,
		},
		{
			// Schema declares the count bounds as int64, not double, so a
			// fractional or oversized one is as unusable as a non-numeric.
			name:    "non-integer count bounds dropped",
			in:      `{"type":"array","items":{"type":"string"},"minItems":1.5,"maxItems":2.7}`,
			want:    `{"items":{"type":"string"},"type":"array"}`,
			dropped: []string{"maxItems", "minItems"},
		},
		{
			name:    "out-of-range and negative count bounds dropped",
			in:      `{"type":"string","minLength":-4,"maxLength":1e30}`,
			want:    `{"type":"string"}`,
			dropped: []string{"maxLength", "minLength"},
		},
		{
			name: "whole count bounds kept",
			in:   `{"type":"string","minLength":0,"maxLength":128}`,
			want: `{"maxLength":128,"minLength":0,"type":"string"}`,
		},
		{
			// A union is typed by its branches; typing the node for an enum's
			// sake would contradict all of them.
			name:    "enum on a union dropped rather than typing it",
			in:      `{"type":"object","properties":{"a":{"anyOf":[{"type":"string"},{"type":"integer"}],"enum":["x"]}}}`,
			want:    `{"properties":{"a":{"anyOf":[{"type":"string"},{"type":"integer"}]}},"type":"object"}`,
			dropped: []string{"enum"},
		},
		{
			// Both used to vanish without reaching the one debug line that
			// tells a tool author what the gateway could not carry.
			name:    "malformed required and empty union are named",
			in:      `{"type":"object","required":"a","properties":{"b":{"type":"string","anyOf":[]}}}`,
			want:    `{"properties":{"b":{"type":"string"}},"type":"object"}`,
			dropped: []string{"anyOf", "required"},
		},
		{
			name:    "null exclusive bound dropped",
			in:      `{"type":"object","properties":{"n":{"type":"number","exclusiveMinimum":null}}}`,
			want:    `{"properties":{"n":{"type":"number"}},"type":"object"}`,
			dropped: []string{"exclusiveMinimum"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := sanitizeGeminiSchema(json.RawMessage(tc.in))
			if string(got) != tc.want {
				t.Errorf("schema =\n %s\nwant %s", got, tc.want)
			}
			if strings.Join(dropped, ",") != strings.Join(tc.dropped, ",") {
				t.Errorf("dropped = %v, want %v", dropped, tc.dropped)
			}
			if !json.Valid(got) {
				t.Errorf("result is not valid JSON: %s", got)
			}
			assertGeminiSubset(t, got)
		})
	}
}

// Definitions are keyed by name nowhere any more: "#/$defs/Shape",
// "#/definitions/Shape", a nested "#/properties/d/$defs/Shape" and an
// external "https://…#/definitions/Shape" all used to collide in one table
// entry, so which body a $ref got came down to Go's map iteration order and
// the same schema sanitised to different bytes from one run to the next.
func TestSanitizeGeminiSchemaSameNamedDefs(t *testing.T) {
	const in = `{"type":"object",
		"$defs":{"Shape":{"type":"object","properties":{"outer":{"type":"string"}}}},
		"definitions":{"Shape":{"type":"object","properties":{"sibling":{"type":"integer"}}}},
		"properties":{
			"a":{"$ref":"#/$defs/Shape"},
			"b":{"$ref":"#/definitions/Shape"},
			"c":{"$ref":"https://example.com/s.json#/definitions/Shape"},
			"d":{"$defs":{"Shape":{"type":"object","properties":{"nested":{"type":"boolean"}}}},
				"$ref":"#/properties/d/$defs/Shape"}}}`

	want := `{"properties":{` +
		`"a":{"properties":{"outer":{"type":"string"}},"type":"object"},` +
		`"b":{"properties":{"sibling":{"type":"integer"}},"type":"object"},` +
		`"c":{"description":"(unresolved schema reference elided)","type":"string"},` +
		`"d":{"properties":{"nested":{"type":"boolean"}},"type":"object"}},"type":"object"}`

	first, _ := sanitizeGeminiSchema(json.RawMessage(in))
	if string(first) != want {
		t.Fatalf("schema =\n %s\nwant\n %s", first, want)
	}
	assertGeminiSubset(t, first)
	// Byte-for-byte identical on every run: the walk visits properties and
	// keywords in sorted order, so neither the node budget nor a $ref can
	// land differently from one run to the next.
	for i := 0; i < 100; i++ {
		got, _ := sanitizeGeminiSchema(json.RawMessage(in))
		if !bytes.Equal(got, first) {
			t.Fatalf("run %d produced different bytes:\n %s\nfirst run:\n %s", i, got, first)
		}
	}
}

func TestGeminiSchemaEmptyParameters(t *testing.T) {
	// Several model versions reject {"type":"object","properties":{}}, so a
	// declaration with nothing left is sent without parameters at all.
	for _, in := range []string{`{"type":"object","properties":{}}`, `{"type":"object","additionalProperties":false}`, `{}`} {
		got, _ := sanitizeGeminiSchema(json.RawMessage(in))
		if !geminiSchemaEmpty(got) {
			t.Errorf("%s sanitised to %s, expected it to count as empty", in, got)
		}
	}
	got, _ := sanitizeGeminiSchema(json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`))
	if geminiSchemaEmpty(got) {
		t.Errorf("%s should not count as empty", got)
	}
}

// A schema that nests deeper than the inlining cap still produces something
// Gemini accepts rather than an error or a runaway walk.
func TestGeminiSchemaDepthCap(t *testing.T) {
	deep := `{"type":"object","properties":{"a":` + strings.Repeat(`{"type":"object","properties":{"a":`, 12) +
		`{"type":"string"}` + strings.Repeat(`}}`, 12) + `}}`
	got, _ := sanitizeGeminiSchema(json.RawMessage(deep))
	if !json.Valid(got) || !strings.Contains(string(got), geminiSchemaElided) {
		t.Errorf("schema = %s", got)
	}
}

// A tool schema is client-supplied, so the sanitiser's cost has to be bounded
// by its own budget rather than by the input's shape. Chaining differently
// named definitions defeats the per-branch visited set -- each one is new on
// its branch -- and the depth cap alone does not stop the width from
// multiplying at every level.
func TestSanitizeGeminiSchemaIsBoundedOnFanOut(t *testing.T) {
	const n = 20
	defs := map[string]any{}
	names := []string{"A", "B", "C", "D", "E"}
	for i, name := range names {
		props := map[string]any{}
		for j := 0; j < n; j++ {
			if i+1 < len(names) {
				props[fmt.Sprintf("p%d", j)] = map[string]any{"$ref": "#/$defs/" + names[i+1]}
			} else {
				props[fmt.Sprintf("p%d", j)] = map[string]any{"type": "string"}
			}
		}
		defs[name] = map[string]any{"type": "object", "properties": props}
	}
	raw, err := json.Marshal(map[string]any{"$defs": defs, "$ref": "#/$defs/A"})
	if err != nil {
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out, _ := sanitizeGeminiSchema(raw)
	runtime.ReadMemStats(&after)

	const maxOut = 1 << 20 // 1 MiB of output for 4 KiB of input is already generous
	if len(out) > maxOut {
		t.Errorf("%d bytes of input produced %d bytes of output (cap %d)", len(raw), len(out), maxOut)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Errorf("sanitising allocated %d MiB", grew>>20)
	}
	// Whatever it elides, the result must still be a schema Gemini can read.
	var check map[string]any
	if err := json.Unmarshal(out, &check); err != nil {
		t.Fatalf("output is not an object: %v", err)
	}
}
