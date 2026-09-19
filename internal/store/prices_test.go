package store_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/ragmux/ragmux/internal/pricing"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// findPrice returns the row for one provider/pattern pair.
func findPrice(t *testing.T, s *store.Store, providerType, pattern string) *store.ModelPrice {
	t.Helper()
	list, err := s.ListModelPrices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.ProviderType == providerType && p.ModelPattern == pattern {
			return p
		}
	}
	t.Fatalf("no price row for %s/%s", providerType, pattern)
	return nil
}

// bumpBuiltinVersion rewrites one row's numbers as if an older release had
// seeded it, so the next Seed sees a version it must refresh.
func seedAsOlderVersion(t *testing.T, s *store.Store, id int64, version int) {
	t.Helper()
	_, err := s.DB().Exec(context.Background(),
		"UPDATE model_prices SET builtin_version = $2, input_per_mtok = 999 WHERE id = $1", id, version)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSeedPricesIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)

	first, err := pricing.Seed(ctx, s.DB(), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	builtin, err := pricing.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if int(first) != len(builtin) {
		t.Errorf("first seed wrote %d rows, want %d", first, len(builtin))
	}
	// A second run at the same version changes nothing.
	again, err := pricing.Seed(ctx, s.DB(), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("re-seeding at the same version touched %d rows", again)
	}
	list, err := s.ListModelPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(builtin) {
		t.Errorf("table has %d rows after two seeds, want %d", len(list), len(builtin))
	}
	// Absent cache prices stay NULL so the lookup can fall back to input.
	local := findPrice(t, s, "ollama", "*")
	if local.Source != store.PriceSourceBuiltin || local.CacheWritePerMTok != nil || local.InputPerMTok != 0 {
		t.Errorf("ollama catch-all = %+v", local)
	}
}

func TestSeedRefreshesOnlyOutdatedBuiltinRows(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	if _, err := pricing.Seed(ctx, s.DB(), quietLog()); err != nil {
		t.Fatal(err)
	}
	version, err := pricing.BuiltinVersion()
	if err != nil {
		t.Fatal(err)
	}

	// One row an operator edited, one built-in row left from an older
	// release. The next upgrade must refresh only the second.
	edited := findPrice(t, s, "openai", "gpt-4o-mini*")
	edited.InputPerMTok, edited.OutputPerMTok = 42, 43
	if _, err := s.UpdateModelPrice(ctx, edited); err != nil {
		t.Fatal(err)
	}
	stale := findPrice(t, s, "openai", "gpt-4o*")
	seedAsOlderVersion(t, s, stale.ID, version-1)

	n, err := pricing.Seed(ctx, s.DB(), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("seed refreshed %d rows, want only the outdated built-in one", n)
	}
	if got := findPrice(t, s, "openai", "gpt-4o-mini*"); got.Source != store.PriceSourceUser || got.InputPerMTok != 42 {
		t.Errorf("user row was overwritten: %+v", got)
	}
	if got := findPrice(t, s, "openai", "gpt-4o*"); got.InputPerMTok == 999 || got.BuiltinVersion != version {
		t.Errorf("outdated built-in row was not refreshed: %+v", got)
	}
}

func TestModelPriceCRUDAndBuiltinProtection(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	if _, err := pricing.Seed(ctx, s.DB(), quietLog()); err != nil {
		t.Fatal(err)
	}

	// Built-in rows are never deleted, only edited or reset.
	builtin := findPrice(t, s, "anthropic", "claude-sonnet-4-5*")
	if err := s.DeleteModelPrice(ctx, builtin.ID); !errors.Is(err, store.ErrBuiltinPrice) {
		t.Fatalf("deleting a built-in row: %v, want ErrBuiltinPrice", err)
	}
	original := *builtin
	builtin.InputPerMTok = 1.5
	edited, err := s.UpdateModelPrice(ctx, builtin)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Source != store.PriceSourceUser || edited.InputPerMTok != 1.5 {
		t.Errorf("edited row = %+v", edited)
	}
	// Reset puts the shipped numbers back and makes it built-in again.
	back, err := s.ResetModelPrice(ctx, edited.ID, &original)
	if err != nil {
		t.Fatal(err)
	}
	if back.Source != store.PriceSourceBuiltin || back.InputPerMTok != original.InputPerMTok {
		t.Errorf("reset row = %+v, want %+v", back, original)
	}

	// An operator's own row can be created and deleted.
	own, err := s.CreateModelPrice(ctx, &store.ModelPrice{ProviderType: "custom_openai",
		ModelPattern: "deepseek-ai/DeepSeek-V3*", InputPerMTok: 0.27, OutputPerMTok: 1.1, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if own.Source != store.PriceSourceUser {
		t.Errorf("created row source = %q", own.Source)
	}
	if _, err := s.CreateModelPrice(ctx, own); !store.IsUniqueViolation(err) {
		t.Errorf("duplicate pattern: %v", err)
	}
	if err := s.DeleteModelPrice(ctx, own.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetModelPrice(ctx, own.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted row still readable: %v", err)
	}
	if err := s.DeleteModelPrice(ctx, own.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting a missing row: %v", err)
	}
}

func TestPriceCacheServesEditsAfterInvalidate(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	if _, err := pricing.Seed(ctx, s.DB(), quietLog()); err != nil {
		t.Fatal(err)
	}
	cache := pricing.NewCache(s.DB(), quietLog())

	p, ok := cache.Lookup(ctx, "anthropic", "claude-sonnet-4-5-20250929")
	if !ok || p.Input != 3 || p.CacheRead != 0.30 || p.Source != pricing.SourceBuiltin {
		t.Fatalf("seeded lookup = %+v (ok=%v)", p, ok)
	}
	if _, ok := cache.Lookup(ctx, "anthropic", "some-unpriced-model"); ok {
		t.Error("an unpriced model resolved to a price")
	}
	// A local connection is priced at zero rather than left unpriced, so it
	// never reports phantom spend and never shows as "none" either.
	if p, ok := cache.Lookup(ctx, "ollama", "llama3.1:8b"); !ok || p.Input != 0 {
		t.Errorf("ollama lookup = %+v (ok=%v)", p, ok)
	}

	row := findPrice(t, s, "anthropic", "claude-sonnet-4-5*")
	row.InputPerMTok = 9
	if _, err := s.UpdateModelPrice(ctx, row); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate()
	if p, _ := cache.Lookup(ctx, "anthropic", "claude-sonnet-4-5-20250929"); p.Input != 9 || p.Source != pricing.SourceUser {
		t.Errorf("after invalidate = %+v, want the edited user price", p)
	}
}

// insertPriceRow writes a row straight into the table, the way an older
// release's seed would have left it.
func insertPriceRow(t *testing.T, s *store.Store, providerType, pattern, source string, version int) {
	t.Helper()
	_, err := s.DB().Exec(context.Background(), `INSERT INTO model_prices
		(provider_type, model_pattern, input_per_mtok, output_per_mtok, currency, source, builtin_version)
		VALUES ($1, $2, 1, 2, 'USD', $3, $4)`, providerType, pattern, source, version)
	if err != nil {
		t.Fatal(err)
	}
}

// TestSeedNeverRemovesRows pins the seed's half of the upgrade contract. It
// inserts and refreshes and does nothing else: a row the shipped table
// stopped listing stays until a numbered migration removes it. The general
// rule — delete whatever prices.json no longer lists — was considered and
// rejected, because a pattern renamed in some later release would then drop
// that model's price across every install and send it to cost_source "none"
// with no migration to read and nothing in the release notes.
func TestSeedNeverRemovesRows(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	if _, err := pricing.Seed(ctx, s.DB(), quietLog()); err != nil {
		t.Fatal(err)
	}
	version, err := pricing.BuiltinVersion()
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := pricing.Builtin()
	if err != nil {
		t.Fatal(err)
	}

	// Rows the shipped table does not list, as an older release would have
	// left them: one still builtin, one the operator made theirs.
	insertPriceRow(t, s, "custom_openai", "*", store.PriceSourceBuiltin, version-1)
	insertPriceRow(t, s, "openai", "dropped-in-a-later-release*", store.PriceSourceUser, version-1)

	if _, err := pricing.Seed(ctx, s.DB(), quietLog()); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListModelPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, p := range list {
		have[p.ProviderType+"/"+p.ModelPattern] = true
	}
	if !have["custom_openai/*"] || !have["openai/dropped-in-a-later-release*"] {
		t.Error("the seed removed a row; retiring one is a migration's job, not the seed's")
	}
	for _, r := range shipped {
		if !have[r.ProviderType+"/"+r.Pattern] {
			t.Errorf("shipped row %s/%s went missing", r.ProviderType, r.Pattern)
		}
	}
	if len(list) != len(shipped)+2 {
		t.Errorf("table has %d rows, want the %d shipped plus the two inserted", len(list), len(shipped))
	}
}

// TestMigrationRetiredTheCustomOpenAICatchAll: 0015 removes the row on an
// install that already had it seeded, and leaves one the operator edited.
func TestMigrationRetiredTheCustomOpenAICatchAll(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	// The migration has already run on this fresh schema, so re-run its
	// statement against rows planted the way an older install holds them.
	insertPriceRow(t, s, "custom_openai", "*", store.PriceSourceBuiltin, 1)
	insertPriceRow(t, s, "custom_openai", "mine*", store.PriceSourceUser, 1)
	// go test runs with the package directory as the working directory, so
	// this reads the very file that ships in the embedded migration set.
	sql, err := os.ReadFile(filepath.Join("migrations", "0015_drop_custom_openai_catch_all_price.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListModelPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]string{}
	for _, p := range list {
		have[p.ProviderType+"/"+p.ModelPattern] = p.Source
	}
	if _, ok := have["custom_openai/*"]; ok {
		t.Error("the seeded custom_openai catch-all survived the migration")
	}
	if have["custom_openai/mine*"] != store.PriceSourceUser {
		t.Error("the migration removed a row the operator owns")
	}
}

// TestNoPriceForAPaidCustomOpenAI is what the dropped catch-all was hiding:
// custom_openai points at vLLM as readily as at a paid API, so an unpriced
// model must be reported as unpriced rather than billed at zero.
func TestNoPriceForAPaidCustomOpenAI(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	if _, err := pricing.Seed(ctx, s.DB(), quietLog()); err != nil {
		t.Fatal(err)
	}
	cache := pricing.NewCache(s.DB(), quietLog())
	if p, ok := cache.Lookup(ctx, "custom_openai", "deepseek-ai/DeepSeek-V3"); ok {
		t.Errorf("custom_openai priced out of the box: %+v", p)
	}
	// Ollama keeps its free catch-all: it runs on the operator's hardware.
	if p, ok := cache.Lookup(ctx, "ollama", "llama3.1:8b"); !ok || p.Input != 0 || p.Output != 0 {
		t.Errorf("ollama lookup = %+v (ok=%v), want a free catch-all", p, ok)
	}
	// An operator pointing custom_openai at a paid API adds their own row.
	if _, err := s.CreateModelPrice(ctx, &store.ModelPrice{ProviderType: "custom_openai",
		ModelPattern: "deepseek-ai/DeepSeek-V3*", InputPerMTok: 0.27, OutputPerMTok: 1.1, Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate()
	if p, ok := cache.Lookup(ctx, "custom_openai", "deepseek-ai/DeepSeek-V3"); !ok || p.Input != 0.27 || p.Source != pricing.SourceUser {
		t.Errorf("after adding a row: %+v (ok=%v)", p, ok)
	}
}
