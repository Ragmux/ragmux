package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ragmux/ragmux/internal/pricing"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
)

// ---- model prices ----
//
// The table drives the cost estimate on every request log. Viewers read it,
// editors maintain it. Editing a built-in row turns it into a "user" row,
// which upgrades never overwrite again; deleting a built-in row is refused
// (reset it instead) so it cannot come back at the next upgrade.

// maxPricePerMTok bounds a price so a typo cannot turn one request into an
// astronomical figure on the dashboard. Nothing real comes near it.
const maxPricePerMTok = 100000.0

// priceInput is the create/update body. The cache prices are pointers: an
// omitted one means "charge the input rate", which is not the same as zero.
type priceInput struct {
	ProviderType string   `json:"provider_type"`
	ModelPattern string   `json:"model_pattern"`
	Input        float64  `json:"input_per_mtok"`
	Output       float64  `json:"output_per_mtok"`
	CacheWrite   *float64 `json:"cache_write_per_mtok"`
	CacheRead    *float64 `json:"cache_read_per_mtok"`
	Currency     string   `json:"currency"`
}

// validate checks the body and normalises it. create is false for updates,
// where the provider type and pattern are fixed by the existing row.
func (in *priceInput) validate(create bool) error {
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if len(in.Currency) != 3 {
		return errors.New("currency must be a three-letter code")
	}
	if create {
		in.ProviderType = strings.TrimSpace(in.ProviderType)
		in.ModelPattern = strings.TrimSpace(in.ModelPattern)
		if _, err := provider.New(provider.Config{ProviderType: in.ProviderType}); err != nil {
			return errors.New("provider_type must be one of the supported provider types")
		}
		if in.ModelPattern == "" || len(in.ModelPattern) > 128 {
			return errors.New("model_pattern is required (max 128 characters)")
		}
	}
	for _, v := range []*float64{&in.Input, &in.Output, in.CacheWrite, in.CacheRead} {
		if v == nil {
			continue
		}
		if *v < 0 || *v > maxPricePerMTok {
			return errors.New("prices must be between 0 and 100000 per million tokens")
		}
	}
	return nil
}

func (in priceInput) row() *store.ModelPrice {
	return &store.ModelPrice{ProviderType: in.ProviderType, ModelPattern: in.ModelPattern,
		InputPerMTok: in.Input, OutputPerMTok: in.Output,
		CacheWritePerMTok: in.CacheWrite, CacheReadPerMTok: in.CacheRead, Currency: in.Currency}
}

// priceDetails is the audit payload: enough to see what a price became
// without reading the row back.
func priceDetails(p *store.ModelPrice) map[string]any {
	return map[string]any{"provider_type": p.ProviderType, "model_pattern": p.ModelPattern,
		"input_per_mtok": p.InputPerMTok, "output_per_mtok": p.OutputPerMTok, "source": p.Source}
}

// invalidatePrices drops the gateway's cached table after a mutation so the
// next request is priced with the new numbers instead of waiting out the TTL.
func (a *Admin) invalidatePrices() {
	if a.Prices != nil {
		a.Prices.Invalidate()
	}
}

func (a *Admin) listPrices(w http.ResponseWriter, r *http.Request) {
	list, err := a.Store.ListModelPrices(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	version, err := pricing.BuiltinVersion()
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"prices": list, "builtin_version": version,
		"unit": "per_million_tokens"})
}

func (a *Admin) createPrice(w http.ResponseWriter, r *http.Request) {
	var in priceInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if err := in.validate(true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := a.Store.CreateModelPrice(r.Context(), in.row())
	if err != nil {
		if store.IsUniqueViolation(err) {
			writeErr(w, http.StatusConflict, "a price for that provider type and model pattern already exists")
			return
		}
		a.fail(w, err)
		return
	}
	a.invalidatePrices()
	a.audit(r, "price.create", "price", ptr(p.ID), priceDetails(p))
	writeJSON(w, http.StatusCreated, p)
}

// updatePrice overwrites a row's numbers. The provider type and pattern stay
// as they are: changing them would silently re-target an existing row.
func (a *Admin) updatePrice(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	var in priceInput
	if err := decode(r, &in); err != nil {
		badBody(w, err)
		return
	}
	if err := in.validate(false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	row := in.row()
	row.ID = id
	p, err := a.Store.UpdateModelPrice(r.Context(), row)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.invalidatePrices()
	a.audit(r, "price.update", "price", ptr(p.ID), priceDetails(p))
	writeJSON(w, http.StatusOK, p)
}

func (a *Admin) deletePrice(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	p, err := a.Store.GetModelPrice(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.DeleteModelPrice(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrBuiltinPrice) {
			writeErrCode(w, http.StatusConflict, "builtin_price",
				"built-in prices cannot be deleted; edit the row or reset it to the shipped value")
			return
		}
		a.fail(w, err)
		return
	}
	a.invalidatePrices()
	a.audit(r, "price.delete", "price", ptr(id), priceDetails(p))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// resetPrice restores a row to the shipped numbers and makes it built-in
// again, so upgrades start refreshing it once more. A row that has no
// built-in counterpart (an operator's own model pattern) cannot be reset.
func (a *Admin) resetPrice(w http.ResponseWriter, r *http.Request) {
	id, _ := idParam(r)
	current, err := a.Store.GetModelPrice(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	shipped, ok := pricing.ShippedPrice(current.ProviderType, current.ModelPattern)
	if !ok {
		writeErrCode(w, http.StatusConflict, "no_builtin_price",
			"this price has no built-in value to reset to; edit or delete it instead")
		return
	}
	version, err := pricing.BuiltinVersion()
	if err != nil {
		a.fail(w, err)
		return
	}
	// The cache prices go back as the shipped file spells them, nils
	// included: a reset row must be indistinguishable from a freshly seeded
	// one, and the seed writes an absent cache price as NULL.
	p, err := a.Store.ResetModelPrice(r.Context(), id, &store.ModelPrice{
		InputPerMTok: shipped.Input, OutputPerMTok: shipped.Output,
		CacheWritePerMTok: shipped.CacheWrite, CacheReadPerMTok: shipped.CacheRead,
		Currency: shipped.Currency, BuiltinVersion: version})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.invalidatePrices()
	a.audit(r, "price.reset", "price", ptr(p.ID), priceDetails(p))
	writeJSON(w, http.StatusOK, p)
}
