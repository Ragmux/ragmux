package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClampToolNames(t *testing.T) {
	if got := clampToolNames(nil); got != nil {
		t.Errorf("nil -> %v, want nil", got)
	}
	if got := clampToolNames([]string{"", "\x00"}); got != nil {
		t.Errorf("only empty names -> %v, want nil", got)
	}
	got := clampToolNames([]string{"b", "a", "b", "a\x00", "c"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("sorted and de-duplicated = %v", got)
	}

	long := strings.Repeat("n", 500)
	got = clampToolNames([]string{long, long + "x"})
	if len(got) != 1 || len(got[0]) != MaxToolNameBytes {
		t.Errorf("long names = %d entries, first %d bytes; want one of %d bytes", len(got), len(got[0]), MaxToolNameBytes)
	}

	// A character straddling the limit is dropped whole.
	got = clampToolNames([]string{strings.Repeat("a", MaxToolNameBytes-1) + "é"})
	if len(got) != 1 || !utf8.ValidString(got[0]) || got[0] != strings.Repeat("a", MaxToolNameBytes-1) {
		t.Errorf("cut inside a character: %q", got)
	}

	many := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		many = append(many, "tool_"+string(rune('a'+i/26))+string(rune('a'+i%26)))
	}
	if got := clampToolNames(many); len(got) != MaxToolNames || got[0] != "tool_aa" {
		t.Errorf("many names = %d entries starting %q, want %d from tool_aa", len(got), got[0], MaxToolNames)
	}
}

func TestClampSessionID(t *testing.T) {
	if got := clampSessionID("run-1"); got != "run-1" {
		t.Errorf("plain = %q", got)
	}
	if got := clampSessionID("a\x00b"); got != "ab" {
		t.Errorf("NUL not removed: %q", got)
	}
	if got := clampSessionID(strings.Repeat("s", 1000)); len(got) != MaxSessionIDBytes {
		t.Errorf("len = %d, want %d", len(got), MaxSessionIDBytes)
	}
}
