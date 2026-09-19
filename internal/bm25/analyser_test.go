package bm25

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateTokenizer(t *testing.T) {
	// Every code in the table is accepted, and every code in the table has a
	// non-empty Snowball language: an entry mapping to "" would build a DDL
	// pg_search rejects, which is the failure this whole table exists to
	// prevent. store's TestPgSearchStemmingTokenizerAnswersWithBM25 is what
	// proves the languages themselves against a live server.
	codes := StemmerCodes()
	if len(codes) != len(stemmers) || len(codes) == 0 {
		t.Fatalf("codes = %v", codes)
	}
	for i, code := range codes {
		if i > 0 && codes[i-1] >= code {
			t.Errorf("codes are not sorted: %v", codes)
		}
		if stemmers[code] == "" {
			t.Errorf("%q has no language", code)
		}
		if err := ValidateTokenizer(code + "_stem"); err != nil {
			t.Errorf("%s_stem rejected: %v", code, err)
		}
	}
	// Two languages pg_search does support were missing from the first
	// version of this table, so every store using them degraded exactly the
	// way en_stem did. Naming them keeps them from being dropped again.
	for _, code := range []string{"en", "cs", "pl", "tr"} {
		if stemmers[code] == "" {
			t.Errorf("%q must be supported", code)
		}
	}
	// Anything that is not a stemmer is none of this function's business:
	// tokenizer types are pg_search's to define.
	for _, ok := range []string{"default", "whitespace", "keyword", "source_code", ""} {
		if err := ValidateTokenizer(ok); err != nil {
			t.Errorf("%q should pass through: %v", ok, err)
		}
	}
	// A stemmer code with no entry is refused, and the message says what is
	// available rather than only what is wrong.
	for _, bad := range []string{"hi_stem", "sr_stem", "english_stem", "_stem"} {
		err := ValidateTokenizer(bad)
		if err == nil {
			t.Errorf("%q should be refused", bad)
			continue
		}
		if !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "en_stem") {
			t.Errorf("%q message should name the value and the supported set: %v", bad, err)
		}
	}
}

// TestStemmersAreDistinct holds the invariant TokenizerName depends on.
//
// The reverse lookup ranges over the map, so two codes sharing a language
// ("no" and a hypothetical "nb" both mapping to Norwegian) would make one
// index read back as either name depending on map iteration order: the drift
// warning would appear and disappear between restarts of the same unchanged
// deployment, and nothing else in the suite would notice.
func TestStemmersAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for code, lang := range stemmers {
		if first, dup := seen[lang]; dup {
			t.Errorf("%q and %q both map to %q; TokenizerName cannot reverse that", first, code, lang)
			continue
		}
		seen[lang] = code
	}
}

func TestTokenizerJSONAndNameRoundTrip(t *testing.T) {
	// Every analyser this build accepts survives the trip through the shape
	// PostgreSQL stores, which is what lets the drift check compare an index
	// against PG_SEARCH_TOKENIZER instead of against itself.
	names := append([]string{"default", "whitespace", "keyword"}, nil...)
	for _, code := range StemmerCodes() {
		names = append(names, code+"_stem")
	}
	for _, name := range names {
		raw := json.RawMessage(TokenizerJSON(name))
		if got := TokenizerName(raw); got != name {
			t.Errorf("%q round-tripped to %q via %s", name, got, raw)
		}
	}
	// A stemming analyser really is stored as the "default" type, so reading
	// the type alone would report every stemming index as drifted.
	if got := TokenizerJSON("en_stem"); got != `{"type":"default","stemmer":"English"}` {
		t.Errorf("en_stem = %s", got)
	}
	// An unknown stemmer code is not a stemmer: ValidateTokenizer refused it
	// at startup, and here it stays a type so pg_search rejects it loudly.
	if got := TokenizerJSON("hi_stem"); got != `{"type":"hi_stem"}` {
		t.Errorf("hi_stem = %s", got)
	}
	// "Could not tell" cases. Each must be silence, not a guess: the value
	// only reaches a log line, and a wrong guess is a drift warning about a
	// drift that is not there.
	for name, raw := range map[string]string{
		"unknown language":      `{"type":"default","stemmer":"Klingon"}`,
		"stemmer on other type": `{"type":"whitespace","stemmer":"English"}`,
		"not an object":         `"default"`,
		"truncated":             `{"type":`,
	} {
		if got := TokenizerName(json.RawMessage(raw)); got != "" {
			t.Errorf("%s: %s read as %q, want the empty string", name, raw, got)
		}
	}
}
