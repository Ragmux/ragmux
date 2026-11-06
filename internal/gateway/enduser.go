package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// End users and tags (ADR-035).
//
// An end user is who a request is made for, in the client's own words. It
// reaches the request log row, the per-end-user limit tier and, from the
// body only, a provider that takes such a field. It is never a metric label,
// a span attribute or a log field (ADR-002, PRD rule 10): it is the client's
// own string, and an unbounded set.

// The headers, and the codes of the 400s a malformed value gets.
const (
	endUserHeader = "X-Ragmux-End-User"
	tagsHeader    = "X-Ragmux-Tags"

	errInvalidEndUser = "invalid_end_user"
	errInvalidTags    = "invalid_tags"
)

// endUserValueRule is what a usable end user id looks like, for the texts that
// refuse or warn about one that is not.
var endUserValueRule = "1 to " + strconv.Itoa(provider.MaxEndUserBytes) + " characters of letters, digits and . _ : @ + = / -"

// endUserFrom reads X-Ragmux-End-User. present is false when the request sends
// none; ok is false when it sends one that is not usable.
//
// A value that is not usable is refused, not ignored: the header is Ragmux's
// own and a client that sets it means it, so a typo answered with a silent
// "no end user" would escape the per-end-user ceiling by being malformed. A
// header sent twice is refused too, because there is no telling which of the
// two the client means.
func endUserFrom(r *http.Request) (id string, present, ok bool) {
	vals := r.Header.Values(endUserHeader)
	if len(vals) == 0 {
		return "", false, true
	}
	if len(vals) != 1 || !provider.ValidEndUser(vals[0]) {
		return "", true, false
	}
	return vals[0], true, true
}

// validTag reports whether t is a usable tag: 1 to 64 bytes of ASCII letters,
// digits and . _ : / -.
func validTag(t string) bool {
	if len(t) == 0 || len(t) > store.MaxTagBytes {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == ':', c == '/', c == '-':
		default:
			return false
		}
	}
	return true
}

// tagsError is a malformed set of tags. Its message is for the client.
type tagsError struct{ msg string }

func (e *tagsError) Error() string { return e.msg }

// checkTags validates a set of tags and returns it sorted and without
// duplicates. A repeated tag is not an error and counts once toward the limit
// of store.MaxTags. The set is validated whole: one bad tag refuses it all.
func checkTags(in []string) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if !validTag(t) {
			return nil, &tagsError{"a tag is 1 to " + strconv.Itoa(store.MaxTagBytes) + " characters of letters, digits and . _ : / -"}
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) > store.MaxTags {
		return nil, &tagsError{"at most " + strconv.Itoa(store.MaxTags) + " tags are allowed"}
	}
	sort.Strings(out)
	return out, nil
}

// tagsFromHeader reads X-Ragmux-Tags, a comma-separated list. present is false
// when the request sends none. Spaces around a comma are dropped; an empty item
// (a trailing comma, an empty header) is a malformed value, like any other.
func tagsFromHeader(r *http.Request) (tags []string, present bool, err error) {
	vals := r.Header.Values(tagsHeader)
	if len(vals) == 0 {
		return nil, false, nil
	}
	var items []string
	for _, v := range vals {
		for _, item := range strings.Split(v, ",") {
			items = append(items, strings.Trim(item, " \t"))
		}
	}
	tags, err = checkTags(items)
	return tags, true, err
}

// asTagsError is the error a malformed tags value is answered with: the code
// that names it, and the message. ok is false for any other error.
func asTagsError(err error) (code, msg string, ok bool) {
	var te *tagsError
	if errors.As(err, &te) {
		return errInvalidTags, te.msg, true
	}
	return "", "", false
}

// bodyEndUserField is one place a body may carry an end user: the field's
// name for the Warning text, its string value, and whether the field is filled
// at all. A field that is present but not a string is filled, and not usable.
type bodyEndUserField struct {
	name   string
	value  string
	filled bool
}

// pickBodyEndUser chooses the end user a body carries: the first filled field
// of fields, in the order given. A filled field that is not usable is not
// skipped for the next one -- the client named an end user, and a different
// field's value is not it -- and earns the warning. value is empty when no
// field is filled or the chosen one is not usable.
func pickBodyEndUser(fields ...bodyEndUserField) (value, warning string) {
	for _, f := range fields {
		if !f.filled {
			continue
		}
		if provider.ValidEndUser(f.value) {
			return f.value, ""
		}
		return "", f.name + " is not a usable end user id (" + endUserValueRule + "); it is not recorded as the end user"
	}
	return "", ""
}

// applyBodyEndUser records the end user a body carries on the call and returns
// the Warning text of an unusable field, empty for none. The
// X-Ragmux-End-User header, when the request sent one, stays the end user of
// record (ADR-035: what is Ragmux's own wins over the provider's field). The
// body's usable value is also set aside as providerEndUser, for a translated
// provider request, and goes there whatever the header says; the header's value
// never does.
func (c *call) applyBodyEndUser(fields ...bodyEndUserField) (warning string) {
	value, warning := pickBodyEndUser(fields...)
	if value != "" {
		c.providerEndUser = value
		c.setEndUser(value, false)
	}
	return warning
}

// extraString reads a string field of a request's unknown fields.
func extraString(extra map[string]json.RawMessage, key string) (value string, filled bool) {
	return rawString(extra[key])
}

// rawString reads a string out of a JSON value. filled is false for an absent,
// null or empty one. A value of another type is filled and has no usable
// value, which is how pickBodyEndUser treats it: the client did name
// something, and a number is not an end user id.
func rawString(raw json.RawMessage) (value string, filled bool) {
	if t := bytes.TrimSpace(raw); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", true
	}
	return s, s != ""
}

// setEndUser sets the end user of record. fromHeader says the value is the
// header's: it wins over one the body gave.
func (c *call) setEndUser(id string, fromHeader bool) {
	if c.endUser != "" && !fromHeader {
		return
	}
	c.endUser, c.subject.EndUser = id, id
}

// applyTags resolves the call's tags once the body has been read. ext is
// ragmux.tags and extSet says the request carried it; the header's tags were
// read by authenticate. When both are there the extension wins -- both are
// Ragmux's own, so the one specific to the request beats the one a proxy may
// add -- and the returned text is the Warning that says the header was not
// used. It is empty otherwise.
func (c *call) applyTags(ext []string, extSet bool) (warning string) {
	if !extSet {
		return ""
	}
	c.tags = ext
	if c.headerTagsSet {
		return tagsHeader + " is ignored: ragmux.tags is set and takes precedence"
	}
	return ""
}

// stampIdentity writes the end user and the tags on a request log row.
func (c *call) stampIdentity(rec *store.RequestLog) {
	rec.EndUser = c.endUser
	rec.Tags = c.tags
}
