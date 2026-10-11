package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"
)

// pricesSHA256 pins every released version of the embedded table to its
// digest. The map is append-only: a released version's digest is a fact
// about what shipped, so the way to record a price change is to add an
// entry, never to edit one.
//
// That is what forces the version bump. Seed refreshes a seeded builtin row
// only when "version" grows, so a price edited without a bump leaves every
// existing install on the old number. Editing the digest in place used to
// make that green; now the version the file carries is already pinned to a
// different digest and the test says so.
var pricesSHA256 = map[int]string{
	1: "45c8ce28eca57e738c77666c9dfbb336febe84c3dae031f2ac091da46c3c24af",
	2: "317856df091340f07a0c632c829fd2dee56dc4bc473b5b277d7abd4721054948",
	3: "920637082e48334bee70488eef521330d9ec09bef0fbbc0af203ad0ea6142731",
}

func TestPricesVersionPinned(t *testing.T) {
	v, err := BuiltinVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v < 1 {
		t.Fatalf("builtin version = %d", v)
	}
	sum := sha256.Sum256(BuiltinBytes())
	got := hex.EncodeToString(sum[:])
	want, pinned := pricesSHA256[v]
	switch {
	case pinned && want != got:
		t.Fatalf("prices.json changed but still says \"version\": %d, which is pinned to %s (now %s)\n"+
			"bump \"version\" — seeded builtin rows only refresh when it grows — and add the new digest to pricesSHA256",
			v, want, got)
	case !pinned:
		t.Fatalf("prices.json version %d is not pinned\nadd %d: %q to pricesSHA256", v, v, got)
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
	local := false
	for _, r := range rows {
		p := r.Price
		if p.Input < 0 || p.Output < 0 || p.CacheWrite < 0 || p.CacheRead < 0 {
			t.Errorf("%s/%s has a negative price: %+v", r.ProviderType, r.Pattern, p)
		}
		if p.Currency != "USD" || p.Source != SourceBuiltin {
			t.Errorf("%s/%s: currency %q source %q", r.ProviderType, r.Pattern, p.Currency, p.Source)
		}
		// custom_openai is a URL, not a place: it points at vLLM as readily
		// as at a paid API. A catch-all would report a real bill as $0.00
		// with cost_source "builtin", which reads as a priced zero rather
		// than the missing price it is.
		if r.ProviderType == "custom_openai" {
			t.Errorf("custom_openai must ship no price row, got %s %+v", r.Pattern, p)
		}
		if r.ProviderType == "ollama" {
			local = true
			if r.Pattern != "*" || p.Input != 0 || p.Output != 0 {
				t.Errorf("ollama must ship a free catch-all, got %s %+v", r.Pattern, p)
			}
		}
	}
	// Ollama runs on the operator's own hardware: it must never report
	// phantom spend, and there is no endpoint where it could cost money.
	if !local {
		t.Error("no catch-all row for ollama")
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

// TestCostMicrosSaturates pins the two ends of the range. A cost is never
// negative, whatever a broken or hostile upstream reported, and it never
// leaves int64: int64(math.Round(x)) is implementation-defined outside the
// range and arm64 and amd64 do not agree on the answer.
func TestCostMicrosSaturates(t *testing.T) {
	p := Price{Input: 3, Output: 15, CacheWrite: 3.75, CacheRead: 0.30}
	huge := Price{Input: 1e18, Output: 1e18, CacheWrite: 1e18, CacheRead: 1e18}
	cases := []struct {
		name                                   string
		price                                  Price
		prompt, cached, cacheWrite, completion int
		want                                   int64
	}{
		{name: "negative completion", price: p, prompt: 100, completion: -100, want: 300},
		{name: "negative prompt", price: p, prompt: -100, completion: 10, want: 150},
		{name: "everything negative", price: p, prompt: -1, cached: -1, cacheWrite: -1, completion: -1, want: 0},
		{name: "negative cached share", price: p, prompt: 100, cached: -50, completion: 0, want: 300},
		// A price row someone typed a minus into cannot make a credit.
		{name: "negative price", price: Price{Input: -3, Output: -15}, prompt: 1000, completion: 500, want: 0},
		// One negative rate is floored on its own: it prices nothing, and it
		// does not discount the terms priced beside it. 500 x 15 stands.
		{name: "one negative rate", price: Price{Input: -3, Output: 15}, prompt: 1000, completion: 500, want: 7500},
		{name: "negative cache rates", price: Price{Input: 3, Output: 15, CacheWrite: -100, CacheRead: -100},
			prompt: 1000, cached: 300, cacheWrite: 200, completion: 100, want: 1500 + 1500},
		// Beyond int64 micros: saturate rather than wrap or go platform-specific.
		{name: "overflow on input", price: huge, prompt: 1 << 40, want: math.MaxInt64},
		{name: "overflow on output", price: huge, completion: 1 << 40, want: math.MaxInt64},
		{name: "overflow just under", price: Price{Input: 1}, prompt: 1 << 40, want: 1 << 40},
	}
	for _, c := range cases {
		got := c.price.CostMicros(c.prompt, c.cached, c.cacheWrite, c.completion)
		if got != c.want {
			t.Errorf("%s: CostMicros = %d, want %d", c.name, got, c.want)
		}
		if got < 0 {
			t.Errorf("%s: CostMicros returned a negative cost", c.name)
		}
	}
	// A NaN in the table prices nothing rather than poisoning the total.
	if got := (Price{Input: math.NaN()}).CostMicros(10, 0, 0, 0); got != 0 {
		t.Errorf("NaN price cost = %d, want 0", got)
	}
	if got := (Price{Input: math.Inf(1)}).CostMicros(10, 0, 0, 0); got != math.MaxInt64 {
		t.Errorf("infinite price cost = %d, want saturation", got)
	}
}

func TestSearchUnitMicros(t *testing.T) {
	cases := []struct {
		name  string
		price Price
		units int
		want  int64
	}{
		{name: "cohere list price", price: Price{PerSearchUnit: 0.002}, units: 1, want: 2000},
		{name: "several units", price: Price{PerSearchUnit: 0.002}, units: 3, want: 6000},
		{name: "no search unit rate", price: Price{Input: 3, Output: 15}, units: 5, want: 0},
		{name: "zero units", price: Price{PerSearchUnit: 0.002}, units: 0, want: 0},
		{name: "negative units", price: Price{PerSearchUnit: 0.002}, units: -4, want: 0},
		{name: "negative rate", price: Price{PerSearchUnit: -0.002}, units: 2, want: 0},
		{name: "NaN rate", price: Price{PerSearchUnit: math.NaN()}, units: 2, want: 0},
		// 0.0000004 x 1e6 = 0.4 micros: rounded, not floored into a free call.
		{name: "rounds to nearest micro", price: Price{PerSearchUnit: 0.0000006}, units: 1, want: 1},
		{name: "sub-micro rounds down", price: Price{PerSearchUnit: 0.0000004}, units: 1, want: 0},
		{name: "overflow saturates", price: Price{PerSearchUnit: 1e18}, units: 1 << 40, want: math.MaxInt64},
	}
	for _, c := range cases {
		got := c.price.SearchUnitMicros(c.units)
		if got != c.want {
			t.Errorf("%s: SearchUnitMicros = %d, want %d", c.name, got, c.want)
		}
		if got < 0 {
			t.Errorf("%s: SearchUnitMicros returned a negative cost", c.name)
		}
	}
}

// A row with a search unit rate prices a call that reports search units by
// those units alone (ADR-021); token prices on the same row are never added.
func TestBilledMicros(t *testing.T) {
	cohere := Price{Input: 5, Output: 7, PerSearchUnit: 0.002, HasSearchUnit: true}
	cases := []struct {
		name  string
		price Price
		units int
		want  int64
	}{
		{name: "units alone when the row has a rate", price: cohere, units: 3, want: 6000},
		{name: "tokens are not added", price: cohere, units: 1, want: 2000},
		{name: "no units falls back to tokens", price: cohere, units: 0, want: 100*5 + 10*7},
		{name: "unset rate falls back to tokens", price: Price{Input: 5, Output: 7}, units: 3, want: 100*5 + 10*7},
		// An explicit 0 is a rate that is set: the call is free, not billed by tokens.
		{name: "explicit zero rate bills units alone", price: Price{Input: 5, Output: 7, HasSearchUnit: true}, units: 3, want: 0},
		{name: "explicit zero rate with no units uses tokens", price: Price{Input: 5, Output: 7, HasSearchUnit: true}, units: 0, want: 100*5 + 10*7},
		{name: "unset rate ignores a stray value", price: Price{Input: 5, Output: 7, PerSearchUnit: 0.002}, units: 3, want: 100*5 + 10*7},
		{name: "NaN rate that is set is floored to zero", price: Price{Input: 5, Output: 7, PerSearchUnit: math.NaN(), HasSearchUnit: true}, units: 3, want: 0},
	}
	for _, c := range cases {
		if got := c.price.BilledMicros(100, 0, 0, 10, c.units); got != c.want {
			t.Errorf("%s: BilledMicros = %d, want %d", c.name, got, c.want)
		}
	}
}

// The shipped table prices the rerank APIs the way each one bills: Cohere per
// search unit, Voyage per token.
func TestBuiltinRerankPrices(t *testing.T) {
	rows, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	table := NewTable(rows)
	for _, model := range []string{"rerank-v3.5", "rerank-english-v3.0", "rerank-multilingual-v3.0"} {
		p, ok := table.Lookup("cohere_rerank", model)
		if !ok || p.PerSearchUnit <= 0 {
			t.Errorf("cohere_rerank/%s: price %+v (ok=%v), want a per_search_unit rate", model, p, ok)
		}
	}
	for _, model := range []string{"rerank-2.5", "rerank-2.5-lite", "rerank-2", "rerank-2-lite"} {
		p, ok := table.Lookup("voyage_rerank", model)
		if !ok || p.Input <= 0 || p.PerSearchUnit != 0 {
			t.Errorf("voyage_rerank/%s: price %+v (ok=%v), want a token price and no search unit rate", model, p, ok)
		}
	}
	// The lite model must not fall into the broader rerank-2.5* row.
	lite, _ := table.Lookup("voyage_rerank", "rerank-2.5-lite")
	full, _ := table.Lookup("voyage_rerank", "rerank-2.5")
	if lite.Input >= full.Input {
		t.Errorf("rerank-2.5-lite input %v is not below rerank-2.5 %v", lite.Input, full.Input)
	}
}
