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

// BuiltinPrice returns the shipped price for one exact provider/pattern
// pair. Resetting an edited row restores it from here.
func BuiltinPrice(providerType, pattern string) (Price, bool) {
	rows, err := Builtin()
	if err != nil {
		return Price{}, false
	}
	for _, r := range rows {
		if r.ProviderType == providerType && r.Pattern == pattern {
			return r.Price, true
		}
	}
	return Price{}, false
}
