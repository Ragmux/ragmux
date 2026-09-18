package rag

import (
	"strings"
	"unicode/utf8"
)

// Chunk is one text window produced by the splitter.
type Chunk struct {
	Index   int
	Content string
}

// Split divides text into windows of roughly size characters (runes) with
// the given overlap. Paragraph boundaries are preferred; paragraphs longer
// than size are split on sentence/word boundaries where possible.
func Split(text string, size, overlap int) []Chunk {
	if size <= 0 {
		size = 1000
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 4
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// First pass: greedy packing of paragraphs into windows.
	var units []string
	for _, p := range strings.Split(text, "\n\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if utf8.RuneCountInString(p) <= size {
			units = append(units, p)
			continue
		}
		units = append(units, slide(p, size, overlap)...)
	}

	var out []Chunk
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		out = append(out, Chunk{Index: len(out), Content: strings.TrimSpace(string(cur))})
		if overlap > 0 && len(cur) > overlap {
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
		ur := []rune(u)
		if len(cur) > 0 && len(cur)+2+len(ur) > size {
			flush()
		}
		if len(cur) > 0 {
			cur = append(cur, '\n', '\n')
		}
		cur = append(cur, ur...)
		if len(cur) >= size {
			flush()
		}
	}
	if len(strings.TrimSpace(string(cur))) > 0 {
		// Avoid emitting a trailing chunk that is purely overlap of the last one.
		last := ""
		if len(out) > 0 {
			last = out[len(out)-1].Content
		}
		if c := strings.TrimSpace(string(cur)); !strings.HasSuffix(last, c) {
			out = append(out, Chunk{Index: len(out), Content: c})
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

// EstimateTokens approximates token count as characters / 4.
func EstimateTokens(s string) int {
	n := utf8.RuneCountInString(s)
	return (n + 3) / 4
}
