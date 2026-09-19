package pricing

import (
	_ "embed"
	"sync"
)

// builtinJSON is the shipped price table. It is JSON rather than YAML
// because encoding/json is in the standard library and Ragmux adds no
// module dependency for a file it reads once at start.
//
//go:embed prices.json
var builtinJSON []byte

var (
	builtinOnce sync.Once
	builtinData *builtinFile
	builtinErr  error
)

func loadBuiltin() (*builtinFile, error) {
	builtinOnce.Do(func() { builtinData, builtinErr = parseBuiltin(builtinJSON) })
	return builtinData, builtinErr
}

// BuiltinBytes returns the raw embedded table, for the test that pins its
// checksum to the version number.
func BuiltinBytes() []byte { return builtinJSON }

// BuiltinVersion is the version of the shipped table. A seeded builtin row
// is only refreshed when this number grows, so a price change is not picked
// up until someone bumps it deliberately.
func BuiltinVersion() (int, error) {
	f, err := loadBuiltin()
	if err != nil {
		return 0, err
	}
	return f.Version, nil
}

// Builtin returns the shipped price rows. Their ids are their position in
// the file, which only matters for breaking precedence ties among patterns
// of equal specificity.
func Builtin() ([]Row, error) {
	f, err := loadBuiltin()
	if err != nil {
		return nil, err
	}
	rows := make([]Row, len(f.Models))
	for i, m := range f.Models {
		rows[i] = Row{ID: int64(i + 1), ProviderType: m.ProviderType, Pattern: m.Model, Price: m.resolve(f.Currency)}
	}
	return rows, nil
}

// Shipped is one entry of the built-in table as prices.json spells it,
// before resolve() fills the fallbacks in. CacheWrite and CacheRead stay nil
// when the file leaves them out, because that is how Seed stores them: NULL
// means "this provider charges the input rate" and keeps following
// input_per_mtok, while a number copied from the input rate freezes.
type Shipped struct {
	Input      float64
	Output     float64
	CacheWrite *float64
	CacheRead  *float64
	Currency   string
}

// ShippedPrice returns the unresolved shipped entry for one exact
// provider/pattern pair. Resetting an edited row restores it from here: a
// reset must write the same NULLs the seed wrote, or the reset row stops
// tracking a later input price change while its unedited siblings follow it.
func ShippedPrice(providerType, pattern string) (Shipped, bool) {
	f, err := loadBuiltin()
	if err != nil {
		return Shipped{}, false
	}
	for _, m := range f.Models {
		if m.ProviderType != providerType || m.Model != pattern {
			continue
		}
		// The pointers are copied rather than shared: builtinData is parsed
		// once and handed to every caller.
		return Shipped{Input: m.Input, Output: m.Output,
			CacheWrite: copyFloat(m.CacheWrite), CacheRead: copyFloat(m.CacheRead),
			Currency: f.Currency}, true
	}
	return Shipped{}, false
}

func copyFloat(v *float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
