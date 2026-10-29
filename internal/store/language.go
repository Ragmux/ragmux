package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// LanguageSimple is the language of a store that is neither stemmed nor
// stripped of stop words: PostgreSQL's 'simple' configuration, which is what
// chunks.tsv has always been generated with.
const LanguageSimple = "simple"

// ErrInvalidLanguage marks a store language (or a legacy fts_config) the server
// cannot honour. The admin API answers it with 400.
var ErrInvalidLanguage = errors.New("invalid store language")

// languageConfigs maps an ISO 639-1 code onto the PostgreSQL text search
// configuration that stems and stop-word-filters that language (ADR-027). It
// is the one table: the write path validates against it, the search and
// ingest paths read the configuration name from it, and the legacy fts_config
// translation is its inverse. Only languages PostgreSQL ships a built-in
// configuration for are listed; a code without an entry is rejected, and an
// entry whose configuration the connected server lacks (an older PostgreSQL
// without 'armenian', say) is rejected too, by the pg_ts_config check.
var languageConfigs = map[string]string{
	"ar": "arabic",
	"ca": "catalan",
	"da": "danish",
	"de": "german",
	"el": "greek",
	"en": "english",
	"es": "spanish",
	"eu": "basque",
	"fi": "finnish",
	"fr": "french",
	"ga": "irish",
	"hi": "hindi",
	"hu": "hungarian",
	"hy": "armenian",
	"id": "indonesian",
	"it": "italian",
	"lt": "lithuanian",
	"ne": "nepali",
	"nl": "dutch",
	"no": "norwegian",
	"pt": "portuguese",
	"ro": "romanian",
	"ru": "russian",
	"sr": "serbian",
	"sv": "swedish",
	"ta": "tamil",
	"tr": "turkish",
	"yi": "yiddish",
}

// configLanguages is languageConfigs turned around.
var configLanguages = func() map[string]string {
	m := make(map[string]string, len(languageConfigs))
	for code, cfg := range languageConfigs {
		m[cfg] = code
	}
	return m
}()

// LanguageCodes lists the accepted language values, 'simple' first.
func LanguageCodes() []string {
	out := make([]string, 0, len(languageConfigs))
	for code := range languageConfigs {
		out = append(out, code)
	}
	sort.Strings(out)
	return append([]string{LanguageSimple}, out...)
}

// LanguageConfig is the text search configuration name of a language: "simple"
// for 'simple' (and for the empty string), the mapped configuration for an ISO
// code, and false for anything else.
func LanguageConfig(language string) (string, bool) {
	if language == "" || language == LanguageSimple {
		return LanguageSimple, true
	}
	cfg, ok := languageConfigs[language]
	return cfg, ok
}

// LanguageForConfig is the language a legacy fts_config name stands for
// ("english" -> "en"), and false for a configuration no language maps to.
func LanguageForConfig(cfg string) (string, bool) {
	if cfg == LanguageSimple {
		return LanguageSimple, true
	}
	code, ok := configLanguages[cfg]
	return code, ok
}

// checkLanguage validates one language value against the mapping table and the
// server's pg_ts_config.
func (s *Store) checkLanguage(ctx context.Context, language string) error {
	cfg, ok := LanguageConfig(language)
	if !ok {
		return fmt.Errorf("%w: language %q is not 'simple' or one of %s", ErrInvalidLanguage, language,
			strings.Join(LanguageCodes()[1:], ", "))
	}
	if exists, err := s.TextSearchConfigExists(ctx, cfg); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("%w: language %q needs the text search configuration %q, which this PostgreSQL server does not have",
			ErrInvalidLanguage, language, cfg)
	}
	return nil
}

// ResolveLanguage turns the language and the deprecated fts_config a caller
// sent into the store language. Both are optional.
//
//   - language alone is validated and used;
//   - fts_config alone is translated to the language it names (an old client
//     that only knows fts_config keeps working), a configuration no language
//     maps to is an error;
//   - both must agree; a fts_config that names another language than
//     language is an error.
//
// specified is false when neither was sent, so the caller can keep what the
// store has (an update) or default to 'simple' (a create).
func (s *Store) ResolveLanguage(ctx context.Context, language, ftsConfig string) (resolved string, specified bool, err error) {
	language = strings.ToLower(strings.TrimSpace(language))
	ftsConfig = strings.ToLower(strings.TrimSpace(ftsConfig))
	if language == "" && ftsConfig == "" {
		return "", false, nil
	}
	if ftsConfig != "" {
		fromConfig, ok := LanguageForConfig(ftsConfig)
		if !ok {
			return "", true, fmt.Errorf("%w: fts_config %q is deprecated and maps to no language; set language to 'simple' or one of %s",
				ErrInvalidLanguage, ftsConfig, strings.Join(LanguageCodes()[1:], ", "))
		}
		if language == "" {
			language = fromConfig
		} else if language != fromConfig {
			return "", true, fmt.Errorf("%w: fts_config %q (language %q) contradicts language %q; send language only",
				ErrInvalidLanguage, ftsConfig, fromConfig, language)
		}
	}
	if err := s.checkLanguage(ctx, language); err != nil {
		return "", true, err
	}
	return language, true, nil
}
