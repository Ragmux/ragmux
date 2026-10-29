package rag

import (
	"encoding/json"
	"math"
	"os"
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

func TestEstimateTokens(t *testing.T) {
	// The default divisor is the old characters/4 rule, rounded up.
	for _, lang := range []string{"", "simple", "en", "de", "zz"} {
		for _, c := range []struct {
			s    string
			want int
		}{{"", 0}, {"a", 1}, {"abcd", 1}, {"abcde", 2}, {strings.Repeat("x", 400), 100}, {"çğıöşü", 2}} {
			if got := EstimateTokens(c.s, lang); got != c.want {
				t.Errorf("EstimateTokens(%q, %q) = %d, want %d", c.s, lang, got, c.want)
			}
		}
	}
	// Runes, not bytes: a Turkish letter is two bytes and one character.
	text := strings.Repeat("ş", 300)
	if got, want := EstimateTokens(text, "tr"), int(math.Ceil(300/charsPerToken["tr"])); got != want {
		t.Errorf("EstimateTokens of 300 runes under tr = %d, want %d", got, want)
	}
	// A language with its own divisor estimates more tokens than the default
	// for the same text, never fewer.
	for lang, div := range charsPerToken {
		if div <= 0 || div >= DefaultCharsPerToken {
			t.Errorf("charsPerToken[%q] = %v: a table entry exists to be below the default %v", lang, div, DefaultCharsPerToken)
		}
		if EstimateTokens(text, lang) <= EstimateTokens(text, "en") {
			t.Errorf("%s: estimate %d is not above the default's %d", lang, EstimateTokens(text, lang), EstimateTokens(text, "en"))
		}
	}
}

// tokenReference is testdata/token_reference_tr.json: the token count of
// testdata/token_corpus_tr.txt under a reference tokenizer, written by the
// measurement described on charsPerToken (made in CI or a sandbox, never on a
// developer machine).
type tokenReference struct {
	Tokenizer string `json:"tokenizer"`
	Tokens    int    `json:"tokens"`
}

// TestEstimateTokensTurkishCorpus holds the Turkish divisor to the reference
// tokenizer's count on the fixed corpus: within 15%.
func TestEstimateTokensTurkishCorpus(t *testing.T) {
	corpus, err := os.ReadFile("testdata/token_corpus_tr.txt")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/token_reference_tr.json")
	if os.IsNotExist(err) {
		t.Skip("testdata/token_reference_tr.json is not there yet: the reference count has to be measured with a tokenizer " +
			"library, in CI or a sandbox (see charsPerToken); the Turkish divisor is provisional until then")
	}
	if err != nil {
		t.Fatal(err)
	}
	var ref tokenReference
	if err := json.Unmarshal(raw, &ref); err != nil || ref.Tokens <= 0 || ref.Tokenizer == "" {
		t.Fatalf("token_reference_tr.json = %q (%v): want {\"tokenizer\": ..., \"tokens\": N}", raw, err)
	}
	got := EstimateTokens(string(corpus), "tr")
	lo, hi := float64(ref.Tokens)*0.85, float64(ref.Tokens)*1.15
	if float64(got) < lo || float64(got) > hi {
		t.Errorf("EstimateTokens(corpus, tr) = %d, reference (%s) = %d: outside +-15%% [%.0f, %.0f]; re-measure the divisor",
			got, ref.Tokenizer, ref.Tokens, lo, hi)
	}
	// The default divisor is what was wrong for Turkish; keep the evidence.
	t.Logf("corpus %d runes: tr estimate %d, default estimate %d, reference %d", len([]rune(string(corpus))), got, EstimateTokens(string(corpus), "en"), ref.Tokens)
}
