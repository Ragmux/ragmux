package provider

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

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
			name: "unresolvable ref elided",
			in:   `{"type":"object","properties":{"a":{"$ref":"#/components/schemas/Missing"}}}`,
			want: `{"properties":{"a":{"description":"(recursive schema elided)","type":"string"}},"type":"object"}`,
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
		})
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
