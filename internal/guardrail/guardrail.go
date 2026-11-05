// Package guardrail is the rule engine behind a project's guardrails (ADR-034):
// it parses and validates the rules stored in projects.guardrails, compiles
// them, and applies them to text -- to a whole string, or to a stream through a
// sliding window.
//
// The package knows nothing about requests, providers or HTTP. The gateway
// decides which texts of a request to hand over at which stage and what to do
// with the verdict; this package finds the matches and rewrites them.
//
// Nothing here keeps, logs or returns the text that matched. A match is a pair
// of offsets, a redaction a fixed placeholder, and the only things that leave a
// scan are counters and rule ids (PRD rule 10).
package guardrail

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Stages a rule can be bound to.
const (
	StagePreRequest   = "pre_request"
	StagePreRetrieval = "pre_retrieval"
	StagePostResponse = "post_response"
)

// Rule types.
const (
	TypeTCKN     = "tckn"
	TypeIBAN     = "iban"
	TypeRegex    = "regex"
	TypeKeywords = "keywords"
)

// Rule actions.
const (
	ActionBlock  = "block"
	ActionRedact = "redact"
	ActionFlag   = "flag"
)

// Webhook failure modes.
const (
	FailClosed = "fail_closed"
	FailOpen   = "fail_open"
)

// Limits of a configuration.
const (
	MaxRules        = 32
	MaxPatternBytes = 512
	MaxKeywords     = 500
	MaxKeywordBytes = 64

	DefaultWindow = 128
	MinWindow     = 32
	MaxWindow     = 2048
	// maxBuiltinChars is the longest match of tckn or iban: 34 characters of
	// an IBAN and the 8 spaces that group them. SafeBuiltinWindow is the
	// smallest window that holds any of them whole.
	maxBuiltinChars   = 42
	SafeBuiltinWindow = 64

	DefaultWebhookTimeoutMS = 2000
	MinWebhookTimeoutMS     = 100
	MaxWebhookTimeoutMS     = 10000
)

var ruleIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Rule is one guardrail rule. Pattern is the regular expression of a "regex"
// rule and Keywords the word list of a "keywords" rule; the built-in types take
// neither.
type Rule struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Action   string   `json:"action"`
	Stages   []string `json:"stages"`
	Pattern  string   `json:"pattern,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
}

// Webhook is the project's guardrail webhook settings. The endpoint itself
// (URL, secret) lives in webhook_endpoints; this says whether it is consulted
// and how a failure is treated. A nil *Webhook is a project without one.
type Webhook struct {
	Enabled     bool   `json:"enabled"`
	TimeoutMS   int    `json:"timeout_ms,omitempty"`
	FailureMode string `json:"failure_mode,omitempty"`
}

// Config is the content of projects.guardrails.
type Config struct {
	Rules []Rule `json:"rules"`
	// StreamWindowChars is W of the stream scanner; zero is DefaultWindow.
	StreamWindowChars int      `json:"stream_window_chars,omitempty"`
	Webhook           *Webhook `json:"webhook,omitempty"`
}

// ErrInvalid wraps every validation failure. Its message is written for the
// person who typed the configuration and is safe to show (HTTP 400).
var ErrInvalid = errors.New("invalid guardrail configuration")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Parse decodes the stored JSON. Unknown fields are ignored so a replica
// reading what a newer release wrote keeps working; a rule type this build does
// not know is caught by Compile.
func Parse(raw []byte) (Config, error) {
	var c Config
	raw = trimJSON(raw)
	if len(raw) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("guardrail configuration: %w", err)
	}
	return c, nil
}

// trimJSON maps the empty value and JSON null to nothing.
func trimJSON(raw []byte) []byte {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	return []byte(s)
}

// Empty reports whether the configuration does nothing: no rule and no webhook.
func (c Config) Empty() bool {
	return len(c.Rules) == 0 && (c.Webhook == nil || !c.Webhook.Enabled)
}

// Window is W: the configured stream window, or the default.
func (c Config) Window() int {
	if c.StreamWindowChars == 0 {
		return DefaultWindow
	}
	return c.StreamWindowChars
}

// timeoutMS is the configured webhook timeout, or the default.
func (w Webhook) timeoutMS() int {
	if w.TimeoutMS == 0 {
		return DefaultWebhookTimeoutMS
	}
	return w.TimeoutMS
}

// Validate checks a configuration an editor submitted and returns warnings for
// what is allowed but risky. Every error wraps ErrInvalid.
func (c Config) Validate() (warnings []string, err error) {
	if len(c.Rules) > MaxRules {
		return nil, invalidf("at most %d rules are allowed", MaxRules)
	}
	w := c.Window()
	if w < MinWindow || w > MaxWindow {
		return nil, invalidf("stream_window_chars must be between %d and %d", MinWindow, MaxWindow)
	}
	if c.Webhook != nil {
		if m := c.Webhook.FailureMode; m != "" && m != FailClosed && m != FailOpen {
			return nil, invalidf(`webhook.failure_mode must be "fail_closed" or "fail_open"`)
		}
		if t := c.Webhook.timeoutMS(); t < MinWebhookTimeoutMS || t > MaxWebhookTimeoutMS {
			return nil, invalidf("webhook.timeout_ms must be between %d and %d", MinWebhookTimeoutMS, MaxWebhookTimeoutMS)
		}
	}
	seen := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		if !ruleIDPattern.MatchString(r.ID) {
			return nil, invalidf("rule %d: id must match ^[a-z0-9_-]{1,32}$", i+1)
		}
		if seen[r.ID] {
			return nil, invalidf("rule id %q is used twice", r.ID)
		}
		seen[r.ID] = true
		warn, err := r.validate(w)
		if err != nil {
			return nil, invalidf("rule %q: %s", r.ID, err.Error())
		}
		warnings = append(warnings, warn...)
	}
	return warnings, nil
}

// validate checks one rule against the stream window w.
func (r *Rule) validate(w int) (warnings []string, err error) {
	switch r.Action {
	case ActionBlock, ActionRedact, ActionFlag:
	default:
		return nil, errors.New(`action must be "block", "redact" or "flag"`)
	}
	if len(r.Stages) == 0 {
		return nil, errors.New("stages must name at least one stage")
	}
	post := false
	seen := map[string]bool{}
	for _, s := range r.Stages {
		switch s {
		case StagePreRequest, StagePreRetrieval, StagePostResponse:
		default:
			return nil, fmt.Errorf("unknown stage %q", cutText(s))
		}
		if seen[s] {
			return nil, fmt.Errorf("stage %q is listed twice", s)
		}
		seen[s] = true
		post = post || s == StagePostResponse
	}
	switch r.Type {
	case TypeTCKN, TypeIBAN:
		if r.Pattern != "" || len(r.Keywords) > 0 {
			return nil, fmt.Errorf("a %s rule takes no pattern or keywords", r.Type)
		}
		if post && w < SafeBuiltinWindow {
			warnings = append(warnings, fmt.Sprintf(
				"rule %q: a %s match can be up to %d characters long; with stream_window_chars below %d its first characters may be written before the match is complete",
				r.ID, r.Type, maxBuiltinChars, SafeBuiltinWindow))
		}
	case TypeRegex:
		if len(r.Keywords) > 0 {
			return nil, errors.New("a regex rule takes no keywords")
		}
		if len(r.Pattern) == 0 || len(r.Pattern) > MaxPatternBytes {
			return nil, fmt.Errorf("pattern must be 1 to %d bytes", MaxPatternBytes)
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("pattern does not compile: %s", cutText(err.Error()))
		}
		if re.MatchString("") {
			return nil, errors.New("pattern must not match the empty string")
		}
		if post {
			if n, bounded := maxMatchChars(r.Pattern); !bounded || n > w {
				warnings = append(warnings, fmt.Sprintf(
					"rule %q: the pattern can match more than %d characters, so a match may escape the stream window of a post_response stream; raise stream_window_chars or bound the pattern", r.ID, w))
			}
		}
	case TypeKeywords:
		if r.Pattern != "" {
			return nil, errors.New("a keywords rule takes no pattern")
		}
		if len(r.Keywords) == 0 || len(r.Keywords) > MaxKeywords {
			return nil, fmt.Errorf("keywords must list 1 to %d words", MaxKeywords)
		}
		for _, k := range r.Keywords {
			switch {
			case strings.TrimSpace(k) == "" || strings.TrimSpace(k) != k:
				return nil, errors.New("a keyword must not be empty or start or end with a space")
			case len(k) > MaxKeywordBytes:
				return nil, fmt.Errorf("a keyword is at most %d bytes", MaxKeywordBytes)
			case !utf8.ValidString(k):
				return nil, errors.New("a keyword must be valid UTF-8")
			case post && utf8.RuneCountInString(k) > w:
				return nil, fmt.Errorf("a keyword is longer than stream_window_chars (%d) and could not be matched in a stream", w)
			}
		}
	default:
		return nil, fmt.Errorf("unknown type %q", cutText(r.Type))
	}
	return warnings, nil
}

// cutText bounds text an editor typed before it is echoed in a message.
func cutText(s string) string {
	if len(s) <= 64 {
		return s
	}
	n := 64
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// maxMatchChars is the length in characters of the longest string pattern can
// match; bounded is false when it has none (a star or an open repeat).
func maxMatchChars(pattern string) (n int, bounded bool) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return 0, false
	}
	const inf = 1 << 30
	var walk func(*syntax.Regexp) int
	walk = func(re *syntax.Regexp) int {
		sum := func() int {
			t := 0
			for _, s := range re.Sub {
				t += walk(s)
				if t >= inf {
					return inf
				}
			}
			return t
		}
		switch re.Op {
		case syntax.OpLiteral:
			return len(re.Rune)
		case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
			return 1
		case syntax.OpStar, syntax.OpPlus:
			return inf
		case syntax.OpQuest:
			return walk(re.Sub[0])
		case syntax.OpRepeat:
			sub := walk(re.Sub[0])
			if re.Max < 0 || (sub > 0 && re.Max > inf/sub) {
				return inf
			}
			return sub * re.Max
		case syntax.OpConcat:
			return sum()
		case syntax.OpAlternate:
			m := 0
			for _, s := range re.Sub {
				m = max(m, walk(s))
			}
			return m
		case syntax.OpCapture:
			return walk(re.Sub[0])
		}
		return 0 // empty match, anchors, word boundaries
	}
	v := walk(re)
	return v, v < inf
}

// Span is a match: byte offsets into the scanned text, End exclusive.
type Span struct{ Start, End int }

// matcher finds the spans a rule matches in a string, in order, without
// overlaps.
type matcher interface {
	find(s string) []Span
}

type builtinMatcher func(string) []Span

func (f builtinMatcher) find(s string) []Span { return f(s) }

type regexMatcher struct{ re *regexp.Regexp }

func (m regexMatcher) find(s string) []Span {
	idx := m.re.FindAllStringIndex(s, -1)
	out := make([]Span, 0, len(idx))
	for _, p := range idx {
		if p[1] > p[0] { // an empty match hides nothing and would never advance
			out = append(out, Span{p[0], p[1]})
		}
	}
	return out
}

// keywordMatcher matches whole words. The words are one case-insensitive
// alternation, longest first, so RE2 does the scanning in linear time. The word
// boundary is checked here instead of with \b: RE2's \b knows ASCII letters
// only, so a keyword that begins or ends in a Turkish letter ("şifre") would
// never find a boundary and never match.
type keywordMatcher struct {
	re *regexp.Regexp
	// each holds every keyword on its own, anchored at the start and longest
	// first. When the alternation's pick is not a whole word, a shorter keyword
	// that starts at the same place may still be ("kredi" inside "kredi
	// kartim", where "kredi kart" was the pick), and only these can tell.
	each []*regexp.Regexp
}

func (m keywordMatcher) find(s string) []Span {
	var out []Span
	for pos := 0; pos < len(s); {
		loc := m.re.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		start, end := pos+loc[0], pos+loc[1]
		if wordEdge(s, start, end) {
			out = append(out, Span{start, end})
			pos = end
			continue
		}
		if shorter := m.shorterAt(s, start, end); shorter > start {
			out = append(out, Span{start, shorter})
			pos = shorter
			continue
		}
		// Not a whole word. A word that starts inside this candidate is still
		// a candidate, so step one character, not to the end of the match.
		_, w := utf8.DecodeRuneInString(s[start:])
		pos = start + w
	}
	return out
}

// shorterAt looks for a keyword that starts at start, ends before end and is a
// whole word, and returns where it ends, or 0.
func (m keywordMatcher) shorterAt(s string, start, end int) int {
	for _, re := range m.each {
		loc := re.FindStringIndex(s[start:end])
		if loc == nil || start+loc[1] >= end {
			continue
		}
		if wordEdge(s, start, start+loc[1]) {
			return start + loc[1]
		}
	}
	return 0
}

// isWordRune is the character class a keyword must not touch on either side.
func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// wordEdge reports whether s[start:end] is bounded by non-word characters (or
// the ends of s), where the keyword itself begins or ends in a word character.
func wordEdge(s string, start, end int) bool {
	if first, _ := utf8.DecodeRuneInString(s[start:end]); isWordRune(first) && start > 0 {
		if prev, _ := utf8.DecodeLastRuneInString(s[:start]); isWordRune(prev) {
			return false
		}
	}
	if last, _ := utf8.DecodeLastRuneInString(s[start:end]); isWordRune(last) && end < len(s) {
		if next, _ := utf8.DecodeRuneInString(s[end:]); isWordRune(next) {
			return false
		}
	}
	return true
}

func newKeywordMatcher(words []string) (matcher, error) {
	sorted := append([]string(nil), words...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	quoted := make([]string, len(sorted))
	for i, w := range sorted {
		quoted[i] = regexp.QuoteMeta(w)
	}
	re, err := regexp.Compile("(?i)(?:" + strings.Join(quoted, "|") + ")")
	if err != nil {
		return nil, err
	}
	each := make([]*regexp.Regexp, len(quoted))
	for i, q := range quoted {
		if each[i], err = regexp.Compile(`(?i)^` + q); err != nil {
			return nil, err
		}
	}
	return keywordMatcher{re: re, each: each}, nil
}

// compiled is a rule ready to scan.
type compiled struct {
	id     string
	action string
	m      matcher
}

// Set is a compiled configuration: immutable and safe for concurrent use.
type Set struct {
	// byStage holds the rules of each stage, in the order they were declared.
	byStage map[string][]*compiled
	window  int
	webhook Webhook
	// hasWebhook is true when the configuration turns the webhook on.
	hasWebhook bool
}

// Compile validates and compiles a configuration. It is what the gateway runs
// on the stored JSON: a configuration that cannot be compiled is an error, not
// a project without guardrails.
func Compile(c Config) (*Set, error) {
	if _, err := c.Validate(); err != nil {
		return nil, err
	}
	s := &Set{byStage: map[string][]*compiled{}, window: c.Window()}
	if c.Webhook != nil && c.Webhook.Enabled {
		s.hasWebhook, s.webhook = true, *c.Webhook
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		var m matcher
		switch r.Type {
		case TypeTCKN:
			m = builtinMatcher(findTCKN)
		case TypeIBAN:
			m = builtinMatcher(findIBAN)
		case TypeRegex:
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				return nil, invalidf("rule %q: pattern does not compile", r.ID)
			}
			m = regexMatcher{re}
		case TypeKeywords:
			var err error
			if m, err = newKeywordMatcher(r.Keywords); err != nil {
				return nil, invalidf("rule %q: keywords do not compile", r.ID)
			}
		}
		cr := &compiled{id: r.ID, action: r.Action, m: m}
		for _, st := range r.Stages {
			s.byStage[st] = append(s.byStage[st], cr)
		}
	}
	return s, nil
}

// Rewrites reports whether a rule of the stage changes or refuses text (block
// or redact); a stage of flag rules only counts what it sees.
func (s *Set) Rewrites(stage string) bool {
	if s == nil {
		return false
	}
	for _, r := range s.byStage[stage] {
		if r.action == ActionBlock || r.action == ActionRedact {
			return true
		}
	}
	return false
}

// HasStage reports whether any rule is bound to stage.
func (s *Set) HasStage(stage string) bool { return s != nil && len(s.byStage[stage]) > 0 }

// Window is W, the stream window in characters.
func (s *Set) Window() int {
	if s == nil || s.window == 0 {
		return DefaultWindow
	}
	return s.window
}

// WebhookSettings reports whether the webhook is on, its timeout in
// milliseconds and whether a failure lets the request through.
func (s *Set) WebhookSettings() (enabled bool, timeoutMS int, failOpen bool) {
	if s == nil || !s.hasWebhook {
		return false, 0, false
	}
	return true, s.webhook.timeoutMS(), s.webhook.FailureMode == FailOpen
}

// Hit is what one rule found in a text: how many matches, and the action the
// rule would take.
type Hit struct {
	ID     string
	Action string
	Count  int
}

// Report looks at text with every rule of the stage, each on its own and on the
// text as it is, and reports what each found; Count is 0 for a rule that found
// nothing. Nothing is applied, counted or remembered: it is how a text is
// checked without being acted on (the moderation endpoint).
func (s *Set) Report(stage, text string) []Hit {
	if s == nil {
		return nil
	}
	rules := s.byStage[stage]
	out := make([]Hit, 0, len(rules))
	for _, r := range rules {
		out = append(out, Hit{ID: r.id, Action: r.action, Count: len(r.m.find(text))})
	}
	return out
}

// Scan accumulates the verdict over the texts of one stage. It is not safe for
// concurrent use; a request owns its Scans.
type Scan struct {
	rules []*compiled
	// asFlag makes a redact rule count as a flag, for text that must not be
	// rewritten (the signed thinking of a model).
	asFlag bool

	// ident makes a redaction a placeholder that is a legal identifier (see
	// ApplyName).
	ident bool

	redactions int
	flags      int
	blocked    bool
	ids        []string
}

// Scan starts a scan of stage. redactAsFlag turns redact rules into flags.
func (s *Set) Scan(stage string, redactAsFlag bool) *Scan {
	if s == nil {
		return &Scan{}
	}
	return &Scan{rules: s.byStage[stage], asFlag: redactAsFlag}
}

// Active reports whether the scan has any rule to apply.
func (sc *Scan) Active() bool { return len(sc.rules) > 0 }

// Redactions is how many matches were replaced.
func (sc *Scan) Redactions() int { return sc.redactions }

// Flags is how many matches were only counted.
func (sc *Scan) Flags() int { return sc.flags }

// Blocked reports whether a block rule matched.
func (sc *Scan) Blocked() bool { return sc.blocked }

// Rules lists the ids of the rules that matched, each once, in the order they
// first matched.
func (sc *Scan) Rules() []string { return sc.ids }

func (sc *Scan) note(id string) {
	for _, have := range sc.ids {
		if have == id {
			return
		}
	}
	sc.ids = append(sc.ids, id)
}

// Placeholder is what a redacted match is replaced with.
func Placeholder(ruleID string) string { return "[REDACTED:" + ruleID + "]" }

// NamePlaceholder is what a match is replaced with in a name (a tool's, a
// participant's): names are letters, digits, "_" and "-", and a rule id is
// made of exactly those, so "REDACTED-" and the id make a name.
func NamePlaceholder(ruleID string) string { return "REDACTED-" + ruleID }

func (sc *Scan) placeholder(ruleID string) string {
	if sc.ident {
		return NamePlaceholder(ruleID)
	}
	return Placeholder(ruleID)
}

// ApplyName is Apply for a name. Block and flag rules act as they do in Apply;
// a redact rule writes a placeholder that is still a legal name, so that the
// text is hidden and the field stays valid. (A redacted name no longer matches
// the name the client's tool table holds; that is the price of hiding it.)
func (sc *Scan) ApplyName(text string) string {
	sc.ident = true
	defer func() { sc.ident = false }()
	return sc.Apply(text)
}

// Span positions of the placeholders a scan wrote itself are tracked by
// position, never recognised by their text: a client can type
// "[REDACTED:pw]" as easily as a rule can write it, and text that is trusted
// because of what it says is text a client can use to get past a rule.

// outsidePlaceholders drops the matches that lie wholly inside a placeholder
// the scan wrote (ph): the placeholder carries a rule's id, and an id can be a
// word another rule looks for. A match that reaches past a placeholder reads
// text the client wrote and stays.
func outsidePlaceholders(spans []Span, ph []Span) []Span {
	if len(spans) == 0 || len(ph) == 0 {
		return spans
	}
	kept := spans[:0:0]
	for _, sp := range spans {
		inside := false
		for _, p := range ph {
			if sp.Start >= p.Start && sp.End <= p.End {
				inside = true
				break
			}
		}
		if !inside {
			kept = append(kept, sp)
		}
	}
	return kept
}

// replaceSpans rewrites text with ph in place of each span (sorted, without
// overlaps) and moves the tracked placeholders to where they are in the result:
// a tracked placeholder a span overlaps is gone with it, one that comes later
// shifts by what the spans before it changed, and the new placeholders are
// added.
func replaceSpans(text string, spans []Span, placeholder string, tracked []Span) (string, []Span) {
	var b strings.Builder
	var added []Span
	last := 0
	for _, sp := range spans {
		b.WriteString(text[last:sp.Start])
		start := b.Len()
		b.WriteString(placeholder)
		added = append(added, Span{start, b.Len()})
		last = sp.End
	}
	b.WriteString(text[last:])
	moved := make([]Span, 0, len(tracked)+len(added))
	for _, p := range tracked {
		shift, gone := 0, false
		for _, sp := range spans {
			switch {
			case sp.End <= p.Start:
				shift += len(placeholder) - (sp.End - sp.Start)
			case sp.Start < p.End:
				gone = true
			}
		}
		if !gone {
			moved = append(moved, Span{p.Start + shift, p.End + shift})
		}
	}
	return b.String(), append(moved, added...)
}

// shiftSpans moves tracked spans to a text that starts cut bytes later; a span
// that is not wholly after cut is dropped.
func shiftSpans(ph []Span, cut int) []Span {
	var out []Span
	for _, p := range ph {
		if p.Start >= cut {
			out = append(out, Span{p.Start - cut, p.End - cut})
		}
	}
	return out
}

// Probe looks at text with the block rules only and records a match as a
// block. It changes nothing and counts nothing else: it is for a second
// reading of a text that has been read already in pieces (the JSON of tool call
// arguments, whose keys and values are scanned one by one), so that a pattern
// that spans them can still stop the request.
func (sc *Scan) Probe(text string) {
	for _, r := range sc.rules {
		if r.action != ActionBlock {
			continue
		}
		if len(r.m.find(text)) > 0 {
			sc.blocked = true
			sc.note(r.id)
		}
	}
}

// Apply scans text with every rule of the stage and returns it with the redact
// matches replaced. Block rules are looked at first, then redact rules in the
// order they were declared, then flag rules on what is left; a match counts
// once however many times Apply sees the same rule.
func (sc *Scan) Apply(text string) string {
	out, _, flagSpans, _ := sc.apply(text, nil, true)
	sc.flags += len(flagSpans)
	return out
}

// apply is Apply for a text that may be the front of a longer one. When final
// is false a match that touches the end of text is provisional: the text that
// follows could still change it (a digit that extends a run, a letter that
// extends a word), so it is neither acted on nor counted, and hold is the
// number of bytes at the end of the returned text that belong to such a match.
// flagSpans are the flag matches of the returned text, in its coordinates.
func (sc *Scan) apply(text string, ph []Span, final bool) (out string, outPH []Span, flagSpans []Span, hold int) {
	out = text
	if len(sc.rules) == 0 || text == "" {
		return out, ph, nil, 0
	}
	settle := func(spans []Span, cur string) (done []Span) {
		spans = outsidePlaceholders(spans, ph)
		for _, sp := range spans {
			if !final && sp.End == len(cur) {
				hold = max(hold, len(cur)-sp.Start)
				continue
			}
			done = append(done, sp)
		}
		return done
	}
	for _, r := range sc.rules {
		if r.action != ActionBlock {
			continue
		}
		if spans := settle(r.m.find(out), out); len(spans) > 0 {
			sc.blocked = true
			sc.note(r.id)
		}
	}
	for _, r := range sc.rules {
		if r.action != ActionRedact || sc.asFlag {
			continue
		}
		spans := settle(r.m.find(out), out)
		if len(spans) == 0 {
			continue
		}
		sc.redactions += len(spans)
		sc.note(r.id)
		// The provisional tail is measured from the end, so it survives the
		// rewrite of what lies before it.
		out, ph = replaceSpans(out, spans, sc.placeholder(r.id), ph)
	}
	for _, r := range sc.rules {
		if r.action == ActionBlock || (r.action == ActionRedact && !sc.asFlag) {
			continue
		}
		spans := settle(r.m.find(out), out)
		if len(spans) == 0 {
			continue
		}
		flagSpans = append(flagSpans, spans...)
		sc.note(r.id)
	}
	sort.Slice(flagSpans, func(i, j int) bool { return flagSpans[i].Start < flagSpans[j].Start })
	return out, ph, flagSpans, min(hold, len(out))
}

// Window scans a stream of text deltas and hands back the part that is safe to
// write. It keeps the last W-1 characters (and any match that is still being
// completed) back until the text after them has been seen, so a match split
// across deltas is found before any of it is written. W is at least 32, and the
// built-in detectors match at most 42 characters, so the default window covers
// them.
//
// A Window is not safe for concurrent use.
type Window struct {
	sc  *Scan
	w   int
	buf string
	// ph are the placeholders the scan wrote into buf, by position.
	ph      []Span
	blocked bool
}

// Window starts a stream scan over sc with a window of w characters.
func (sc *Scan) Window(w int) *Window {
	return &Window{sc: sc, w: max(w, 1)}
}

// Blocked reports whether a block rule matched; once it has, Push and Close
// return nothing.
func (win *Window) Blocked() bool { return win.blocked }

// Push adds a delta and returns the text that may be written now.
func (win *Window) Push(delta string) string {
	if win.blocked {
		return ""
	}
	win.buf += delta
	return win.step(false)
}

// Close ends the stream and returns everything that was held back.
func (win *Window) Close() string {
	if win.blocked {
		return ""
	}
	return win.step(true)
}

// maxHold bounds how long a provisional match may keep text back, in windows,
// so a pattern that matches ever longer runs cannot grow the buffer without
// limit.
const maxHold = 4

func (win *Window) step(final bool) string {
	blockedBefore := win.sc.blocked
	text, ph, flagSpans, hold := win.sc.apply(win.buf, win.ph, final)
	if win.sc.blocked && !blockedBefore {
		win.blocked, win.buf, win.ph = true, "", nil
		return ""
	}
	if final {
		win.sc.flags += len(flagSpans)
		win.buf, win.ph = "", nil
		return text
	}
	cut := len(text)
	// Keep the last w-1 characters.
	for kept := 0; cut > 0 && kept < win.w-1; kept++ {
		_, n := utf8.DecodeLastRuneInString(text[:cut])
		cut -= n
	}
	// A provisional match stays whole, unless it has grown past what the
	// window could ever have caught.
	if hold > 0 && hold <= maxHold*win.w*utf8.UTFMax {
		cut = min(cut, len(text)-hold)
	}
	// A placeholder is written whole or kept whole.
	for _, p := range ph {
		if p.Start < cut && p.End > cut {
			cut = p.End
		}
	}
	// A flag match that straddles the cut is counted now and let through whole:
	// flagged text is not hidden, and leaving half of it behind would count it
	// again.
	for _, sp := range flagSpans {
		if sp.Start < cut && sp.End > cut && sp.End <= len(text)-hold {
			cut = sp.End
		}
	}
	for _, sp := range flagSpans {
		if sp.Start < cut {
			win.sc.flags++
		}
	}
	win.buf, win.ph = text[cut:], shiftSpans(ph, cut)
	return text[:cut]
}

// Cache compiles the configuration of each project once. A request carries the
// project's raw JSON, so the cache compares the bytes and recompiles only when
// the stored value changed.
type Cache struct {
	mu sync.Mutex
	m  map[int64]cacheEntry
}

type cacheEntry struct {
	raw string
	set *Set
	err error
}

// For returns the compiled configuration of a project: nil when it has none or
// it does nothing, an error when the stored value cannot be used.
func (c *Cache) For(projectID int64, raw []byte) (*Set, error) {
	raw = trimJSON(raw)
	if raw == nil {
		return nil, nil
	}
	key := string(raw)
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[projectID]; ok && e.raw == key {
		return e.set, e.err
	}
	var set *Set
	cfg, err := Parse(raw)
	if err == nil && !cfg.Empty() {
		set, err = Compile(cfg)
	}
	if c.m == nil {
		c.m = map[int64]cacheEntry{}
	}
	c.m[projectID] = cacheEntry{raw: key, set: set, err: err}
	return set, err
}

// PlaceholderWarnings lists the redact rules whose placeholder a block rule
// matches. The placeholder of an answer comes back in the history of the next
// request, and a block rule that reads "[REDACTED:iban]" as a match stops that
// conversation there. The warning is for the person who writes the rules.
func (s *Set) PlaceholderWarnings() []string {
	if s == nil {
		return nil
	}
	seen := map[*compiled]bool{}
	var all []*compiled
	for _, rules := range s.byStage {
		for _, r := range rules {
			if !seen[r] {
				seen[r] = true
				all = append(all, r)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
	var out []string
	for _, r := range all {
		if r.action != ActionRedact {
			continue
		}
		for _, q := range all {
			if q.action != ActionBlock || q == r {
				continue
			}
			if len(q.m.find(Placeholder(r.id))) > 0 || len(q.m.find(NamePlaceholder(r.id))) > 0 {
				out = append(out, fmt.Sprintf("block rule %q matches the placeholder of redact rule %q; a client that sends an answer back in its history will be refused", q.id, r.id))
			}
		}
	}
	return out
}
