// Package pricing turns token counts into an estimated cost. It ships a
// built-in price table (prices.json), seeds it into model_prices on start
// and resolves a provider type plus a model name to a price.
//
// Precedence is by pattern specificity alone — exact, then longest literal
// prefix, then lowest id — and never by where a row came from. An operator
// overrides a shipped number by editing that row, which keeps its pattern
// and flips its source to "user"; a broader pattern they add does not
// shadow the more specific built-in rows, because that would silently
// re-price every model the shipped table already knows.
//
// Every figure is an estimate for the dashboard, not a bill: providers
// round, discount and change prices without telling the gateway.
package pricing

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Price is the cost of one model per million tokens. CacheWrite and
// CacheRead are absolute prices, not multipliers of Input: every provider
// picks its own convention (Anthropic bills writes at 1.25x and reads at
// 0.1x, OpenAI does not bill writes at all and reads at 0.5x), so a
// multiplier in the schema would be wrong for someone.
type Price struct {
	Input      float64
	Output     float64
	CacheWrite float64
	CacheRead  float64
	Currency   string
	Pattern    string
	// Source is "builtin" or "user"; it is recorded on the request log as
	// cost_source so a figure can be traced back to where it came from.
	Source string
}

// SourceBuiltin and SourceUser are the two origins of a price row.
const (
	SourceBuiltin = "builtin"
	SourceUser    = "user"
)

// Row is one entry of a price table: the pattern it matches plus the id
// that breaks ties between equally specific patterns.
type Row struct {
	ID           int64
	ProviderType string
	Pattern      string
	Price        Price
}

// Table resolves a provider type and model name to a price. Rows are sorted
// by precedence once, so Lookup is a scan that stops at the first match.
type Table struct {
	rows []Row
}

// NewTable sorts rows into precedence order: an exact pattern first, then
// the wildcard pattern with the longest literal prefix (the text before the
// first "*"), then the lowest id. The longest prefix is what makes
// "gpt-4o-mini*" win over "gpt-4o*" for "gpt-4o-mini-2024-07-18".
func NewTable(rows []Row) *Table {
	sorted := make([]Row, len(rows))
	copy(sorted, rows)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if ea, eb := !hasWildcard(a.Pattern), !hasWildcard(b.Pattern); ea != eb {
			return ea
		}
		if la, lb := len(literalPrefix(a.Pattern)), len(literalPrefix(b.Pattern)); la != lb {
			return la > lb
		}
		return a.ID < b.ID
	})
	return &Table{rows: sorted}
}

// Rows returns the table's entries in precedence order.
func (t *Table) Rows() []Row { return t.rows }

// Lookup returns the price for a model, or false when nothing matches. A
// caller that gets false records no cost rather than guessing one.
func (t *Table) Lookup(providerType, model string) (Price, bool) {
	if t == nil {
		return Price{}, false
	}
	for _, r := range t.rows {
		if r.ProviderType == providerType && matchGlob(r.Pattern, model) {
			return r.Price, true
		}
	}
	return Price{}, false
}

// maxMicros is math.MaxInt64 as a float64. The conversion rounds up to
// 2^63, so a product at or above it is out of int64 range and saturates.
const maxMicros = float64(math.MaxInt64)

// CostMicros is the cost of one request in millionths of a currency unit.
// Because Price is per 10^6 tokens and a micro is 10^-6 dollars, tokens
// times price-per-million is already the cost in micros; no extra scaling.
//
// promptTokens includes the cached and freshly written parts (that is the
// convention Usage normalises every provider onto), so the part billed at
// the full input rate is what is left after both are taken out.
//
// The result is saturated to [0, math.MaxInt64]. A cost is never negative,
// however broken the numbers an upstream reported, and the ceiling matters
// for its own reason: int64(math.Round(x)) is implementation-defined for an
// x outside int64's range and arm64 and amd64 disagree on what it produces.
func (p Price) CostMicros(promptTokens, cachedPrompt, cacheWrite, completionTokens int) int64 {
	prompt, cached := nonNegative(promptTokens), nonNegative(cachedPrompt)
	written, completion := nonNegative(cacheWrite), nonNegative(completionTokens)
	uncached := prompt - cached - written
	if uncached < 0 {
		uncached = 0
	}
	micros := float64(uncached)*p.Input +
		float64(written)*p.CacheWrite +
		float64(cached)*p.CacheRead +
		float64(completion)*p.Output
	// A NaN (a table row holding one, or 0 x Inf) fails every comparison and
	// falls through to zero, which is the same answer a missing price gives.
	if !(micros > 0) {
		return 0
	}
	if rounded := math.Round(micros); rounded < maxMicros {
		return int64(rounded)
	}
	return math.MaxInt64
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func hasWildcard(pattern string) bool { return strings.Contains(pattern, "*") }

// literalPrefix is the part of a pattern before its first wildcard.
func literalPrefix(pattern string) string {
	if i := strings.IndexByte(pattern, '*'); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// matchGlob reports whether pattern matches s, with "*" standing for any run
// of characters. path.Match is unusable here: it gives "/" a meaning of its
// own and model names routinely contain one ("deepseek-ai/DeepSeek-V3").
func matchGlob(pattern, s string) bool {
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

// builtinFile is the JSON shape of prices.json.
type builtinFile struct {
	Version  int           `json:"version"`
	Currency string        `json:"currency"`
	Unit     string        `json:"unit"`
	Models   []builtinItem `json:"models"`
}

// builtinItem is one model entry. CacheWrite and CacheRead are pointers
// because "absent" and "free" are different: an absent cache price falls
// back to the input rate, a zero one really means zero.
type builtinItem struct {
	ProviderType string   `json:"provider_type"`
	Model        string   `json:"model"`
	Input        float64  `json:"input"`
	Output       float64  `json:"output"`
	CacheWrite   *float64 `json:"cache_write"`
	CacheRead    *float64 `json:"cache_read"`
}

func parseBuiltin(raw []byte) (*builtinFile, error) {
	var f builtinFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse built-in price table: %w", err)
	}
	if f.Version < 1 {
		return nil, fmt.Errorf("built-in price table has version %d, want 1 or more", f.Version)
	}
	if f.Unit != "per_million_tokens" {
		return nil, fmt.Errorf("built-in price table unit %q is not per_million_tokens", f.Unit)
	}
	seen := map[string]bool{}
	for _, m := range f.Models {
		if m.ProviderType == "" || m.Model == "" {
			return nil, fmt.Errorf("built-in price table has an entry without a provider type or model")
		}
		key := m.ProviderType + "\x00" + m.Model
		if seen[key] {
			return nil, fmt.Errorf("built-in price table lists %s/%s twice", m.ProviderType, m.Model)
		}
		seen[key] = true
		if m.Input < 0 || m.Output < 0 || negative(m.CacheWrite) || negative(m.CacheRead) {
			return nil, fmt.Errorf("built-in price for %s/%s is negative", m.ProviderType, m.Model)
		}
	}
	return &f, nil
}

func negative(v *float64) bool { return v != nil && *v < 0 }

// resolve fills in the fallbacks: an absent cache price is the input price,
// which is what a provider charges when nothing was cached.
func (m builtinItem) resolve(currency string) Price {
	p := Price{Input: m.Input, Output: m.Output, CacheWrite: m.Input, CacheRead: m.Input,
		Currency: currency, Pattern: m.Model, Source: SourceBuiltin}
	if m.CacheWrite != nil {
		p.CacheWrite = *m.CacheWrite
	}
	if m.CacheRead != nil {
		p.CacheRead = *m.CacheRead
	}
	return p
}
