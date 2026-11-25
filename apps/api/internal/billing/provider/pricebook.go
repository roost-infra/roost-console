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
	Images  []UnitPrice  `json:"images,omitempty"`
	Videos  []UnitPrice  `json:"videos,omitempty"`
}

// UnitPrice is a per-call (image) or per-second (video) price.
type UnitPrice struct {
	Model          string `json:"model"`
	Tier           string `json:"tier,omitempty"`
	ProviderMicros int64  `json:"provider_micros"`
	RetailMicros   int64  `json:"retail_micros"`
}

// UnitQuote is the retail/cost/margin view for one image call or video second.
type UnitQuote struct {
	Model         string
	Tier          string
	ProviderMinor int64
	RetailMinor   int64
	GrossMinor    int64
	MarginBPS     int64
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
// Duplicate (model, max_input, peak) rows are rejected. Peak rows are loaded
// as a separate tier dimension and selected by Quote.Request.Peak.
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
	seen := map[string]map[string]bool{}
	for i := range doc.Models {
		m := &doc.Models[i]
		if strings.TrimSpace(m.Peak) == "—" {
			m.Peak = ""
		}
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
			seen[m.Model] = map[string]bool{}
		}
		key := fmt.Sprintf("%d|%s", m.MaxInput, m.Peak)
		if seen[m.Model][key] {
			return Pricebook{}, fmt.Errorf("%w: duplicate tier %d/%s for %s", ErrInvalidQuote, m.MaxInput, m.Peak, m.Model)
		}
		seen[m.Model][key] = true
	}
	seenUnits := map[string]bool{}
	for i, u := range append(append([]UnitPrice{}, doc.Images...), doc.Videos...) {
		if strings.TrimSpace(u.Model) == "" {
			return Pricebook{}, fmt.Errorf("%w: unit price row %d has no model", ErrInvalidQuote, i+1)
		}
		if u.ProviderMicros < 0 || u.RetailMicros < 0 {
			return Pricebook{}, fmt.Errorf("%w: negative unit price for %s", ErrInvalidQuote, u.Model)
		}
		kind := "image"
		if i >= len(doc.Images) {
			kind = "video"
		}
		key := kind + "|" + u.Model + "|" + u.Tier
		if seenUnits[key] {
			return Pricebook{}, fmt.Errorf("%w: duplicate unit price %s", ErrInvalidQuote, key)
		}
		seenUnits[key] = true
	}
	return doc, nil
}

// QuoteUnit prices one image call (kind "image") or video second (kind
// "video"). Tier is matched for video; images ignore it.
func (p Pricebook) QuoteUnit(kind, model, tier string) (UnitQuote, error) {
	var list []UnitPrice
	switch kind {
	case "image":
		list = p.Images
	case "video":
		list = p.Videos
	default:
		return UnitQuote{}, fmt.Errorf("%w: unknown unit kind %q", ErrInvalidQuote, kind)
	}
	for _, u := range list {
		if u.Model != model || (kind == "video" && u.Tier != tier) {
			continue
		}
		providerMinor := usdMicrosToMinor(u.ProviderMicros)
		retailMinor := usdMicrosToMinor(u.RetailMicros)
		gross := retailMinor - providerMinor
		margin := int64(0)
		if retailMinor > 0 {
			margin = mulDivRound(gross, 10_000, retailMinor)
		}
		return UnitQuote{
			Model: model, Tier: tier,
			ProviderMinor: providerMinor, RetailMinor: retailMinor,
			GrossMinor: gross, MarginBPS: margin,
		}, nil
	}
	return UnitQuote{}, fmt.Errorf("%w: no unit price for %s/%s", ErrInvalidQuote, model, tier)
}

// Calculator builds the pure calculator from a validated pricebook. Tiers are
// sorted by ceiling so callers can inspect the resulting ladders.
func (p Pricebook) Calculator() Calculator {
	retail := Book{}
	prov := Book{}
	official := Book{}
	for _, row := range p.Models {
		retail[row.Model] = append(retail[row.Model], Tier{MaxInput: row.MaxInput, Peak: row.Peak, Price: row.Retail})
		prov[row.Model] = append(prov[row.Model], Tier{MaxInput: row.MaxInput, Peak: row.Peak, Price: row.Provider})
		if row.Official.Input > 0 || row.Official.Output > 0 {
			official[row.Model] = append(official[row.Model], Tier{MaxInput: row.MaxInput, Peak: row.Peak, Price: row.Official})
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
