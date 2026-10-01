package bm25

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode"
)

// Turkish has two letter i's, and the case pairs are not the ones every other
// Latin-script language uses: İ/i and I/ı. A lowercaser that does not know it
// is reading Turkish folds I to i, so "ISLAK" (wet) is indexed as "islak" and
// never meets a query for "ıslak".
//
// This package does not tokenise anything itself -- pg_search does, with the
// analyser this package names -- so these tests pin the half that is pure Go:
// that Turkish selects the Turkish stemmer, and what each of the three
// lowercasing rules a search path can end up with does to Turkish words.
// TestTurkishCaseTableIsConsistent proves every match column of turkishCases
// follows from the rule it is filed under rather than from a guess. Store's
// locale_test.go repeats the same rules (test files cannot share code across
// packages), computes its expectations from them and pins its own table with
// TestTurkishFoldExpectations; nothing here checks that file.
//
// To be plain about the limit: nothing here measures BM25. Apart from
// TestTurkishAnalyser, these tests exercise Go's unicode tables and a
// hand-written model of tantivy's lowercaser, so they are the specification,
// not the observation. If pg_search's "default" tokenizer changed its folding
// (ASCII folding, NFC), every test in this file would stay green. The
// measurement is TestPgSearchTurkishCaseFolding in internal/store, which runs
// only where pg_search exists -- in CI, the test-paradedb job.

// turkishCase is one document word, one query word and whether the query
// finds the document under each lowercasing rule.
type turkishCase struct {
	doc, query string
	// simple is Unicode's one-to-one lowercase mapping: what glibc's
	// towlower does in every non-Turkish UTF-8 locale (en_US.UTF-8 in the
	// official PostgreSQL image, C.UTF-8 in the all-in-one image), and so
	// what PostgreSQL's 'simple' text search configuration indexes.
	simple bool
	// full is Unicode's full lowercase mapping, where İ becomes two code
	// points, i + U+0307 COMBINING DOT ABOVE. Rust's char::to_lowercase
	// implements it, and through it tantivy's LowerCaser, which pg_search's
	// "default" tokenizer runs.
	full bool
	// turkish is the answer a Turkish reader expects: İ→i and I→ı.
	turkish bool
}

var turkishCases = []turkishCase{
	{doc: "İSTANBUL", query: "istanbul", simple: true, full: false, turkish: true},
	{doc: "İstanbul", query: "İSTANBUL", simple: true, full: true, turkish: true},
	{doc: "İzmir", query: "izmir", simple: true, full: false, turkish: true},
	{doc: "ŞİŞLİ", query: "şişli", simple: true, full: false, turkish: true},
	{doc: "ıslak", query: "ıslak", simple: true, full: true, turkish: true},
	{doc: "ISLAK", query: "ıslak", simple: false, full: false, turkish: true},
	{doc: "IĞDIR", query: "ığdır", simple: false, full: false, turkish: true},
	{doc: "IĞDIR", query: "Iğdır", simple: false, full: false, turkish: true},
	// The two below are the price of Turkish-correct folding: an all-caps
	// word typed with a plain I, or a query typed on a keyboard without ı,
	// stops matching. Any fix has to weigh these against the rows above.
	{doc: "ISTANBUL", query: "istanbul", simple: true, full: true, turkish: false},
	{doc: "ISLAK", query: "islak", simple: true, full: true, turkish: false},
}

// tokens splits text the way both search paths do before lowercasing: on
// anything that is not a letter or a digit.
func tokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func lowerSimple(s string) string { return strings.Map(unicode.ToLower, s) }

// lowerFull is lowerSimple plus the one unconditional multi-rune lowercase
// mapping in Unicode's SpecialCasing.txt, which is the one this whole test
// is about. (The other special lowercase, final sigma, is context-dependent
// and irrelevant to Turkish.)
func lowerFull(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == 'İ' {
			b.WriteString("i̇")
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func lowerTurkish(s string) string { return strings.Map(unicode.TurkishCase.ToLower, s) }

// matches reports whether any query term equals any document term after both
// went through the same lowercaser -- the OR-of-terms both backends run.
func matches(lower func(string) string, doc, query string) bool {
	terms := map[string]bool{}
	for _, tok := range tokens(doc) {
		terms[lower(tok)] = true
	}
	for _, tok := range tokens(query) {
		if terms[lower(tok)] {
			return true
		}
	}
	return false
}

func TestTurkishCaseFoldingRules(t *testing.T) {
	// The single letters the table is built from, under each rule.
	for _, c := range []struct {
		in, simple, full, turkish string
	}{
		{"İ", "i", "i̇", "i"},
		{"I", "i", "i", "ı"},
		{"ı", "ı", "ı", "ı"},
		{"i", "i", "i", "i"},
		{"Ğ", "ğ", "ğ", "ğ"},
		{"Ş", "ş", "ş", "ş"},
		{"İSTANBUL", "istanbul", "i̇stanbul", "istanbul"},
		{"IĞDIR", "iğdir", "iğdir", "ığdır"},
	} {
		if got := lowerSimple(c.in); got != c.simple {
			t.Errorf("simple(%q) = %q, want %q", c.in, got, c.simple)
		}
		if got := lowerFull(c.in); got != c.full {
			t.Errorf("full(%q) = %q, want %q", c.in, got, c.full)
		}
		if got := lowerTurkish(c.in); got != c.turkish {
			t.Errorf("turkish(%q) = %q, want %q", c.in, got, c.turkish)
		}
	}
	// Go's own strings.ToLower is the simple rule, not the full one: a Go
	// component lowercasing a query before it reaches pg_search would hand it
	// "istanbul" for a document pg_search indexed as "i̇stanbul".
	if got := strings.ToLower("İSTANBUL"); got != "istanbul" {
		t.Errorf("strings.ToLower(İSTANBUL) = %q", got)
	}
	// The combining dot the full rule produces is not a letter, so a
	// tokenizer that split *after* lowercasing would cut "i̇stanbul" in two.
	// Both search paths split first; tokens() does too.
	if got := tokens("İSTANBUL"); len(got) != 1 {
		t.Errorf("tokens(İSTANBUL) = %q, want one token", got)
	}
}

func TestTurkishCaseTableIsConsistent(t *testing.T) {
	for _, c := range turkishCases {
		if got := matches(lowerSimple, c.doc, c.query); got != c.simple {
			t.Errorf("%q vs %q: simple match = %v, table says %v", c.doc, c.query, got, c.simple)
		}
		if got := matches(lowerFull, c.doc, c.query); got != c.full {
			t.Errorf("%q vs %q: full match = %v, table says %v", c.doc, c.query, got, c.full)
		}
		if got := matches(lowerTurkish, c.doc, c.query); got != c.turkish {
			t.Errorf("%q vs %q: turkish match = %v, table says %v", c.doc, c.query, got, c.turkish)
		}
	}
}

// TestTurkishAnalyser pins what PG_SEARCH_TOKENIZER=tr_stem builds. The
// stemmer runs after tantivy's lowercaser, so it inherits the full rule's
// İ handling; it adds suffix stripping, not Turkish case folding.
func TestTurkishAnalyser(t *testing.T) {
	if err := ValidateTokenizer("tr_stem"); err != nil {
		t.Fatalf("tr_stem rejected: %v", err)
	}
	raw := TokenizerJSON("tr_stem")
	if raw != `{"type":"default","stemmer":"Turkish"}` {
		t.Errorf("tr_stem = %s", raw)
	}
	if got := TokenizerName(json.RawMessage(raw)); got != "tr_stem" {
		t.Errorf("tr_stem round-tripped to %q", got)
	}
}
