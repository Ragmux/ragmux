package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// MessagesBody is the JSON object of a Messages request held field by field.
// Each value stays the raw bytes the client wrote, and the fields keep their
// order, so a caller that changes a few of them (the native /v1/messages path,
// ADR-028) sends the rest exactly as it came.
type MessagesBody struct {
	keys []string
	vals map[string]json.RawMessage
}

// ParseMessagesBody splits a JSON object into its top-level fields. A body
// that is not exactly one JSON object is an error. A key that repeats keeps its
// first position and its last value, the way encoding/json reads it.
func ParseMessagesBody(raw []byte) (*MessagesBody, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("the body must be a JSON object")
	}
	b := &MessagesBody{vals: map[string]json.RawMessage{}}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if _, seen := b.vals[key]; !seen {
			b.keys = append(b.keys, key)
		}
		b.vals[key] = v
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON object")
	}
	return b, nil
}

// Get returns the raw value of a field.
func (b *MessagesBody) Get(key string) (json.RawMessage, bool) {
	v, ok := b.vals[key]
	return v, ok
}

// Has reports whether the field is present, null included.
func (b *MessagesBody) Has(key string) bool {
	_, ok := b.vals[key]
	return ok
}

// Keys lists the fields in the order they were written.
func (b *MessagesBody) Keys() []string { return append([]string(nil), b.keys...) }

// Set replaces a field, or appends it after the others. raw must be valid
// JSON.
func (b *MessagesBody) Set(key string, raw json.RawMessage) {
	if _, ok := b.vals[key]; !ok {
		b.keys = append(b.keys, key)
	}
	b.vals[key] = raw
}

// SetValue is Set for a Go value, marshalled without HTML escaping.
func (b *MessagesBody) SetValue(key string, v any) error {
	raw, err := marshalNoEscape(v)
	if err != nil {
		return err
	}
	b.Set(key, raw)
	return nil
}

// Delete removes a field.
func (b *MessagesBody) Delete(key string) {
	if _, ok := b.vals[key]; !ok {
		return
	}
	delete(b.vals, key)
	for i, k := range b.keys {
		if k == key {
			b.keys = append(b.keys[:i], b.keys[i+1:]...)
			break
		}
	}
}

// Clone copies the field table; the raw values are shared and never mutated.
func (b *MessagesBody) Clone() *MessagesBody {
	c := &MessagesBody{keys: append([]string(nil), b.keys...), vals: make(map[string]json.RawMessage, len(b.vals))}
	for k, v := range b.vals {
		c.vals[k] = v
	}
	return c
}

// Bytes renders the object. Values are written as they are held.
func (b *MessagesBody) Bytes() []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range b.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := marshalNoEscape(k)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(b.vals[k])
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// marshalNoEscape is json.Marshal without the HTML escaping of <, > and &, so
// text the gateway adds to a prompt reaches the model as it is written.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
