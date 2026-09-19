package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/pricing"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// priceRow returns the stored row for one provider/pattern pair.
func priceRow(t *testing.T, s *store.Store, providerType, pattern string) *store.ModelPrice {
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

// callReset drives the reset handler the way the router does, with the row
// id in the chi URL parameters.
func callReset(t *testing.T, a *Admin, id int64) (*httptest.ResponseRecorder, *store.ModelPrice) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/prices/"+strconv.FormatInt(id, 10)+"/reset", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatInt(id, 10))
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	a.resetPrice(w, r)
	if w.Code != http.StatusOK {
		return w, nil
	}
	out := &store.ModelPrice{}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("decode reset response %s: %v", w.Body.String(), err)
	}
	return w, out
}

// TestResetPriceKeepsAbsentCachePriceNull pins the contract the seed writes.
// prices.json leaves cache_write out for the OpenAI models, which means "this
// provider charges the input rate"; the seed stores that as NULL and the
// lookup coalesces it to input_per_mtok, so the row keeps following a later
// input price change. A reset that writes the resolved number instead would
// freeze it, and the reset row would silently disagree with its unedited
// siblings after the next release.
func TestResetPriceKeepsAbsentCachePriceNull(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	if _, err := pricing.Seed(ctx, s.DB(), quietLog()); err != nil {
		t.Fatal(err)
	}
	a := &Admin{Store: s, Log: quietLog()}

	row := priceRow(t, s, "openai", "gpt-5*")
	if row.CacheWritePerMTok != nil {
		t.Fatalf("seed stored cache_write %v, want NULL", *row.CacheWritePerMTok)
	}
	shippedInput := row.InputPerMTok

	row.InputPerMTok, row.OutputPerMTok = 42, 43
	cacheWrite := 7.5
	row.CacheWritePerMTok = &cacheWrite
	edited, err := s.UpdateModelPrice(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Source != store.PriceSourceUser {
		t.Fatalf("edited row source = %q", edited.Source)
	}

	w, got := callReset(t, a, edited.ID)
	if got == nil {
		t.Fatalf("reset returned %d: %s", w.Code, w.Body.String())
	}
	if got.CacheWritePerMTok != nil {
		t.Errorf("reset response cache_write = %v, want null", *got.CacheWritePerMTok)
	}

	back := priceRow(t, s, "openai", "gpt-5*")
	if back.Source != store.PriceSourceBuiltin || back.InputPerMTok != shippedInput {
		t.Errorf("reset row = %+v, want a builtin row at input %v", back, shippedInput)
	}
	if back.CacheWritePerMTok != nil {
		t.Errorf("reset wrote %v into cache_write_per_mtok; a freshly seeded row has NULL there", *back.CacheWritePerMTok)
	}

	// Seed → reset → seed: the cycle leaves the row indistinguishable from
	// one that was never edited, and the lookup still falls back to input.
	if _, err := pricing.Seed(ctx, s.DB(), quietLog()); err != nil {
		t.Fatal(err)
	}
	after := priceRow(t, s, "openai", "gpt-5*")
	if after.CacheWritePerMTok != nil || after.InputPerMTok != shippedInput {
		t.Errorf("row after re-seed = %+v", after)
	}
	p, ok := pricing.NewCache(s.DB(), quietLog()).Lookup(ctx, "openai", "gpt-5")
	if !ok || p.CacheWrite != p.Input || p.Input != shippedInput {
		t.Errorf("lookup = %+v (ok=%v), want cache write to follow input %v", p, ok, shippedInput)
	}
}

// TestResetPriceRejectsRowsWithNoShippedCounterpart keeps the 409 path honest
// now that custom_openai ships no row at all.
func TestResetPriceRejectsRowsWithNoShippedCounterpart(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	if _, err := pricing.Seed(ctx, s.DB(), quietLog()); err != nil {
		t.Fatal(err)
	}
	own, err := s.CreateModelPrice(ctx, &store.ModelPrice{ProviderType: "custom_openai",
		ModelPattern: "*", InputPerMTok: 1, OutputPerMTok: 2, Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	a := &Admin{Store: s, Log: quietLog()}
	w, _ := callReset(t, a, own.ID)
	if w.Code != http.StatusConflict {
		t.Errorf("reset of an operator's own row = %d %s, want 409", w.Code, w.Body.String())
	}
}
