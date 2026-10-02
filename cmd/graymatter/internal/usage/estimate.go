package usage

import (
	"fmt"
	"io"
	"math/big"
	"strings"
	"time"
)

// PriceBook is supplied explicitly by the user with dated provenance. There is
// no guessed model pricing and no conversion from subscription tokens to spend.
type PriceBook struct {
	Provider    string              `json:"provider"`
	Model       string              `json:"model"`
	Currency    string              `json:"currency"`
	Source      string              `json:"source"`
	EffectiveAt time.Time           `json:"effective_at"`
	Rates       map[string]UnitRate `json:"rates"`
}
type UnitRate struct {
	Amount   string `json:"amount"`
	PerUnits int64  `json:"per_units"`
}

func DecodePriceBook(r io.Reader) (PriceBook, error) {
	var p PriceBook
	e := decodeJSON(r, &p, 1<<20, true)
	return p, e
}
func Estimate(e UsageEvent, p PriceBook) (CostObservation, error) {
	c := CostObservation{}
	if p.Provider != e.Provider || p.Model != e.Model || p.Source == "" || p.EffectiveAt.IsZero() || e.Time.Before(p.EffectiveAt) {
		return c, fmt.Errorf("price book provider, model, effective date or provenance does not match event")
	}
	total := new(big.Rat)
	partial := false
	priced := false
	for kind, n := range e.Quantities {
		if n < 0 {
			return c, fmt.Errorf("negative usage quantity")
		}
		rate, ok := p.Rates[kind]
		if !ok {
			if n > 0 && kind != "requests" {
				partial = true
			}
			continue
		}
		if rate.PerUnits <= 0 {
			return c, fmt.Errorf("rate per_units must be positive")
		}
		v, err := decimal(rate.Amount)
		if err != nil {
			return c, err
		}
		if v.Sign() < 0 {
			return c, fmt.Errorf("unit price must be nonnegative")
		}
		priced = true
		v.Mul(v, new(big.Rat).SetInt64(n))
		v.Quo(v, new(big.Rat).SetInt64(rate.PerUnits))
		total.Add(total, v)
	}
	if !priced {
		return c, fmt.Errorf("no event quantity has a configured price")
	}
	c = CostObservation{Provider: e.Provider, AccountID: e.AccountID, Amount: decimalString(total), Currency: strings.ToUpper(p.Currency), Kind: "estimate", Scope: "request:" + e.RequestID, Source: p.Source, StartAt: e.Time, EndAt: e.Time.Add(time.Nanosecond), ObservedAt: time.Now().UTC(), Partial: partial}
	s := Snapshot{Costs: []CostObservation{c}}
	if err := s.Normalize(); err != nil {
		return c, err
	}
	return s.Costs[0], nil
}
