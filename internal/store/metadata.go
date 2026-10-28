package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Document metadata (ADR-025). It lives on documents.metadata and nowhere
// else: chunks carry no copy, so changing it is one row update and the next
// filtered search sees it.
//
// The shape is deliberately small, because the filter grammar has to be able
// to answer every value with a containment predicate (@>) a GIN
// jsonb_path_ops index serves:
//
//	{"lang": "tr", "year": 2026, "draft": false, "tags": ["a", "b"]}
//
// A value is a string, a number, a boolean or a flat array of those. There is
// no nesting and no null.
const (
	// MaxMetadataKeys bounds the number of keys of one document.
	MaxMetadataKeys = 32
	// MaxMetadataBytes bounds the size of one document's metadata as the
	// database hands it back (jsonb's text form, which is what every API
	// response carries), not the size of what was submitted: PostgreSQL stores
	// a number as a numeric and prints it in full, so "1e308" is 5 bytes in and
	// 309 bytes out. See renderedSize.
	MaxMetadataBytes = 8 << 10
	// MaxMetadataStringBytes bounds one string value.
	MaxMetadataStringBytes = 256
	// MaxMetadataArrayLen bounds an array value and a filter's "in" list.
	MaxMetadataArrayLen = 100
	// MaxMetadataNumberLen bounds the text of one number.
	MaxMetadataNumberLen = 64
	// MaxMetadataExponent bounds the explicit decimal exponent of a number:
	// the range of a float64 ("1e308" is the largest, "1e-308" the smallest).
	// Go's JSON parser accepts any exponent, PostgreSQL's numeric rejects the
	// extreme ones (a 500 where a 400 belongs) and prints the others in full:
	// '1e130000'::jsonb is 70 bytes stored and 130 000 digits when read back.
	// At 308 a number is at most ~310 characters when read back, and
	// MaxMetadataBytes counts them that way.
	MaxMetadataExponent = 308
	// maxMetadataInputBytes is a guard on the raw input before it is parsed.
	// It is generous (whitespace does not count towards MaxMetadataBytes) and
	// only keeps a pathological body from costing a parse.
	maxMetadataInputBytes = 64 << 10
)

var metadataKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ErrInvalidMetadata is wrapped by every error ValidateMetadata and
// ParseMetadataFilter return: the caller's input is not acceptable (a 400).
// The messages never echo a value, and echo a key only when it has the
// allowed shape.
var ErrInvalidMetadata = errors.New("invalid metadata")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMetadata, fmt.Sprintf(format, args...))
}

// decodeObject splits a JSON object into its keys and raw values, in input
// order. A duplicate key is an error: which of the two would win is a
// parser accident, not a decision the caller made.
func decodeObject(raw []byte, what string) ([]string, []json.RawMessage, error) {
	if len(raw) > maxMetadataInputBytes {
		return nil, nil, invalidf("%s is too large", what)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		return nil, nil, invalidf("%s must be a JSON object", what)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // the opening brace
		return nil, nil, invalidf("%s must be a JSON object", what)
	}
	var keys []string
	var vals []json.RawMessage
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, invalidf("%s must be a JSON object", what)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, nil, invalidf("%s must be a JSON object", what)
		}
		if seen[key] {
			return nil, nil, invalidf("%s repeats a key", what)
		}
		seen[key] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, invalidf("%s must be a JSON object", what)
		}
		keys = append(keys, key)
		vals = append(vals, v)
	}
	return keys, vals, nil
}

// scalar validates one metadata or filter scalar (string, number, boolean) and
// returns its canonical JSON text.
func scalar(raw json.RawMessage, key, what string) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, invalidf("%s %q: empty value", what, key)
	}
	switch c := raw[0]; {
	case c == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, invalidf("%s %q: malformed string", what, key)
		}
		if len(s) > MaxMetadataStringBytes {
			return nil, invalidf("%s %q: a string value is at most %d bytes", what, key, MaxMetadataStringBytes)
		}
		// jsonb cannot hold U+0000, and a value the database refuses would
		// surface as a 500 instead of the 400 it deserves.
		if strings.ContainsRune(s, 0) || !utf8.ValidString(s) {
			return nil, invalidf("%s %q: a string value must be valid text without NUL", what, key)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(s); err != nil {
			return nil, invalidf("%s %q: malformed string", what, key)
		}
		return bytes.TrimRight(buf.Bytes(), "\n"), nil
	case c == 't' || c == 'f':
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, invalidf("%s %q: unsupported value", what, key)
		}
		if b {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case c == '-' || (c >= '0' && c <= '9'):
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, invalidf("%s %q: malformed number", what, key)
		}
		if err := checkNumber(n.String()); err != nil {
			return nil, invalidf("%s %q: %v", what, key, err)
		}
		return []byte(n.String()), nil
	case c == 'n':
		return nil, invalidf("%s %q: null is not allowed", what, key)
	default:
		return nil, invalidf("%s %q: nested objects and arrays are not allowed here", what, key)
	}
}

// checkNumber bounds a number that is already valid JSON (ADR-025): its text
// length, its explicit exponent (see MaxMetadataExponent) and that it is
// finite as a float64 ("9e308" has an allowed exponent and still overflows).
// The mantissa needs no limit of its own, it is no longer than the text.
func checkNumber(text string) error {
	if len(text) > MaxMetadataNumberLen {
		return fmt.Errorf("a number is at most %d characters", MaxMetadataNumberLen)
	}
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exp, err := strconv.Atoi(strings.TrimPrefix(text[i+1:], "+"))
		if err != nil || exp > MaxMetadataExponent || exp < -MaxMetadataExponent {
			return fmt.Errorf("a number's exponent must be between -%d and %d", MaxMetadataExponent, MaxMetadataExponent)
		}
	}
	if _, err := strconv.ParseFloat(text, 64); err != nil {
		return errors.New("a number must be finite")
	}
	return nil
}

// renderedScalarLen computes how many bytes PostgreSQL's jsonb text output uses for
// the pieces ValidateMetadata assembles. The rules are the database's, and
// TestRenderedSizeMatchesPostgreSQL holds this file to them against a real
// server:
//
//   - objects print as {"k": v, "k2": v2} and arrays as [v, v2]: ": " and ", "
//     are two bytes each, the empty ones are "{}" and "[]";
//   - a number is a numeric, printed in plain decimal notation with the scale
//     it was written with: 1e308 is 309 digits, 1e-308 is "0." and 308
//     digits, 1.50 stays 1.50, 1.5e3 is 1500, and zero has no sign;
//   - a string is escaped as JSON with the shortest escapes: \" \\ \b \f \n
//     \r \t, \u00XX for the other control characters, everything else raw.
func renderedScalarLen(c []byte) int {
	switch c[0] {
	case '"':
		var str string
		_ = json.Unmarshal(c, &str)
		n := 2
		for _, r := range str {
			switch {
			case r == '"' || r == '\\' || r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
				n += 2
			case r < 0x20:
				n += 6
			default:
				n += utf8.RuneLen(r)
			}
		}
		return n
	case 't', 'f':
		return len(c)
	}
	return len(numericText(string(c)))
}

// numericText is a JSON number the way numeric_out prints it. text has passed
// checkNumber, so the exponent is small.
func numericText(text string) string {
	neg := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	exp := 0
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exp, _ = strconv.Atoi(strings.TrimPrefix(text[i+1:], "+"))
		text = text[:i]
	}
	whole, frac, _ := strings.Cut(text, ".")
	digits := whole + frac
	point := len(whole) + exp // where the decimal point falls in digits
	var intPart, fracPart string
	switch {
	case point >= len(digits):
		intPart = digits + strings.Repeat("0", point-len(digits))
	case point > 0:
		intPart, fracPart = digits[:point], digits[point:]
	default:
		fracPart = strings.Repeat("0", -point) + digits
	}
	// The fraction is as long as the scale the number was written with (the
	// fraction digits less the exponent, never negative), zeros included.
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	out := intPart
	if fracPart != "" {
		out += "." + fracPart
	}
	if neg && strings.Trim(digits, "0") != "" {
		out = "-" + out
	}
	return out
}

func checkKey(key, what string) error {
	if !metadataKeyRE.MatchString(key) {
		return invalidf("%s key must match %s", what, metadataKeyRE.String())
	}
	return nil
}

// ValidateMetadata checks a document's metadata against the ADR-025 shape and
// returns its canonical compact JSON, ready for the jsonb column. An empty
// object is valid and clears the metadata.
func ValidateMetadata(raw json.RawMessage) (json.RawMessage, error) {
	keys, vals, err := decodeObject(raw, "metadata")
	if err != nil {
		return nil, err
	}
	if len(keys) > MaxMetadataKeys {
		return nil, invalidf("metadata has more than %d keys", MaxMetadataKeys)
	}
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return keys[order[a]] < keys[order[b]] })

	var out bytes.Buffer
	out.WriteByte('{')
	// size is what the database will print for this document (see
	// renderedScalarLen), checked as it grows so that a document of huge
	// numbers is refused before it is built.
	size := 2
	over := func() error {
		if size > MaxMetadataBytes {
			return invalidf("metadata is larger than %d bytes (as stored: a number is kept in full decimal form, so 1e308 is 309 bytes)", MaxMetadataBytes)
		}
		return nil
	}
	for n, i := range order {
		key := keys[i]
		if err := checkKey(key, "metadata"); err != nil {
			return nil, err
		}
		if n > 0 {
			out.WriteByte(',')
			size += 2 // ", "
		}
		kj, _ := json.Marshal(key)
		out.Write(kj)
		out.WriteByte(':')
		size += len(kj) + 2 // key and ": "

		v := bytes.TrimSpace(vals[i])
		if len(v) > 0 && v[0] == '[' {
			var elems []json.RawMessage
			if err := json.Unmarshal(v, &elems); err != nil {
				return nil, invalidf("metadata %q: malformed array", key)
			}
			if len(elems) > MaxMetadataArrayLen {
				return nil, invalidf("metadata %q: an array holds at most %d values", key, MaxMetadataArrayLen)
			}
			out.WriteByte('[')
			size += 2 // brackets
			for j, e := range elems {
				c, err := scalar(e, key, "metadata")
				if err != nil {
					return nil, err
				}
				if j > 0 {
					out.WriteByte(',')
					size += 2
				}
				out.Write(c)
				size += renderedScalarLen(c)
				if err := over(); err != nil {
					return nil, err
				}
			}
			out.WriteByte(']')
			continue
		}
		c, err := scalar(v, key, "metadata")
		if err != nil {
			return nil, err
		}
		out.Write(c)
		size += renderedScalarLen(c)
		if err := over(); err != nil {
			return nil, err
		}
	}
	out.WriteByte('}')
	if err := over(); err != nil {
		return nil, err
	}
	return json.RawMessage(out.Bytes()), nil
}

// MetadataKeys returns the sorted key names of an already valid metadata
// document. Audit records them; it never records a value.
func MetadataKeys(raw json.RawMessage) []string {
	keys, _, err := decodeObject(raw, "metadata")
	if err != nil {
		return nil
	}
	sort.Strings(keys)
	return keys
}

// MetadataFilter is a parsed ADR-025 filter: conditions on document metadata,
// ANDed across keys. A value is a scalar (equality, JSON-type aware) or
// {"in": [...]} (any of 1-100 scalars). Against an array-valued document key
// both mean "contains".
//
// The zero value and nil filter nothing.
type MetadataFilter struct {
	conds []filterCond
}

type filterCond struct {
	key string
	// vals are canonical scalar JSON texts; the condition holds when the
	// document's value for key equals, or is an array containing, any of them.
	vals [][]byte
}

// ParseMetadataFilter parses the grammar of ADR-025. Anything outside it is
// an ErrInvalidMetadata: no ranges, no "not", no "or", no nested objects.
func ParseMetadataFilter(raw json.RawMessage) (*MetadataFilter, error) {
	keys, vals, err := decodeObject(raw, "filter")
	if err != nil {
		return nil, err
	}
	if len(keys) > MaxMetadataKeys {
		return nil, invalidf("filter has more than %d keys", MaxMetadataKeys)
	}
	f := &MetadataFilter{}
	for i, key := range keys {
		if err := checkKey(key, "filter"); err != nil {
			return nil, err
		}
		v := bytes.TrimSpace(vals[i])
		cond := filterCond{key: key}
		switch {
		case len(v) > 0 && v[0] == '{':
			opKeys, opVals, err := decodeObject(v, "filter")
			if err != nil {
				return nil, err
			}
			if len(opKeys) != 1 || opKeys[0] != "in" {
				return nil, invalidf("filter %q: the only operator is {\"in\": [...]}", key)
			}
			var elems []json.RawMessage
			if t := bytes.TrimSpace(opVals[0]); len(t) == 0 || t[0] != '[' || json.Unmarshal(t, &elems) != nil {
				return nil, invalidf("filter %q: \"in\" takes an array", key)
			}
			if len(elems) < 1 || len(elems) > MaxMetadataArrayLen {
				return nil, invalidf("filter %q: \"in\" takes between 1 and %d values", key, MaxMetadataArrayLen)
			}
			for _, e := range elems {
				c, err := scalar(e, key, "filter")
				if err != nil {
					return nil, err
				}
				cond.vals = append(cond.vals, c)
			}
		case len(v) > 0 && v[0] == '[':
			return nil, invalidf("filter %q: use {\"in\": [...]} to match one of several values", key)
		default:
			c, err := scalar(v, key, "filter")
			if err != nil {
				return nil, err
			}
			cond.vals = [][]byte{c}
		}
		f.conds = append(f.conds, cond)
	}
	return f, nil
}

// Empty reports whether the filter restricts nothing.
func (f *MetadataFilter) Empty() bool { return f == nil || len(f.conds) == 0 }

// Keys returns the filtered key names, sorted.
func (f *MetadataFilter) Keys() []string {
	if f == nil {
		return nil
	}
	out := make([]string, len(f.conds))
	for i, c := range f.conds {
		out[i] = c.key
	}
	sort.Strings(out)
	return out
}

// predicate compiles the filter to a SQL condition on <alias>.metadata. Every
// condition is one containment test against an array of documents, so a GIN
// jsonb_path_ops index on the column can answer it:
//
//	d.metadata @> ANY($n::text[]::jsonb[])
//
// For each wanted value v of key k the array holds {"k": v} (the document
// stores the value itself) and {"k": [v]} (it stores an array holding it):
// jsonb containment only lets an array "contain" a bare value at the top
// level, not under a key, so the second form is what makes an array-valued
// key match a scalar filter.
//
// Values travel as bind parameters, starting at placeholder $first; the
// alias is a constant of the calling query, never input. An empty filter
// compiles to "" and no arguments.
func (f *MetadataFilter) predicate(alias string, first int) (string, []any) {
	if f.Empty() {
		return "", nil
	}
	parts := make([]string, 0, len(f.conds))
	args := make([]any, 0, len(f.conds))
	for i, c := range f.conds {
		kj, _ := json.Marshal(c.key)
		docs := make([]string, 0, 2*len(c.vals))
		for _, v := range c.vals {
			docs = append(docs,
				"{"+string(kj)+":"+string(v)+"}",
				"{"+string(kj)+":["+string(v)+"]}")
		}
		parts = append(parts, fmt.Sprintf("%s.metadata @> ANY($%d::text[]::jsonb[])", alias, first+i))
		args = append(args, docs)
	}
	return strings.Join(parts, " AND "), args
}

// documentSet is the SQL for "the documents of rag store $storeParam that the
// filter lets through", as a subquery of ids.
func (f *MetadataFilter) documentSet(storeParam string, first int) (string, []any) {
	pred, args := f.predicate("fd", first)
	return "SELECT fd.id FROM documents fd WHERE fd.rag_store_id = " + storeParam + " AND " + pred, args
}

// HasMatchingDocuments reports whether the filter lets at least one document
// with chunks through. Retrieval asks it before embedding the query: a filter
// nothing matches has an empty answer that costs no embedding call.
func (s *Store) HasMatchingDocuments(ctx context.Context, storeID int64, f *MetadataFilter) (bool, error) {
	if f.Empty() {
		return true, nil
	}
	pred, args := f.predicate("fd", 2)
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM documents fd WHERE fd.rag_store_id = $1 AND fd.chunk_count > 0 AND `+pred+`)`,
		append([]any{storeID}, args...)...).Scan(&ok)
	return ok, err
}

// SetDocumentMetadata replaces a document's metadata. meta must come from
// ValidateMetadata. It touches the one row: no chunk is rewritten and nothing
// is reprocessed, which is the point of keeping metadata on documents.
func (s *Store) SetDocumentMetadata(ctx context.Context, id int64, meta json.RawMessage) (*Document, error) {
	return scanDoc(s.pool.QueryRow(ctx,
		"UPDATE documents SET metadata = $1::jsonb, updated_at = now() WHERE id = $2 RETURNING "+docCols,
		[]byte(meta), id))
}

// DocumentMetadataByID returns the metadata of the given documents, keyed by
// id. A document that no longer exists is simply absent.
func (s *Store) DocumentMetadataByID(ctx context.Context, ids []int64) (map[int64]json.RawMessage, error) {
	out := make(map[int64]json.RawMessage, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, "SELECT id, metadata FROM documents WHERE id = ANY($1)", ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var m json.RawMessage
		if err := rows.Scan(&id, &m); err != nil {
			return nil, err
		}
		out[id] = m
	}
	return out, rows.Err()
}
