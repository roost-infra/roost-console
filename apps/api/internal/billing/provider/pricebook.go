package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Pricebook is the on-disk shape of the provider + retail price input. It is
// the bridge between the pricing workbook and the pure Calculator.
type Pricebook struct {
	Version string       `json:"version"`
	Plan    Plan         `json:"plan"`
	Models  []ModelPrice `json:"models"`
}

// ModelPrice is one model/tier row. Provider and Retail are micro-USD per 1M
// tokens; Official is used only when Plan.Basis is BasisOfficial.
type ModelPrice struct {
	Model    string `json:"model"`
	Tier     string `json:"tier"`
	MaxInput int64  `json:"max_input"`
	Peak     string `json:"peak"`
	Free     bool   `json:"free,omitempty"`
	Provider Price  `json:"provider"`
	Official Price  `json:"official"`
	Retail   Price  `json:"retail"`
}

// LoadPricebook decodes and validates a pricebook document.
//
// Duplicate model/max_input rows are rejected: a tier ladder must have one
// price per ceiling. Peak rows are intentionally not loaded until the request
// carries a peak window; they are a separate dimension.
func LoadPricebook(r io.Reader) (Pricebook, error) {
	var doc Pricebook
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Pricebook{}, fmt.Errorf("provider: decode pricebook: %w", err)
	}
	if strings.TrimSpace(doc.Version) == "" {
		return Pricebook{}, fmt.Errorf("%w: version is required", ErrInvalidQuote)
	}
	if doc.Plan.Basis == "" {
		doc.Plan.Basis = BasisProvider
	}
	switch doc.Plan.Basis {
	case BasisProvider, BasisOfficial:
	default:
		return Pricebook{}, fmt.Errorf("%w: unknown plan basis %q", ErrInvalidQuote, doc.Plan.Basis)
	}
	if doc.Plan.CostMicros < 0 || doc.Plan.AllowanceMicros < 0 {
		return Pricebook{}, fmt.Errorf("%w: plan cost/allowance must be non-negative", ErrInvalidQuote)
	}
	seen := map[string]map[int64]bool{}
	for i := range doc.Models {
		m := &doc.Models[i]
		if strings.TrimSpace(m.Model) == "" {
			return Pricebook{}, fmt.Errorf("%w: model at row %d is empty", ErrInvalidQuote, i+1)
		}
		if m.MaxInput < 0 {
			return Pricebook{}, fmt.Errorf("%w: negative tier ceiling for %s", ErrInvalidQuote, m.Model)
		}
		if !m.Free && m.Provider.Input <= 0 && m.Provider.Output <= 0 {
			return Pricebook{}, fmt.Errorf("%w: provider price missing for %s/%s", ErrInvalidQuote, m.Model, m.Tier)
		}
		if !m.Free && m.Retail.Input <= 0 && m.Retail.Output <= 0 {
			return Pricebook{}, fmt.Errorf("%w: retail price missing for %s/%s", ErrInvalidQuote, m.Model, m.Tier)
		}
		if seen[m.Model] == nil {
			seen[m.Model] = map[int64]bool{}
		}
		if seen[m.Model][m.MaxInput] {
			return Pricebook{}, fmt.Errorf("%w: duplicate tier %d for %s", ErrInvalidQuote, m.MaxInput, m.Model)
		}
		seen[m.Model][m.MaxInput] = true
	}
	return doc, nil
}

// Calculator builds the pure calculator from a validated pricebook. Tiers are
// sorted by ceiling so callers can inspect the resulting ladders.
func (p Pricebook) Calculator() Calculator {
	retail := Book{}
	prov := Book{}
	official := Book{}
	for _, row := range p.Models {
		retail[row.Model] = append(retail[row.Model], Tier{MaxInput: row.MaxInput, Price: row.Retail})
		prov[row.Model] = append(prov[row.Model], Tier{MaxInput: row.MaxInput, Price: row.Provider})
		if row.Official.Input > 0 || row.Official.Output > 0 {
			official[row.Model] = append(official[row.Model], Tier{MaxInput: row.MaxInput, Price: row.Official})
		}
	}
	for _, m := range []Book{retail, prov, official} {
		for model := range m {
			sort.SliceStable(m[model], func(i, j int) bool {
				a, b := m[model][i].MaxInput, m[model][j].MaxInput
				if a == 0 {
					return false
				}
				if b == 0 {
					return true
				}
				return a < b
			})
		}
	}
	return Calculator{Retail: retail, Provider: prov, Official: official, Plan: p.Plan}
}
