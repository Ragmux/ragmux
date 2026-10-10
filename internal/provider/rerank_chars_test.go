package provider

import (
	"strings"
	"testing"
)

// The estimate counts what the adapters send: a passage past the cap
// contributes the capped length (plus the ellipsis), not its full size.
func TestRerankRequestCharsCapsPassages(t *testing.T) {
	big := strings.Repeat("a", 1<<20)
	got := RerankRequestChars("query", []string{"short", big})
	want := len("query") + len("short") + rerankPassageChars + len("…")
	if got != want {
		t.Errorf("RerankRequestChars = %d, want %d", got, want)
	}
}
