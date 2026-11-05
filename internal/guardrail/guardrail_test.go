package guardrail

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

const (
	tckn = "10000000146"
	iban = "TR33 0006 1005 1978 6457 8413 26"
)

var allStages = []string{StagePreRequest, StagePreRetrieval, StagePostResponse}

func rule(id, typ, action string, stages ...string) Rule {
	if len(stages) == 0 {
		stages = []string{StagePreRequest}
	}
	return Rule{ID: id, Type: typ, Action: action, Stages: stages}
}

func compile(t *testing.T, rules ...Rule) *Set {
	t.Helper()
	s, err := Compile(Config{Rules: rules})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return s
}

func TestValidate(t *testing.T) {
	ok := func(r Rule) Config { return Config{Rules: []Rule{r}} }
	regex := func(p string) Rule { r := rule("r", TypeRegex, ActionFlag); r.Pattern = p; return r }
	words := func(w ...string) Rule { r := rule("k", TypeKeywords, ActionFlag); r.Keywords = w; return r }

	many := Config{}
	for i := 0; i <= MaxRules; i++ {
		many.Rules = append(many.Rules, rule(fmt.Sprintf("r%d", i), TypeTCKN, ActionFlag))
	}
	manyWords := make([]string, MaxKeywords+1)
	for i := range manyWords {
		manyWords[i] = fmt.Sprintf("w%d", i)
	}

	bad := map[string]Config{
		"too many rules":            many,
		"empty id":                  ok(rule("", TypeTCKN, ActionFlag)),
		"upper-case id":             ok(rule("Tckn", TypeTCKN, ActionFlag)),
		"long id":                   ok(rule(strings.Repeat("a", 33), TypeTCKN, ActionFlag)),
		"duplicate id":              {Rules: []Rule{rule("a", TypeTCKN, ActionFlag), rule("a", TypeIBAN, ActionFlag)}},
		"unknown action":            ok(rule("a", TypeTCKN, "delete")),
		"unknown type":              ok(rule("a", "email", ActionFlag)),
		"no stages":                 ok(Rule{ID: "a", Type: TypeTCKN, Action: ActionFlag}),
		"unknown stage":             ok(rule("a", TypeTCKN, ActionFlag, "later")),
		"stage twice":               ok(rule("a", TypeTCKN, ActionFlag, StagePreRequest, StagePreRequest)),
		"regex without pattern":     ok(regex("")),
		"regex that does not parse": ok(regex("(")),
		"regex too long":            ok(regex(strings.Repeat("a", MaxPatternBytes+1))),
		"regex matching nothing":    ok(regex("a*")),
		"pattern on tckn":           ok(Rule{ID: "a", Type: TypeTCKN, Action: ActionFlag, Stages: allStages[:1], Pattern: "x"}),
		"keywords on regex":         ok(func() Rule { r := regex("x"); r.Keywords = []string{"y"}; return r }()),
		"keywords without words":    ok(words()),
		"too many keywords":         ok(words(manyWords...)),
		"keyword too long":          ok(words(strings.Repeat("a", MaxKeywordBytes+1))),
		"blank keyword":             ok(words(" ")),
		"keyword with a margin":     ok(words(" word")),
		"window too small":          {StreamWindowChars: 31},
		"window too large":          {StreamWindowChars: 2049},
		"timeout too small":         {Webhook: &Webhook{Enabled: true, TimeoutMS: 99}},
		"timeout too large":         {Webhook: &Webhook{Enabled: true, TimeoutMS: 10001}},
		"unknown failure mode":      {Webhook: &Webhook{Enabled: true, FailureMode: "shrug"}},
		"keyword longer than W": {StreamWindowChars: 32, Rules: []Rule{func() Rule {
			r := words(strings.Repeat("a", 33))
			r.Stages = []string{StagePostResponse}
			return r
		}()}},
	}
	for name, c := range bad {
		_, err := c.Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want ErrInvalid", name, err)
		}
	}
	// A long keyword is fine where nothing streams.
	if _, err := ok(words(strings.Repeat("a", 60))).Validate(); err != nil {
		t.Errorf("a 60-byte keyword on pre_request: %v", err)
	}
	for _, c := range []Config{
		{},
		{StreamWindowChars: MinWindow}, {StreamWindowChars: MaxWindow},
		{Webhook: &Webhook{Enabled: true, TimeoutMS: 100, FailureMode: FailOpen}},
		ok(regex(`\d{3}-\d{3}`)), ok(words("secret", "şifre")),
	} {
		if _, err := c.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", c, err)
		}
	}
}

func TestValidateWarnsAboutPatternsThatCanEscapeTheWindow(t *testing.T) {
	post := func(p string, w int) Config {
		r := rule("r", TypeRegex, ActionRedact, StagePostResponse)
		r.Pattern = p
		return Config{StreamWindowChars: w, Rules: []Rule{r}}
	}
	for name, tc := range map[string]struct {
		cfg  Config
		warn bool
	}{
		"unbounded plus":          {post(`secret-\w+`, 128), true},
		"unbounded star":          {post(`a.*b`, 128), true},
		"open repeat":             {post(`a{3,}`, 128), true},
		"bounded but long":        {post(`[a-z]{100}x{100}`, 128), true},
		"bounded and short":       {post(`\d{4}-\d{4}`, 128), false},
		"alternation of literals": {post(`foo|barbaz`, 128), false},
		"bounded and exactly W":   {post(strings.Repeat("a", 40), 40), false},
		"one over W":              {post(strings.Repeat("a", 41), 40), true},
	} {
		w, err := tc.cfg.Validate()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := len(w) > 0; got != tc.warn {
			t.Errorf("%s: warnings = %v, want a warning: %v", name, w, tc.warn)
		}
	}
	// The same pattern before the model is called streams nothing.
	r := rule("r", TypeRegex, ActionRedact, StagePreRequest)
	r.Pattern = `a+`
	if w, err := (Config{Rules: []Rule{r}}).Validate(); err != nil || len(w) != 0 {
		t.Errorf("pre_request regex: warnings %v, err %v", w, err)
	}
	// A small window and a built-in detector that streams.
	small := Config{StreamWindowChars: 32, Rules: []Rule{rule("t", TypeTCKN, ActionRedact, StagePostResponse)}}
	if w, _ := small.Validate(); len(w) != 1 {
		t.Errorf("tckn with W=32: warnings %v", w)
	}
	safe := Config{StreamWindowChars: SafeBuiltinWindow, Rules: small.Rules}
	if w, _ := safe.Validate(); len(w) != 0 {
		t.Errorf("tckn with W=64: warnings %v", w)
	}
}

func TestParse(t *testing.T) {
	for _, raw := range []string{"", "  ", "null"} {
		c, err := Parse([]byte(raw))
		if err != nil || !c.Empty() {
			t.Errorf("Parse(%q) = %+v, %v", raw, c, err)
		}
	}
	c, err := Parse([]byte(`{"rules":[{"id":"a","type":"tckn","action":"block","stages":["pre_request"],"future":1}],"future":true}`))
	if err != nil || len(c.Rules) != 1 || c.Empty() {
		t.Errorf("Parse = %+v, %v", c, err)
	}
	if _, err := Parse([]byte(`{"rules":`)); err == nil {
		t.Error("a truncated value must not parse")
	}
	if !(Config{Webhook: &Webhook{Enabled: false}}).Empty() || (Config{Webhook: &Webhook{Enabled: true}}).Empty() {
		t.Error("Empty must follow the webhook switch")
	}
}

func TestApplyActions(t *testing.T) {
	text := "tc " + tckn + " iban " + iban + " end"
	s := compile(t,
		rule("flag-tckn", TypeTCKN, ActionFlag),
		rule("red-iban", TypeIBAN, ActionRedact),
	)
	sc := s.Scan(StagePreRequest, false)
	got := sc.Apply(text)
	if want := "tc " + tckn + " iban [REDACTED:red-iban] end"; got != want {
		t.Errorf("Apply = %q, want %q", got, want)
	}
	if sc.Redactions() != 1 || sc.Flags() != 1 || sc.Blocked() || strings.Join(sc.Rules(), ",") != "red-iban,flag-tckn" {
		t.Errorf("scan = %d redactions, %d flags, blocked %v, rules %v", sc.Redactions(), sc.Flags(), sc.Blocked(), sc.Rules())
	}

	b := compile(t, rule("stop", TypeTCKN, ActionBlock))
	bs := b.Scan(StagePreRequest, false)
	if bs.Apply("nothing here"); bs.Blocked() {
		t.Error("blocked without a match")
	}
	if got := bs.Apply(text); got != text || !bs.Blocked() {
		t.Errorf("block must leave the text alone and set Blocked, got %q blocked=%v", got, bs.Blocked())
	}
}

func TestApplyAccumulatesAcrossTexts(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionRedact), rule("f", TypeIBAN, ActionFlag))
	sc := s.Scan(StagePreRequest, false)
	sc.Apply(tckn)
	sc.Apply("a " + tckn + " b " + tckn)
	sc.Apply(iban)
	sc.Apply(iban + " " + iban)
	if sc.Redactions() != 3 || sc.Flags() != 3 {
		t.Errorf("redactions %d flags %d, want 3 and 3", sc.Redactions(), sc.Flags())
	}
	if len(sc.Rules()) != 2 {
		t.Errorf("rule ids = %v, each must be listed once", sc.Rules())
	}
}

func TestRedactAsFlagLeavesTheTextAlone(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionRedact, StagePostResponse))
	sc := s.Scan(StagePostResponse, true)
	if got := sc.Apply("x " + tckn); got != "x "+tckn {
		t.Errorf("text changed: %q", got)
	}
	if sc.Redactions() != 0 || sc.Flags() != 1 {
		t.Errorf("redactions %d flags %d, want 0 and 1", sc.Redactions(), sc.Flags())
	}
}

func TestStagesAreIndependent(t *testing.T) {
	s := compile(t, rule("a", TypeTCKN, ActionBlock, StagePreRequest), rule("b", TypeTCKN, ActionFlag, StagePostResponse))
	if !s.HasStage(StagePreRequest) || s.HasStage(StagePreRetrieval) || !s.HasStage(StagePostResponse) {
		t.Error("HasStage")
	}
	if sc := s.Scan(StagePreRetrieval, false); sc.Active() {
		t.Error("a stage without rules must be inactive")
	}
	sc := s.Scan(StagePostResponse, false)
	sc.Apply(tckn)
	if sc.Blocked() || sc.Flags() != 1 {
		t.Error("the pre_request block rule must not run at post_response")
	}
	var nilSet *Set
	if nilSet.HasStage(StagePreRequest) || nilSet.Scan(StagePreRequest, false).Active() || nilSet.Window() != DefaultWindow {
		t.Error("a nil Set must be inert")
	}
}

func TestRegexRule(t *testing.T) {
	r := rule("card", TypeRegex, ActionRedact)
	r.Pattern = `\b\d{4}-\d{4}\b`
	sc := compile(t, r).Scan(StagePreRequest, false)
	if got := sc.Apply("pay 1234-5678 or 8765-4321."); got != "pay [REDACTED:card] or [REDACTED:card]." {
		t.Errorf("got %q", got)
	}
}

func TestKeywords(t *testing.T) {
	r := rule("words", TypeKeywords, ActionFlag)
	r.Keywords = []string{"password", "pass", "şifre", "ışık", "credit card", "card", "a.b"}
	s := compile(t, r)
	count := func(text string) int {
		sc := s.Scan(StagePreRequest, false)
		sc.Apply(text)
		return sc.Flags()
	}
	for text, want := range map[string]int{
		"my password is here":      1,
		"my PASSWORD is here":      1,
		"passwords":                0, // a longer word
		"bypass":                   0,
		"pass, password; pass.":    3,
		"şifre":                    1,
		"ŞİFRE nedir? şifre.":      1, // simple case folding does not map İ to i; the lower-case one matches
		"şifreler":                 0,
		"ışık yandı":               1,
		"çışık":                    0, // a Turkish letter before is a word character
		"my credit card number":    1,
		"xcredit card":             1, // "card" stands alone even though "credit card" does not
		"discard":                  0,
		"a.b and a.b":              2,
		"aXb":                      0, // keywords are literals, not patterns
		"password_2":               0,
		"":                         0,
		"password":                 1,
		"password password":        2,
		"(password)":               1,
		"the word 'password' here": 1,
	} {
		if got := count(text); got != want {
			t.Errorf("keywords in %q: %d, want %d", text, got, want)
		}
	}
}

func TestKeywordsRedactKeepsTheRest(t *testing.T) {
	r := rule("w", TypeKeywords, ActionRedact)
	r.Keywords = []string{"secret"}
	sc := compile(t, r).Scan(StagePreRequest, false)
	if got := sc.Apply("a Secret and a secret."); got != "a [REDACTED:w] and a [REDACTED:w]." {
		t.Errorf("got %q", got)
	}
}

// ---------------------------------------------------------------- window

// stream pushes text through a window in pieces of n characters and returns
// what was written, with the verdict.
func stream(t *testing.T, s *Set, pieces []string, w int) (written string, sc *Scan, win *Window) {
	t.Helper()
	sc = s.Scan(StagePostResponse, false)
	win = sc.Window(w)
	var b strings.Builder
	for _, p := range pieces {
		b.WriteString(win.Push(p))
	}
	b.WriteString(win.Close())
	return b.String(), sc, win
}

func chunks(text string, n int) []string {
	rs := []rune(text)
	var out []string
	for i := 0; i < len(rs); i += n {
		out = append(out, string(rs[i:min(i+n, len(rs))]))
	}
	return out
}

func TestWindowRedactsAMatchSplitAtEveryPosition(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionRedact, StagePostResponse), rule("i", TypeIBAN, ActionRedact, StagePostResponse))
	for _, tc := range []struct{ name, secret, want string }{
		{"tckn", tckn, "[REDACTED:t]"},
		{"iban", iban, "[REDACTED:i]"},
	} {
		text := "before text " + tc.secret + " and after text"
		want := "before text " + tc.want + " and after text"
		for _, size := range []int{1, 2, 3, 5, 7, 11, 40, 1000} {
			for _, w := range []int{32, 64, 128} {
				got, sc, _ := stream(t, s, chunks(text, size), w)
				if got != want {
					t.Errorf("%s, delta %d, window %d: %q, want %q", tc.name, size, w, got, want)
				}
				if sc.Redactions() != 1 {
					t.Errorf("%s, delta %d, window %d: %d redactions", tc.name, size, w, sc.Redactions())
				}
			}
		}
	}
}

func TestWindowNeverWritesAPartOfAMatch(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionRedact, StagePostResponse), rule("i", TypeIBAN, ActionRedact, StagePostResponse))
	// Whatever the cut, no prefix of the output ever holds the secret or the
	// start of one that was completed later: check each prefix written so far.
	for _, secret := range []string{tckn, iban} {
		text := strings.Repeat("x", 200) + " " + secret + " " + strings.Repeat("y", 200)
		sc := s.Scan(StagePostResponse, false)
		win := sc.Window(64)
		var written strings.Builder
		for _, p := range chunks(text, 1) {
			written.WriteString(win.Push(p))
			if strings.Contains(written.String(), secret) {
				t.Fatalf("the secret was written whole after %d bytes", written.Len())
			}
			// The first digits of the match must not have escaped either.
			if i := strings.Index(text, secret); written.Len() > i && strings.HasSuffix(written.String(), secret[:2]) {
				t.Fatalf("the beginning of the secret was written: %q", written.String()[written.Len()-8:])
			}
		}
		written.WriteString(win.Close())
		if strings.Contains(written.String(), secret) {
			t.Fatal("the secret reached the output")
		}
	}
}

func TestWindowBlocksBeforeAnythingOfTheMatchIsWritten(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionBlock, StagePostResponse))
	text := strings.Repeat("a", 300) + " " + tckn + " tail"
	for _, size := range []int{1, 3, 50} {
		got, sc, win := stream(t, s, chunks(text, size), 64)
		if !sc.Blocked() || !win.Blocked() {
			t.Fatalf("delta %d: not blocked", size)
		}
		if strings.Contains(got, tckn[:3]) || strings.Contains(got, "tail") || len(got) > 300 {
			t.Errorf("delta %d: wrote %d bytes past the block: %q", size, len(got), got[max(0, len(got)-20):])
		}
		if win.Push("more") != "" || win.Close() != "" {
			t.Errorf("delta %d: a blocked window must stay silent", size)
		}
	}
}

func TestWindowBlockedAtTheVeryEndIsStillBlocked(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionBlock, StagePostResponse))
	got, sc, _ := stream(t, s, chunks("short "+tckn, 4), 64)
	if !sc.Blocked() || got != "" {
		t.Errorf("blocked=%v written=%q: a short answer is held whole until Close and must not leak", sc.Blocked(), got)
	}
}

func TestWindowDoesNotBlockWhenALongerNumberFollows(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionBlock, StagePostResponse))
	// Eleven digits that would be a TCKN, then more digits: not an identity
	// number, and the stream must not be cut because the first eleven looked
	// like one for a moment.
	text := "see " + tckn + "123 for details"
	for _, size := range []int{1, 2, 13} {
		got, sc, _ := stream(t, s, chunks(text, size), 32)
		if sc.Blocked() || got != text {
			t.Errorf("delta %d: blocked=%v got %q", size, sc.Blocked(), got)
		}
	}
}

func TestWindowWithoutAMatchWritesEverythingInOrder(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionRedact, StagePostResponse))
	text := strings.Repeat("Merhaba dünya, şöyle güzel bir gün! ", 40)
	for _, size := range []int{1, 4, 100} {
		got, sc, _ := stream(t, s, chunks(text, size), 32)
		if got != text || sc.Redactions() != 0 || sc.Flags() != 0 {
			t.Errorf("delta %d: output differs or counters moved (%d, %d)", size, sc.Redactions(), sc.Flags())
		}
	}
}

func TestWindowKeepsMultiByteCharactersWhole(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionRedact, StagePostResponse))
	text := strings.Repeat("ğüşiöçİ", 100) + " " + tckn + " " + strings.Repeat("日本語", 50)
	var written strings.Builder
	sc := s.Scan(StagePostResponse, false)
	win := sc.Window(32)
	for _, p := range chunks(text, 1) {
		out := win.Push(p)
		if out != "" && !isValidUTF8(out) {
			t.Fatalf("a delta of the output is not valid UTF-8: %q", out)
		}
		written.WriteString(out)
	}
	written.WriteString(win.Close())
	if want := strings.Replace(text, tckn, "[REDACTED:t]", 1); written.String() != want {
		t.Errorf("output differs from %q", want[:40])
	}
}

func isValidUTF8(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestWindowCountsAFlagOnce(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionFlag, StagePostResponse))
	text := strings.Repeat("a", 50) + " " + tckn + " " + strings.Repeat("b", 400) + " " + tckn
	for _, size := range []int{1, 7, 40, 5000} {
		for _, w := range []int{32, 128} {
			got, sc, _ := stream(t, s, chunks(text, size), w)
			if got != text {
				t.Errorf("delta %d window %d: a flag must not change the text", size, w)
			}
			if sc.Flags() != 2 {
				t.Errorf("delta %d window %d: %d flags, want 2", size, w, sc.Flags())
			}
		}
	}
}

func TestWindowKeywordsAcrossDeltas(t *testing.T) {
	r := rule("w", TypeKeywords, ActionRedact, StagePostResponse)
	r.Keywords = []string{"gizli proje"}
	s := compile(t, r)
	text := "Bu çok gizli proje hakkında. " + strings.Repeat(".", 100) + " gizli proje"
	for _, size := range []int{1, 2, 5, 33} {
		got, sc, _ := stream(t, s, chunks(text, size), 32)
		if strings.Contains(got, "gizli") || sc.Redactions() != 2 {
			t.Errorf("delta %d: %q (%d redactions)", size, got, sc.Redactions())
		}
	}
}

func TestWindowCloseWithNothingPushed(t *testing.T) {
	s := compile(t, rule("t", TypeTCKN, ActionBlock, StagePostResponse))
	win := s.Scan(StagePostResponse, false).Window(64)
	if win.Close() != "" || win.Blocked() {
		t.Error("an empty stream must end quietly")
	}
}

func TestWindowBoundsWhatAnEndlessMatchCanHold(t *testing.T) {
	r := rule("run", TypeRegex, ActionFlag, StagePostResponse)
	r.Pattern = `x+`
	s := compile(t, r)
	sc := s.Scan(StagePostResponse, false)
	win := sc.Window(32)
	var out strings.Builder
	for i := 0; i < 5000; i++ {
		out.WriteString(win.Push("x"))
	}
	if out.Len() < 4000 {
		t.Errorf("only %d of 5000 characters were written while the stream ran: the buffer grows without bound", out.Len())
	}
	out.WriteString(win.Close())
	if out.Len() != 5000 {
		t.Errorf("wrote %d characters, want 5000", out.Len())
	}
}

func TestCache(t *testing.T) {
	var c Cache
	if s, err := c.For(1, nil); s != nil || err != nil {
		t.Errorf("no configuration: %v, %v", s, err)
	}
	if s, err := c.For(1, []byte(`{"rules":[]}`)); s != nil || err != nil {
		t.Errorf("an empty configuration does nothing: %v, %v", s, err)
	}
	raw := []byte(`{"rules":[{"id":"a","type":"tckn","action":"flag","stages":["pre_request"]}]}`)
	s1, err := c.For(7, raw)
	if err != nil || s1 == nil {
		t.Fatalf("For = %v, %v", s1, err)
	}
	if s2, _ := c.For(7, raw); s2 != s1 {
		t.Error("an unchanged value must reuse the compiled set")
	}
	other := []byte(`{"rules":[{"id":"b","type":"iban","action":"flag","stages":["pre_request"]}]}`)
	if s3, _ := c.For(7, other); s3 == s1 || s3 == nil {
		t.Error("a changed value must be compiled again")
	}
	if s4, _ := c.For(8, raw); s4 == s1 {
		t.Error("projects must not share a set")
	}
	bad := []byte(`{"rules":[{"id":"a","type":"email","action":"flag","stages":["pre_request"]}]}`)
	for i := 0; i < 2; i++ {
		if s, err := c.For(9, bad); s != nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("an unusable value must be an error every time, got %v, %v", s, err)
		}
	}
}

func TestWebhookSettings(t *testing.T) {
	var nilSet *Set
	if on, _, _ := nilSet.WebhookSettings(); on {
		t.Error("nil set has no webhook")
	}
	s, err := Compile(Config{Webhook: &Webhook{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if on, ms, open := s.WebhookSettings(); !on || ms != DefaultWebhookTimeoutMS || open {
		t.Errorf("defaults: %v %d %v", on, ms, open)
	}
	s, _ = Compile(Config{Webhook: &Webhook{Enabled: true, TimeoutMS: 500, FailureMode: FailOpen}})
	if on, ms, open := s.WebhookSettings(); !on || ms != 500 || !open {
		t.Errorf("explicit: %v %d %v", on, ms, open)
	}
	s, _ = Compile(Config{Webhook: &Webhook{Enabled: false}, Rules: []Rule{rule("a", TypeTCKN, ActionFlag)}})
	if on, _, _ := s.WebhookSettings(); on {
		t.Error("a disabled webhook must read as off")
	}
}

// A longer keyword that is not a whole word must not hide a shorter one that is.
func TestKeywordsShorterWordBehindALongerCandidate(t *testing.T) {
	r := rule("k", TypeKeywords, ActionBlock, allStages...)
	r.Keywords = []string{"kredi", "kredi karti"}
	s := compile(t, r)
	for text, want := range map[string]bool{
		"kredi":              true,
		"kredi karti":        true,
		"kredi kart":         true,
		"kredi kartim var":   true, // "kredi karti" is not a word here; "kredi" is
		"kredi kartin var":   true,
		"kredim":             false,
		"akredi":             false,
		"kredi kartimiz":     true,
		"kreditor kredikart": false,
	} {
		sc := s.Scan(StagePreRequest, false)
		sc.Apply(text)
		if sc.Blocked() != want {
			t.Errorf("%q: blocked = %v, want %v", text, sc.Blocked(), want)
		}
	}
	// And the span of the shorter word is the one that is redacted.
	r.Action = ActionRedact
	sc := compile(t, r).Scan(StagePreRequest, false)
	if got := sc.Apply("kredi kartim var"); got != "[REDACTED:k] kartim var" {
		t.Errorf("redacted = %q", got)
	}
	// The same through a window, in one-character deltas.
	written, sc2, _ := stream(t, compile(t, r), chunks("kredi kartim var", 1), 32)
	if written != "[REDACTED:k] kartim var" || sc2.Redactions() != 1 {
		t.Errorf("stream: %q, %d redactions", written, sc2.Redactions())
	}
}

// What a redact rule wrote is not judged again: not by the next rule, not by
// the next step of a window.
func TestPlaceholdersAreNotScannedAgain(t *testing.T) {
	id := rule("iban", TypeTCKN, ActionRedact, allStages...) // the rule's id is also a word another rule blocks
	word := rule("w", TypeKeywords, ActionBlock, allStages...)
	word.Keywords = []string{"iban"}
	s := compile(t, id, word)

	sc := s.Scan(StagePreRequest, false)
	if got := sc.Apply("id " + tckn + " ok"); got != "id [REDACTED:iban] ok" || sc.Blocked() {
		t.Errorf("whole text: %q blocked=%v", got, sc.Blocked())
	}
	written, sc2, _ := stream(t, s, chunks("id "+tckn+" ok", 1), 32)
	if written != "id [REDACTED:iban] ok" || sc2.Blocked() {
		t.Errorf("stream: %q blocked=%v", written, sc2.Blocked())
	}
	// Two redact rules: the second one does not see the first one's placeholder.
	a, b := rule("a", TypeTCKN, ActionRedact, allStages...), rule("b", TypeKeywords, ActionRedact, allStages...)
	b.Keywords = []string{"a"}
	sc3 := compile(t, a, b).Scan(StagePreRequest, false)
	if got := sc3.Apply("x " + tckn + " a"); got != "x [REDACTED:a] [REDACTED:b]" || sc3.Redactions() != 2 {
		t.Errorf("two rules: %q, %d redactions", got, sc3.Redactions())
	}
	// Text the client wrote around a placeholder-looking token is still read.
	sc4 := s.Scan(StagePreRequest, false)
	sc4.Apply("[REDACTED:iban] and iban")
	if !sc4.Blocked() {
		t.Error("a real match next to a placeholder-looking token was missed")
	}
}

// A placeholder is recognised by where a scan wrote it, not by what it says: a
// client can type "[REDACTED:pw]" itself, and a rule must still read the text
// around and under it.
func TestTypedPlaceholdersAreNotTrusted(t *testing.T) {
	pw := rule("pw", TypeRegex, ActionBlock, allStages...)
	pw.Pattern = `(?i)password\s*=\s*\S+`
	s := compile(t, pw)
	for text, want := range map[string]bool{
		"password=hunter2":                  true,
		"password=hunter2[REDACTED:pw]":     true,
		"password=[REDACTED:pw]hunter2":     true,
		"password=hunter2[REDACTED:other]":  true,
		"a [REDACTED:pw] with nothing else": false,
	} {
		sc := s.Scan(StagePreRequest, false)
		sc.Apply(text)
		if sc.Blocked() != want {
			t.Errorf("whole %q: blocked = %v, want %v", text, sc.Blocked(), want)
		}
		_, sc2, _ := stream(t, s, chunks(text, 1), 32)
		if sc2.Blocked() != want {
			t.Errorf("stream %q: blocked = %v, want %v", text, sc2.Blocked(), want)
		}
	}
}

// A block rule that reaches over what a redact rule wrote reads the same text
// whole and in pieces.
func TestWholeAndStreamAgreeAroundPlaceholders(t *testing.T) {
	redact := rule("k", TypeTCKN, ActionRedact, allStages...)
	block := rule("line", TypeRegex, ActionBlock, allStages...)
	block.Pattern = `id: .*`
	s := compile(t, redact, block)
	for _, text := range []string{"id: " + tckn + " secret stuff", "x id: " + tckn, "no match " + tckn + " here"} {
		sc := s.Scan(StagePreRequest, false)
		sc.Apply(text)
		for _, n := range []int{1, 3, 7} {
			written, sc2, _ := stream(t, s, chunks(text, n), 32)
			if sc2.Blocked() != sc.Blocked() {
				t.Errorf("%q in pieces of %d: stream blocked = %v, whole = %v (written %q)", text, n, sc2.Blocked(), sc.Blocked(), written)
			}
			if !sc.Blocked() && written != sc.Apply(text) && strings.Contains(written, tckn) {
				t.Errorf("%q: the stream wrote the match: %q", text, written)
			}
		}
	}
}

func TestProbeOnlyBlocks(t *testing.T) {
	flag := rule("f", TypeTCKN, ActionFlag)
	redact := rule("r", TypeTCKN, ActionRedact)
	sc := compile(t, flag, redact).Scan(StagePreRequest, false)
	sc.Probe("id " + tckn)
	if sc.Blocked() || sc.Flags() != 0 || sc.Redactions() != 0 {
		t.Errorf("a probe counted something: blocked=%v flags=%d redactions=%d", sc.Blocked(), sc.Flags(), sc.Redactions())
	}
	block := rule("b", TypeTCKN, ActionBlock)
	sc = compile(t, block).Scan(StagePreRequest, false)
	sc.Probe("id " + tckn)
	if !sc.Blocked() || strings.Join(sc.Rules(), ",") != "b" {
		t.Errorf("blocked=%v rules=%v", sc.Blocked(), sc.Rules())
	}
}

func TestRewrites(t *testing.T) {
	s := compile(t, rule("f", TypeTCKN, ActionFlag), rule("r", TypeIBAN, ActionRedact, StagePostResponse))
	if s.Rewrites(StagePreRequest) || !s.Rewrites(StagePostResponse) || (*Set)(nil).Rewrites(StagePreRequest) {
		t.Error("Rewrites must be true only for a stage with a block or redact rule")
	}
}

func TestApplyNameWritesALegalName(t *testing.T) {
	kw := rule("tok", TypeKeywords, ActionRedact)
	kw.Keywords = []string{"token"}
	sc := compile(t, kw).Scan(StagePreRequest, false)
	if got := sc.ApplyName("refresh-token"); got != "refresh-REDACTED-tok" {
		t.Errorf("ApplyName = %q", got)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(sc.ApplyName("get-token_2")) {
		t.Error("the redacted name is not a legal name")
	}
	if got := sc.Apply("a token"); got != "a [REDACTED:tok]" {
		t.Errorf("Apply after ApplyName = %q: the name style must not stick", got)
	}
	// Block and flag act as in Apply.
	blk := rule("b", TypeTCKN, ActionBlock)
	sc2 := compile(t, blk).Scan(StagePreRequest, false)
	if sc2.ApplyName("n" + tckn); !sc2.Blocked() {
		t.Error("a block rule must block a name")
	}
}

func TestPlaceholderWarnings(t *testing.T) {
	redact := rule("iban", TypeTCKN, ActionRedact, allStages...)
	word := rule("w", TypeKeywords, ActionBlock, StagePreRequest)
	word.Keywords = []string{"iban"}
	if w := compile(t, redact, word).PlaceholderWarnings(); len(w) != 1 || !strings.Contains(w[0], `"w"`) || !strings.Contains(w[0], `"iban"`) {
		t.Errorf("warnings = %q", w)
	}
	other := rule("other", TypeKeywords, ActionBlock)
	other.Keywords = []string{"secret"}
	if w := compile(t, redact, other).PlaceholderWarnings(); len(w) != 0 {
		t.Errorf("warnings = %q", w)
	}
	// A block rule with the same id as nothing, and a flag rule, do not warn.
	flag := rule("f", TypeKeywords, ActionFlag)
	flag.Keywords = []string{"iban"}
	if w := compile(t, redact, flag).PlaceholderWarnings(); len(w) != 0 {
		t.Errorf("a flag rule does not stop a conversation: %q", w)
	}
	if w := (*Set)(nil).PlaceholderWarnings(); w != nil {
		t.Errorf("nil set: %q", w)
	}
}

// Known limit, pinned: a window holds text back while a redact rule settles,
// and a block rule that is written against the text a redact rule has already
// replaced sees the replacement in a stream. What the redact rule hid never
// leaks; whether the block fires can differ from the whole-text reading, and
// when it does not fire the rest of the text it would have matched is written
// (docs/api.md says so).
func TestStreamBlockRuleAfterAnEarlierRedactionMayNotFire(t *testing.T) {
	redact := rule("k", TypeTCKN, ActionRedact, allStages...)
	block := rule("line", TypeRegex, ActionBlock, allStages...)
	block.Pattern = `id: \d{11} secret`
	s := compile(t, redact, block)
	text := "id: " + tckn + " secret stuff and a long tail that follows"
	whole := s.Scan(StagePostResponse, false)
	whole.Apply(text)
	if !whole.Blocked() {
		t.Fatal("the whole text is blocked")
	}
	for _, n := range []int{1, 3} {
		written, sc, _ := stream(t, s, chunks(text, n), 32)
		if strings.Contains(written, tckn) {
			t.Errorf("pieces of %d: what the redact rule hid leaked: %q", n, written)
		}
		if !sc.Blocked() && !strings.Contains(written, "secret stuff") {
			t.Errorf("pieces of %d: not blocked, and the rest of the text is missing: %q", n, written)
		}
	}
}
