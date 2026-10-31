// Package glob is the one pattern matcher the gateway uses for model names:
// literal text with "*" standing for any run of characters. The price table,
// the capability table and the tool allow/deny lists all resolve a name with
// it, so a pattern means the same thing wherever it is written.
//
// It is deliberately not path.Match: that gives "/" a meaning of its own, and
// model names routinely contain one ("deepseek-ai/DeepSeek-V3"). It is not a
// regular expression either, so a pattern cannot be made to backtrack.
package glob

import "strings"

// HasWildcard reports whether pattern contains a "*".
func HasWildcard(pattern string) bool { return strings.Contains(pattern, "*") }

// LiteralPrefix is the part of a pattern before its first wildcard.
func LiteralPrefix(pattern string) string {
	if i := strings.IndexByte(pattern, '*'); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// Compare orders two patterns by specificity: a negative result means a is
// more specific than b, positive means less, zero means a tie the caller
// breaks itself. An exact pattern beats any wildcard one; between wildcard
// patterns the longer literal prefix wins, which is what lets "gpt-4o-mini*"
// take "gpt-4o-mini-2024-07-18" from "gpt-4o*".
func Compare(a, b string) int {
	if ea, eb := !HasWildcard(a), !HasWildcard(b); ea != eb {
		if ea {
			return -1
		}
		return 1
	}
	la, lb := len(LiteralPrefix(a)), len(LiteralPrefix(b))
	switch {
	case la > lb:
		return -1
	case la < lb:
		return 1
	}
	return 0
}

// Match reports whether pattern matches s, with "*" standing for any run of
// characters, including an empty one.
func Match(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	head, tail := parts[0], parts[len(parts)-1]
	if !strings.HasPrefix(s, head) {
		return false
	}
	s = s[len(head):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return len(s) >= len(tail) && strings.HasSuffix(s, tail)
}
