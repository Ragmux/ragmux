// Package bm25 describes the ParadeDB analyser that PG_SEARCH_TOKENIZER
// selects for the BM25 index.
//
// It is a leaf: config validates the setting at startup and store renders it
// into the index DDL, so both need this table, and config -- the package
// everything else is configured by -- must not have to depend on store to
// reach it.
package bm25

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// stemSuffix is the spelling an operator writes for a stemming analyser:
// "en_stem", "tr_stem". It is ParadeDB's own pre-0.16 tokenizer name and the
// one this project has always documented.
const stemSuffix = "_stem"

// stemmers maps the "<iso639-1>_stem" analyser names onto the shape pg_search
// accepts today: the "default" tokenizer carrying a Snowball "stemmer" field.
//
// pg_search dropped the "en_stem" tokenizer *type* somewhere before 0.25 and
// rejects it outright ("unknown tokenizer type: en_stem"), which made the one
// stemming value the documentation named turn every pg_search store into a
// permanent pgvector fallback. Translating here keeps the documented setting
// working and keeps the language name out of operator hands: only the ISO
// code is matched, and the interpolated Snowball name comes from this table,
// never from the environment.
//
// The twenty languages are the ones pg_search 0.25.9 accepted when each was
// tried against a live server on 2026-09-19. A language pg_search has no
// Snowball stemmer for (Catalan, Hindi, Indonesian, Serbian, Ukrainian, ...)
// has no entry rather than a nearby substitute, and ValidateTokenizer turns
// the missing entry into a startup error.
//
// Two codes must never name the same language: Name reverses this map, so a
// duplicate would make an index read back as one code on one boot and the
// other on the next, flickering the drift warning. TestStemmersAreDistinct
// holds that.
var stemmers = map[string]string{
	"ar": "Arabic", "cs": "Czech", "da": "Danish", "de": "German",
	"el": "Greek", "en": "English", "es": "Spanish", "fi": "Finnish",
	"fr": "French", "hu": "Hungarian", "it": "Italian", "nl": "Dutch",
	"no": "Norwegian", "pl": "Polish", "pt": "Portuguese", "ro": "Romanian",
	"ru": "Russian", "sv": "Swedish", "ta": "Tamil", "tr": "Turkish",
}

// StemmerCodes lists the "<code>_stem" analysers this build supports, sorted,
// for error messages and documentation.
func StemmerCodes() []string {
	codes := make([]string, 0, len(stemmers))
	for code := range stemmers {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// ValidateTokenizer rejects a PG_SEARCH_TOKENIZER naming a stemming analyser
// this build cannot translate.
//
// Only "<code>_stem" names are checked. Everything else is a tokenizer type,
// and which of those exist is pg_search's to define -- a wrong one still
// costs a warning and a fallback on the first search. A stemmer code is
// different: the language has to come from the table above, so an unknown one
// can never do anything but fail, and failing at startup beats failing
// invisibly on every search for the life of the process.
func ValidateTokenizer(tok string) error {
	code, ok := strings.CutSuffix(tok, stemSuffix)
	if !ok {
		return nil
	}
	if _, known := stemmers[code]; known {
		return nil
	}
	return fmt.Errorf("this build has no stemmer for %q: supported stemming analysers are %s",
		tok, strings.Join(StemmerCodes(), stemSuffix+", ")+stemSuffix)
}

// TokenizerJSON renders an analyser name as the JSON object pg_search expects
// inside an index's text_fields reloption.
//
// Anything that is not a recognised "<code>_stem" name is passed through as a
// tokenizer type unchanged: config.Load has already bounded it to
// [a-z][a-z0-9_]* so it cannot break out of the literal, an unknown *stemmer*
// was refused at startup by ValidateTokenizer, and an unknown type is rejected
// by pg_search at index build, where the caller turns it into a warning and a
// fallback to pgvector.
func TokenizerJSON(tok string) string {
	if code, ok := strings.CutSuffix(tok, stemSuffix); ok {
		if lang, known := stemmers[code]; known {
			return fmt.Sprintf(`{"type":"default","stemmer":%q}`, lang)
		}
	}
	return fmt.Sprintf(`{"type":%q}`, tok)
}

// TokenizerName is TokenizerJSON backwards: it turns the tokenizer object
// stored in an index's reloptions back into the name an operator would have
// written in PG_SEARCH_TOKENIZER.
//
// The caller compares the result against exactly that setting, so a stemming
// index has to come back as "en_stem" rather than as the "default" type it is
// stored as -- otherwise every stemming index reads as drifted from the
// setting that built it.
//
// An empty result means "could not tell", never "no analyser": the value only
// ever reaches a log line, so a shape this build does not recognise degrades
// to saying nothing rather than to reporting a drift that is not there.
func TokenizerName(raw json.RawMessage) string {
	var tok struct {
		Type    string `json:"type"`
		Stemmer string `json:"stemmer"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return ""
	}
	if tok.Stemmer == "" {
		return tok.Type
	}
	// A stemmer only means "<code>_stem" on the tokenizer this build pairs it
	// with. A hand-built index that hangs a stemmer off some other type is
	// not something PG_SEARCH_TOKENIZER can express, so it is unreadable
	// rather than silently reported as the stemming analyser.
	if tok.Type != "default" {
		return ""
	}
	for code, lang := range stemmers {
		if lang == tok.Stemmer {
			return code + stemSuffix
		}
	}
	return ""
}
