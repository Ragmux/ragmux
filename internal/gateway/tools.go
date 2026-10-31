package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ragmux/ragmux/internal/glob"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// Sessions and tool governance (ADR-032).

// sessionHeader carries the session id; claudeSessionHeader is the one Claude
// Code sends on its own, read as a fallback by the endpoints that opt in.
const (
	sessionHeader       = "X-Ragmux-Session"
	claudeSessionHeader = "x-claude-code-session-id"
)

// errInvalidSession is the code of the 400 a malformed session id gets.
const errInvalidSession = "invalid_session_id"

// sessionFrom reads the request's session id. present is false when the
// request names none; ok is false when it names one that is not usable.
//
// A value that is not usable is refused and never ignored: the session cap is
// a narrowing, and a bad value waved through would be a request that escapes
// the cap by being malformed. The accepted set is validRequestID's -- 1 to 128
// bytes of ASCII letters, digits and . _ : - -- which is safe in a header, a
// log line and a SQL text column. A header sent twice is refused too, because
// there is no telling which of the two values the client means.
//
// claudeFallback lets x-claude-code-session-id stand in when X-Ragmux-Session
// is absent; the same check applies to it.
func sessionFrom(r *http.Request, claudeFallback bool) (id string, ok bool) {
	for _, name := range []string{sessionHeader, claudeSessionHeader} {
		vals := r.Header.Values(name)
		if len(vals) == 0 {
			if name == sessionHeader && claudeFallback {
				continue
			}
			return "", true
		}
		if len(vals) != 1 || !validRequestID(vals[0]) {
			return "", false
		}
		return vals[0], true
	}
	return "", true
}

// toolPolicy is a project's tool allow and deny lists.
type toolPolicy struct {
	allow, deny []string
}

func policyOf(p *store.Project) toolPolicy {
	return toolPolicy{allow: p.ToolAllow, deny: p.ToolDeny}
}

func (p toolPolicy) empty() bool { return len(p.allow) == 0 && len(p.deny) == 0 }

// permits reports whether a tool of this name may be offered. Deny always
// wins; an empty allow list permits everything else, and a non-empty one only
// what it matches. Matching is glob.Match: "*" is the only wildcard and case
// matters, so a pattern cannot be made to backtrack.
func (p toolPolicy) permits(name string) bool {
	for _, pat := range p.deny {
		if glob.Match(pat, name) {
			return false
		}
	}
	if len(p.allow) == 0 {
		return true
	}
	for _, pat := range p.allow {
		if glob.Match(pat, name) {
			return true
		}
	}
	return false
}

// toolName is the name a tool entry is matched by: function.name for a
// function tool (OpenAI), custom.name for a custom one, and name for the
// Messages shape. A server tool that carries no name of its own is known by
// its type. An entry that cannot be read has the empty name.
func toolName(raw json.RawMessage) string {
	var t struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Custom struct {
			Name string `json:"name"`
		} `json:"custom"`
	}
	if json.Unmarshal(raw, &t) != nil {
		return ""
	}
	switch {
	case t.Function.Name != "":
		return t.Function.Name
	case t.Custom.Name != "":
		return t.Custom.Name
	case t.Name != "":
		return t.Name
	case t.Type == "function" || t.Type == "custom":
		return ""
	}
	return t.Type
}

// Kinds of tool_choice, as far as governance cares.
const (
	choiceAuto     = "auto"     // absent, "auto", or a shape this build does not read
	choiceNone     = "none"     // "none"
	choiceRequired = "required" // "required" / {"type":"any"}
	choiceNamed    = "named"    // names one tool
)

// toolChoiceOf classifies a tool_choice value (OpenAI or Messages shape). name
// is set for choiceNamed.
func toolChoiceOf(raw json.RawMessage) (kind, name string) {
	if len(raw) == 0 || string(raw) == "null" {
		return choiceAuto, ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "none":
			return choiceNone, ""
		case "required":
			return choiceRequired, ""
		}
		return choiceAuto, ""
	}
	var o struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Custom struct {
			Name string `json:"name"`
		} `json:"custom"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return choiceAuto, ""
	}
	switch o.Type {
	case "none":
		return choiceNone, ""
	case "any", "required":
		return choiceRequired, ""
	case "function":
		return choiceNamed, o.Function.Name
	case "custom":
		return choiceNamed, o.Custom.Name
	case "tool":
		return choiceNamed, o.Name
	}
	return choiceAuto, ""
}

// toolVerdict is what governTools found.
type toolVerdict struct {
	// denied are the names of the tools stripped from the request.
	denied []string
	// warnings are the Warning texts the stripping earns.
	warnings []string
}

// toolError is a request governTools refuses; the client gets a 400. An empty
// code answers without one.
type toolError struct {
	code, message string
}

// narrowAllowedTools reads an allowed_tools tool_choice
// ({"type":"allowed_tools","allowed_tools":{"mode":…,"tools":[…]}}) and removes
// the tools the policy does not permit from its list. ok is false for any
// other tool_choice. mode is "required" or "auto" ("auto" for anything but
// "required"); removed and left count the entries of the list. choice is the
// value to send when something was removed.
func narrowAllowedTools(policy toolPolicy, raw json.RawMessage) (choice json.RawMessage, mode string, removed, left int, ok bool) {
	var outer map[string]json.RawMessage
	if json.Unmarshal(raw, &outer) != nil {
		return nil, "", 0, 0, false
	}
	var typ string
	if json.Unmarshal(outer["type"], &typ) != nil || typ != "allowed_tools" {
		return nil, "", 0, 0, false
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(outer["allowed_tools"], &inner) != nil {
		return nil, "", 0, 0, false
	}
	var entries []json.RawMessage
	if json.Unmarshal(inner["tools"], &entries) != nil {
		return nil, "", 0, 0, false
	}
	_ = json.Unmarshal(inner["mode"], &mode)
	if mode != "required" {
		mode = "auto"
	}
	kept := make([]json.RawMessage, 0, len(entries))
	for _, e := range entries {
		if policy.permits(toolName(e)) {
			kept = append(kept, e)
		}
	}
	removed, left = len(entries)-len(kept), len(kept)
	if removed == 0 {
		return raw, mode, 0, left, true
	}
	list, err := json.Marshal(kept)
	if err != nil {
		return raw, mode, 0, left, true
	}
	inner["tools"] = list
	innerRaw, err := json.Marshal(inner)
	if err != nil {
		return raw, mode, 0, left, true
	}
	outer["allowed_tools"] = innerRaw
	out, err := json.Marshal(outer)
	if err != nil {
		return raw, mode, 0, left, true
	}
	return out, mode, removed, left, true
}

// governTools applies a project's tool policy to a chat request (ADR-032).
//
// Tools the policy does not permit are stripped from req.Tools and reported in
// the verdict. History is not touched: tool_calls and tool messages already in
// the conversation are things that happened. Then tool_choice is reconciled
// with what is left:
//
//   - naming a tool the policy does not permit is refused, as is requiring a
//     tool when stripping left none: turning the demand into something else
//     would change what the request means;
//   - "auto" (or no tool_choice) with no tool left drops tools and tool_choice;
//   - "none" with no tool left behaves like "auto" (a user decision recorded
//     with ADR-032): nothing could be called anyway, and tool_choice without
//     tools is an error at some upstreams;
//   - an allowed_tools choice loses the entries the policy does not permit; if
//     that empties a "required" list the request is refused, and an emptied
//     "auto" list becomes "none" (the client restricted the model to a subset,
//     so it must not gain the other tools), or is dropped with the tools when
//     none are left;
//   - when no tool is left, parallel_tool_calls goes too: upstreams accept it
//     only beside tools.
//
// A policy with no lists, and a request the policy changes nothing in, leave
// req exactly as it was. A refused request is left as it came in.
func governTools(policy toolPolicy, req *provider.ChatRequest) (toolVerdict, *toolError) {
	var v toolVerdict
	if policy.empty() {
		return v, nil
	}
	kind, named := toolChoiceOf(req.ToolChoice)
	if kind == choiceNamed && !policy.permits(named) {
		return v, &toolError{code: "tool_not_allowed",
			message: fmt.Sprintf("tool_choice names the tool %q, which this project does not allow.", cutName(named))}
	}
	choice, choiceChanged := req.ToolChoice, false
	// Narrowing allowed_tools changes the request, so it earns a Warning too.
	var allowedWarn []string
	if nc, mode, removed, left, ok := narrowAllowedTools(policy, req.ToolChoice); ok {
		if mode == "required" {
			kind = choiceRequired
		}
		switch {
		case removed > 0 && left == 0 && mode == "required":
			return v, &toolError{code: "tool_not_allowed",
				message: "tool_choice requires one of the tools in allowed_tools, but this project does not allow any of them."}
		case removed > 0 && left == 0:
			// The client restricted the model to a subset and the policy took the
			// whole subset away. Dropping tool_choice would hand the model every
			// other tool, which the client did not allow; "none" keeps the meaning.
			choice, choiceChanged = json.RawMessage(`"none"`), true
		case removed > 0:
			choice, choiceChanged = nc, true
		}
		if removed > 0 {
			allowedWarn = []string{fmt.Sprintf("%d tool(s) removed from allowed_tools by this project's tool policy", removed)}
		}
	}
	if len(req.Tools) == 0 || string(req.Tools) == "null" {
		if choiceChanged {
			// No tools in the request: a tool_choice would have nothing to govern
			// and is an error at some upstreams.
			req.ToolChoice = choice
			if string(choice) == `"none"` {
				req.ToolChoice = nil
			}
		}
		v.warnings = allowedWarn
		return v, nil
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(req.Tools, &tools); err != nil {
		// Not an array: the upstream would refuse it, but this is the one place
		// the lists can be applied, so it is not passed along unexamined.
		return v, &toolError{message: "tools must be an array."}
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		if name := toolName(t); !policy.permits(name) {
			v.denied = append(v.denied, name)
			continue
		}
		kept = append(kept, t)
	}
	if len(v.denied) == 0 {
		if choiceChanged {
			req.ToolChoice = choice
		}
		v.warnings = allowedWarn
		return v, nil
	}
	if len(kept) == 0 {
		if kind == choiceRequired || kind == choiceNamed {
			return toolVerdict{}, &toolError{code: "tool_not_allowed",
				message: "tool_choice requires a tool, but this project does not allow any of the tools in the request."}
		}
		req.Tools, req.ToolChoice = nil, nil
		delete(req.Extra, "parallel_tool_calls")
		v.warnings = []string{toolWarning(v.denied, true)}
		return v, nil
	}
	b, err := json.Marshal(kept)
	if err != nil {
		// Marshalling raw messages that just unmarshalled does not fail.
		return toolVerdict{}, &toolError{message: "tools could not be read."}
	}
	req.Tools = b
	if choiceChanged {
		req.ToolChoice = choice
	}
	v.warnings = append([]string{toolWarning(v.denied, false)}, allowedWarn...)
	return v, nil
}

// safeToolName is the shape of a tool name that may be echoed in a Warning
// header; any other name is counted without being named.
var safeToolName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// maxWarnedTools bounds the names one Warning lists.
const maxWarnedTools = 5

// toolWarning is the Warning text for stripped tools.
func toolWarning(denied []string, noneLeft bool) string {
	var named []string
	for _, n := range denied {
		if safeToolName.MatchString(n) && len(named) < maxWarnedTools {
			named = append(named, n)
		}
	}
	text := fmt.Sprintf("%d tool(s) removed by this project's tool policy", len(denied))
	if len(named) > 0 {
		text += ": " + strings.Join(named, ", ")
		if len(named) < len(denied) {
			text += ", ..."
		}
	}
	if noneLeft {
		text += "; no tool is left, so tools and tool_choice were dropped"
	}
	return text
}

// cutName bounds a client-chosen name for a message or a log line.
func cutName(s string) string {
	if len(s) <= store.MaxToolNameBytes {
		return s
	}
	n := store.MaxToolNameBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// boundedNames is the denied names as a log line may carry them: each cut,
// at most store.MaxToolNames of them.
func boundedNames(names []string) []string {
	out := make([]string, 0, min(len(names), store.MaxToolNames))
	for _, n := range names {
		if len(out) == store.MaxToolNames {
			break
		}
		out = append(out, cutName(n))
	}
	return out
}

// toolNameSet collects the names of the tools a model called. The names come
// from the model's answer, so the set is bounded; the row clamps them again.
type toolNameSet struct {
	seen map[string]struct{}
}

// maxCollectedTools is how many distinct names a set keeps; more than a row
// holds, so the row's own cut decides which survive.
const maxCollectedTools = 4 * store.MaxToolNames

// add records the names in a tool_calls value: the array of a message, or the
// one a stream delta carries. Only names are read; arguments are ignored.
func (s *toolNameSet) add(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var calls []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &calls) != nil {
		return
	}
	for _, c := range calls {
		name := c.Function.Name
		if name == "" {
			continue
		}
		if s.seen == nil {
			s.seen = map[string]struct{}{}
		}
		if len(s.seen) >= maxCollectedTools {
			return
		}
		s.seen[cutName(name)] = struct{}{}
	}
}

// names returns the collected names, sorted; nil when there are none.
func (s *toolNameSet) names() []string {
	if len(s.seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.seen))
	for n := range s.seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
