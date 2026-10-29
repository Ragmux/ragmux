package rag

import (
	"math"
	"strings"
	"unicode/utf8"
)

// Chunk is one text window produced by the splitter together with the
// section and page of the blocks it was built from.
type Chunk struct {
	Index   int
	Content string
	Section string
	Page    int
}

// Split divides text into windows of roughly size characters (runes) with
// the given overlap. Paragraph boundaries are preferred; paragraphs longer
// than size are split on sentence/word boundaries where possible.
func Split(text string, size, overlap int) []Chunk {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return SplitBlocks([]Block{{Text: text}}, size, overlap)
}

// SplitBlocks packs blocks into windows of roughly size runes with the
// given overlap. A window never spans two sections: when the section (or
// page) changes a new chunk starts, so every chunk carries one Section and
// Page. Blocks larger than size are split on sentence/word boundaries.
func SplitBlocks(blocks []Block, size, overlap int) []Chunk {
	if size <= 0 {
		size = 1000
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 4
	}

	// First pass: split every block into paragraph units no larger than size.
	type unit struct {
		text    string
		section string
		page    int
	}
	var units []unit
	for _, b := range blocks {
		for _, p := range strings.Split(strings.TrimSpace(b.Text), "\n\n") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if utf8.RuneCountInString(p) <= size {
				units = append(units, unit{p, b.Section, b.Page})
				continue
			}
			for _, piece := range slide(p, size, overlap) {
				units = append(units, unit{piece, b.Section, b.Page})
			}
		}
	}

	var out []Chunk
	var cur []rune
	var curSection string
	var curPage int
	// onlyOverlap is true while cur holds nothing but the tail carried over
	// from the previous chunk.
	onlyOverlap := false
	flush := func(keepOverlap bool) {
		if len(cur) == 0 {
			return
		}
		out = append(out, Chunk{Index: len(out), Content: strings.TrimSpace(string(cur)), Section: curSection, Page: curPage})
		onlyOverlap = keepOverlap && overlap > 0 && len(cur) > overlap
		if onlyOverlap {
			tail := cur[len(cur)-overlap:]
			// Start the overlap at a word boundary when possible.
			if i := indexRune(tail, ' '); i >= 0 && i < len(tail)-1 {
				tail = tail[i+1:]
			}
			cur = append([]rune{}, tail...)
		} else {
			cur = cur[:0]
		}
	}
	for _, u := range units {
		ur := []rune(u.text)
		if len(cur) > 0 && (u.section != curSection || u.page != curPage) {
			// Section or page boundary: no overlap across it.
			if onlyOverlap {
				cur = cur[:0]
			} else {
				flush(false)
			}
		}
		if len(cur) > 0 && len(cur)+2+len(ur) > size {
			flush(true)
		}
		if len(cur) == 0 {
			curSection, curPage = u.section, u.page
		}
		if len(cur) > 0 {
			cur = append(cur, '\n', '\n')
		}
		cur = append(cur, ur...)
		onlyOverlap = false
		if len(cur) >= size {
			flush(true)
		}
	}
	if len(strings.TrimSpace(string(cur))) > 0 {
		// Avoid emitting a trailing chunk that is purely overlap of the last one.
		last := ""
		if len(out) > 0 {
			last = out[len(out)-1].Content
		}
		if c := strings.TrimSpace(string(cur)); !strings.HasSuffix(last, c) {
			out = append(out, Chunk{Index: len(out), Content: c, Section: curSection, Page: curPage})
		}
	}
	return out
}

// slide splits one oversized paragraph into overlapping windows, breaking on
// sentence or word boundaries near the window end.
func slide(p string, size, overlap int) []string {
	r := []rune(p)
	var out []string
	start := 0
	for start < len(r) {
		end := start + size
		if end >= len(r) {
			out = append(out, strings.TrimSpace(string(r[start:])))
			break
		}
		cut := end
		// Look back up to 20% of the window for a good boundary.
		minCut := end - size/5
		for i := end; i > minCut; i-- {
			c := r[i-1]
			if c == '.' || c == '!' || c == '?' || c == '\n' {
				cut = i
				break
			}
		}
		if cut == end {
			for i := end; i > minCut; i-- {
				if r[i-1] == ' ' {
					cut = i
					break
				}
			}
		}
		out = append(out, strings.TrimSpace(string(r[start:cut])))
		next := cut - overlap
		if next <= start {
			next = cut
		}
		start = next
	}
	return out
}

func indexRune(rs []rune, r rune) int {
	for i, c := range rs {
		if c == r {
			return i
		}
	}
	return -1
}

// DefaultCharsPerToken is the characters-per-token divisor of a language with
// no entry in charsPerToken: the long-standing rule of thumb for English.
const DefaultCharsPerToken = 4.0

// charsPerToken is the characters-per-token divisor of the languages whose
// text tokenises noticeably worse than English (ADR-027, R27). The key is a
// store language; anything not listed uses DefaultCharsPerToken. The table is
// an estimate for token limits and usage figures, never a count: it is
// replaced by the upstream's own usage whenever the upstream reports one.
//
// How a value is measured: take a fixed corpus of the language, count its
// characters (runes) and its tokens under a reference tokenizer
// (o200k_base), and enter characters / tokens, rounded to one decimal. The
// measurement needs a tokenizer library and its vocabulary, so it is made in
// CI or a sandbox, never on a developer machine, and the corpus, the
// tokenizer and the date go into the comment of the entry.
//
//	tr: 3.5, measured 2026-10-28 on testdata/token_corpus_tr.txt (1135
//	    characters, counted as runes like EstimateTokens does) with tiktoken
//	    0.9.0 in a throwaway container with the corpus mounted read-only:
//	    o200k_base 307 tokens (3.70 characters/token), cl100k_base 427 tokens
//	    (2.66 characters/token). 3.5 estimates 325 tokens for the corpus,
//	    +6% over o200k_base (inside the +-15% TestEstimateTokensTurkishCorpus
//	    holds it to, against testdata/token_reference_tr.json) and well under
//	    cl100k_base's count, so older tokenizers are over-estimated rather
//	    than under-estimated, the safe direction for a limit check.
var charsPerToken = map[string]float64{
	"tr": 3.5,
}

// EstimateTokens approximates the token count of s as characters divided by
// the language's characters-per-token divisor (4 unless the language is in
// charsPerToken). language is a store language: "simple", an ISO 639-1 code
// or "" all mean the default. Chat messages are not estimated through it.
func EstimateTokens(s, language string) int {
	n := utf8.RuneCountInString(s)
	div := DefaultCharsPerToken
	if d, ok := charsPerToken[language]; ok && d > 0 {
		div = d
	}
	return int(math.Ceil(float64(n) / div))
}
