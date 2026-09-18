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

func TestSplitBlocksNeverMergesSections(t *testing.T) {
	blocks := []Block{
		{Text: "Alpha one.", Section: "Install > Docker"},
		{Text: "Alpha two.", Section: "Install > Docker"},
		{Text: "Beta one.", Section: "Install > Binary"},
		{Text: "Gamma page.", Page: 2},
		{Text: "Gamma more.", Page: 2},
		{Text: "Delta page.", Page: 3},
	}
	chunks := SplitBlocks(blocks, 1000, 100)
	if len(chunks) != 4 {
		t.Fatalf("expected 4 chunks (one per section/page), got %d: %+v", len(chunks), chunks)
	}
	want := []Chunk{
		{0, "Alpha one.\n\nAlpha two.", "Install > Docker", 0},
		{1, "Beta one.", "Install > Binary", 0},
		{2, "Gamma page.\n\nGamma more.", "", 2},
		{3, "Delta page.", "", 3},
	}
	for i, c := range chunks {
		if c != want[i] {
			t.Errorf("chunk %d = %+v, want %+v", i, c, want[i])
		}
	}
}

func TestSplitBlocksOverlapDoesNotCrossSections(t *testing.T) {
	long := strings.Repeat("Sentence in section one. ", 30)
	blocks := []Block{
		{Text: long, Section: "One"},
		{Text: "Section two text.", Section: "Two"},
	}
	chunks := SplitBlocks(blocks, 200, 60)
	for _, c := range chunks {
		if c.Section == "Two" && c.Content != "Section two text." {
			t.Errorf("section two chunk carries overlap from section one: %q", c.Content)
		}
		if c.Section == "One" && strings.Contains(c.Content, "two") {
			t.Errorf("section one chunk contains section two: %q", c.Content)
		}
	}
	if n := len(chunks); n < 4 {
		t.Errorf("expected the long section to be split, got %d chunks", n)
	}
	if last := chunks[len(chunks)-1]; last.Section != "Two" {
		t.Errorf("last chunk should be section two: %+v", last)
	}
}
