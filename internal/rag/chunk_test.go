package rag

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitRespectsSizeAndOverlap(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString("Sentence number ")
		sb.WriteString(strings.Repeat("x", i%7))
		sb.WriteString(" ends here. ")
		if i%5 == 4 {
			sb.WriteString("\n\n")
		}
	}
	text := sb.String()
	chunks := Split(text, 200, 40)
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c.Content); n > 200+2 {
			t.Errorf("chunk %d too long: %d", i, n)
		}
		if c.Index != i {
			t.Errorf("chunk %d has index %d", i, c.Index)
		}
		if strings.TrimSpace(c.Content) == "" {
			t.Errorf("chunk %d empty", i)
		}
	}
	// Every word of the source must appear in some chunk.
	joined := strings.Join(func() []string {
		var s []string
		for _, c := range chunks {
			s = append(s, c.Content)
		}
		return s
	}(), " ")
	for _, w := range strings.Fields(text) {
		if !strings.Contains(joined, w) {
			t.Errorf("word %q lost", w)
		}
	}
}

func TestSplitLongParagraphNoInfiniteLoop(t *testing.T) {
	text := strings.Repeat("abcdefghij", 500) // no spaces at all
	chunks := Split(text, 100, 90)
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	total := 0
	for _, c := range chunks {
		total += utf8.RuneCountInString(c.Content)
	}
	if total < 5000 {
		t.Errorf("content lost: %d", total)
	}
}

func TestSplitEmpty(t *testing.T) {
	if got := Split("  \n\n ", 100, 10); len(got) != 0 {
		t.Errorf("expected nothing, got %v", got)
	}
}

func TestSplitUnicode(t *testing.T) {
	text := strings.Repeat("Türkçe metin çünkü ğüşiöç. ", 50)
	chunks := Split(text, 120, 20)
	for _, c := range chunks {
		if !utf8.ValidString(c.Content) {
			t.Errorf("invalid utf8 in chunk")
		}
	}
}
