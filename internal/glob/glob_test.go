package glob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"gpt-4o", "gpt-4o", true},
		{"gpt-4o", "gpt-4o-mini", false},
		{"*", "anything/at-all:v2", true},
		{"gpt-4o*", "gpt-4o", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "acb", false},
		{"*/DeepSeek-R1", "deepseek-ai/DeepSeek-R1", true},
		{"*/DeepSeek-R1", "DeepSeek-R1", false},
		{"llama3*", "llama2", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.s); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int // sign only
	}{
		{"gpt-4o", "gpt-4o*", -1},
		{"gpt-4o*", "gpt-4o", 1},
		{"gpt-4o-mini*", "gpt-4o*", -1},
		{"gpt-4o*", "gpt-4o-mini*", 1},
		{"a*", "a*", 0},
		{"a*", "b*", 0},
		{"a", "b", 0},
		{"*", "x*", 1},
	}
	for _, c := range cases {
		got := Compare(c.a, c.b)
		if (got < 0) != (c.want < 0) || (got > 0) != (c.want > 0) {
			t.Errorf("Compare(%q, %q) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
}
