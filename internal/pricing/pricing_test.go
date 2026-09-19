package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// pricesSHA256 pins the embedded table to its version number: change a
// price without bumping "version" in prices.json and this fails, which is
// what keeps seeded builtin rows from silently disagreeing with the file.
const pricesSHA256 = "45c8ce28eca57e738c77666c9dfbb336febe84c3dae031f2ac091da46c3c24af"

func TestPricesVersionPinned(t *testing.T) {
	sum := sha256.Sum256(BuiltinBytes())
	got := hex.EncodeToString(sum[:])
	if got != pricesSHA256 {
		t.Fatalf("prices.json changed: sha256 = %s\n"+
			"bump \"version\" in prices.json (seeded builtin rows only refresh when it grows), then set pricesSHA256 to the new digest", got)
	}
	v, err := BuiltinVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v < 1 {
		t.Errorf("builtin version = %d", v)
	}
}

func TestBuiltinTableParses(t *testing.T) {
	rows, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 10 {
		t.Fatalf("built-in table has only %d rows", len(rows))
	}
	local := map[string]bool{}
	for _, r := range rows {
		p := r.Price
		if p.Input < 0 || p.Output < 0 || p.CacheWrite < 0 || p.CacheRead < 0 {
			t.Errorf("%s/%s has a negative price: %+v", r.ProviderType, r.Pattern, p)
		}
		if p.Currency != "USD" || p.Source != SourceBuiltin {
			t.Errorf("%s/%s: currency %q source %q", r.ProviderType, r.Pattern, p.Currency, p.Source)
		}
		if r.ProviderType == "ollama" || r.ProviderType == "custom_openai" {
			local[r.ProviderType] = true
			if r.Pattern != "*" || p.Input != 0 || p.Output != 0 {
				t.Errorf("local provider %s must ship a free catch-all, got %s %+v", r.ProviderType, r.Pattern, p)
			}
		}
	}
	// Local models must never report phantom spend.
	for _, pt := range []string{"ollama", "custom_openai"} {
		if !local[pt] {
			t.Errorf("no catch-all row for %s", pt)
		}
	}
	// An absent cache price resolves to the input rate.
	table := NewTable(rows)
	if p, ok := table.Lookup("openai", "gpt-4o-mini-2024-07-18"); !ok || p.CacheWrite != p.Input {
		t.Errorf("gpt-4o-mini cache write = %v, want the input rate %v (ok=%v)", p.CacheWrite, p.Input, ok)
	}
}

// testTable is a hand-built table whose ids fix the tie-breaking order.
func testTable() *Table {
	price := func(in float64) Price {
		return Price{Input: in, Output: in, CacheWrite: in, CacheRead: in, Currency: "USD", Source: SourceBuiltin}
	}
	return NewTable([]Row{
		{ID: 1, ProviderType: "openai", Pattern: "gpt-4o*", Price: price(2.5)},
		{ID: 2, ProviderType: "openai", Pattern: "gpt-4o-mini*", Price: price(0.15)},
		{ID: 3, ProviderType: "openai", Pattern: "gpt-4o-mini-2024-07-18", Price: price(0.01)},
		{ID: 4, ProviderType: "openai", Pattern: "*", Price: price(99)},
		{ID: 5, ProviderType: "custom_openai", Pattern: "deepseek-ai/DeepSeek-V3*", Price: price(0.27)},
		{ID: 6, ProviderType: "custom_openai", Pattern: "*/DeepSeek-R1", Price: price(0.55)},
		// Two patterns of equal specificity: the lower id wins.
		{ID: 8, ProviderType: "ollama", Pattern: "llama*", Price: price(8)},
		{ID: 7, ProviderType: "ollama", Pattern: "llama3*", Price: price(7)},
		{ID: 9, ProviderType: "ollama", Pattern: "llama3*", Price: price(9)},
	})
}

func TestLookupPrecedence(t *testing.T) {
	table := testTable()
	cases := []struct {
		providerType, model string
		want                float64
		found               bool
	}{
		// Exact beats every wildcard.
		{"openai", "gpt-4o-mini-2024-07-18", 0.01, true},
		// Longest literal prefix beats a shorter one.
		{"openai", "gpt-4o-mini-2025-01-01", 0.15, true},
		{"openai", "gpt-4o-2024-11-20", 2.5, true},
		{"openai", "o3-pro", 99, true},
		// "/" is an ordinary character, unlike in path.Match.
		{"custom_openai", "deepseek-ai/DeepSeek-V3-0324", 0.27, true},
		{"custom_openai", "deepseek-ai/DeepSeek-R1", 0.55, true},
		{"custom_openai", "mistral-large", 0, false},
		// Equal prefix length: lowest id.
		{"ollama", "llama3.1:8b", 7, true},
		{"ollama", "llama2", 8, true},
		// A provider type nobody priced.
		{"anthropic", "claude-sonnet-4-5", 0, false},
	}
	for _, c := range cases {
		p, ok := table.Lookup(c.providerType, c.model)
		if ok != c.found || p.Input != c.want {
			t.Errorf("Lookup(%q, %q) = %v/%v, want %v/%v", c.providerType, c.model, p.Input, ok, c.want, c.found)
		}
	}
	// A nil table answers nothing rather than panicking.
	if _, ok := (*Table)(nil).Lookup("openai", "gpt-4o"); ok {
		t.Error("nil table returned a price")
	}
}

func TestMatchGlob(t *testing.T) {
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
		if got := matchGlob(c.pattern, c.s); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestCostMicros(t *testing.T) {
	// Anthropic-shaped: writes at 1.25x input, reads at 0.1x.
	p := Price{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30}
	cases := []struct {
		name                                   string
		prompt, cached, cacheWrite, completion int
		want                                   int64
	}{
		// 1000 uncached x 3 + 500 completion x 15 = 3000 + 7500.
		{name: "no cache", prompt: 1000, completion: 500, want: 10500},
		// 200 uncached x 3 + 300 write x 3.75 + 500 read x 0.30 + 100 x 15.
		{name: "split", prompt: 1000, cached: 500, cacheWrite: 300, completion: 100, want: 600 + 1125 + 150 + 1500},
		// Everything came from the cache: no full-rate part at all.
		{name: "all cached", prompt: 1000, cached: 1000, completion: 0, want: 300},
		// A provider reporting more cached tokens than prompt tokens must
		// not produce a negative uncached share.
		{name: "floor at zero", prompt: 100, cached: 500, completion: 0, want: 150},
		{name: "rounds to nearest", prompt: 1, completion: 0, want: 3},
	}
	for _, c := range cases {
		if got := p.CostMicros(c.prompt, c.cached, c.cacheWrite, c.completion); got != c.want {
			t.Errorf("%s: CostMicros = %d, want %d", c.name, got, c.want)
		}
	}
	// A free local model costs nothing however large the request.
	free := Price{}
	if got := free.CostMicros(1e6, 0, 0, 1e6); got != 0 {
		t.Errorf("free model cost = %d", got)
	}
	// Rounding: 0.15 per Mtok over 7 tokens is 1.05 micros.
	cheap := Price{Input: 0.15}
	if got := cheap.CostMicros(7, 0, 0, 0); got != 1 {
		t.Errorf("rounded cost = %d, want 1", got)
	}
}
