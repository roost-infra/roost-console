// Package provider separates upstream provider cost rules from the retail
// price a product charges its users.
//
// The calculator is deliberately pure: it does not read the database, the
// clock, or a provider SDK. A caller resolves the model's retail table and
// the selected provider's cost table, then asks for one quote.
package provider

import (
	"errors"
	"fmt"
	"math/big"
)

const (
	// MicroPerUSD is the fixed-point unit for USD prices in this package.
	MicroPerUSD int64 = 1_000_000
	// CreditsPerUSD is the commercial conversion used by the console.
	CreditsPerUSD int64 = 680
	// MinorPerCredit keeps the console's 1 credit = 1000 minor convention.
	MinorPerCredit int64 = 1000
	// DefaultPeakBPS is a normal, non-peak request.
	DefaultPeakBPS int64 = 10_000
)

// ErrInvalidQuote is returned when a model, tier, or usage cannot be priced.
var ErrInvalidQuote = errors.New("provider: invalid quote")

// Price is a provider or retail price in micro-USD per 1,000,000 tokens.
type Price struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Tier prices up to MaxInput prompt tokens. MaxInput <= 0 means no upper bound
// and is selected only when no positive tier matches.
type Tier struct {
	MaxInput int64
	// Peak is an optional provider/label dimension ("高峰", "非高峰"). An empty
	// value is the default, non-peak tier.
	Peak  string
	Price Price
}

// ModelPrices is the tier ladder for one model.
type ModelPrices []Tier

// TierFor picks the smallest matching positive ceiling, falling back to the
// unbounded tier. The caller passes the prompt token count that the provider
// (or product) uses for tier selection.
func (m ModelPrices) TierFor(tokens int64) (Tier, error) {
	return m.TierForPeak(tokens, "")
}

// TierForPeak selects the tier for a peak label, falling back to the default
// (empty/非高峰) tier when the requested label has no dedicated row.
func (m ModelPrices) TierForPeak(tokens int64, peak string) (Tier, error) {
	if peak != "" {
		if t, ok := m.selectTier(tokens, func(p string) bool { return p == peak }); ok {
			return t, nil
		}
	}
	if t, ok := m.selectTier(tokens, func(p string) bool { return p == "" || p == "非高峰" }); ok {
		return t, nil
	}
	if peak != "" {
		// A peak-only model with no default row still has to price.
		if t, ok := m.selectTier(tokens, func(string) bool { return true }); ok {
			return t, nil
		}
	}
	return Tier{}, fmt.Errorf("%w: no tier for %d tokens", ErrInvalidQuote, tokens)
}

func (m ModelPrices) selectTier(tokens int64, match func(peak string) bool) (Tier, bool) {
	var best *Tier
	var unbounded *Tier
	for i := range m {
		t := &m[i]
		if !match(t.Peak) {
			continue
		}
		if t.MaxInput <= 0 {
			if unbounded == nil {
				unbounded = t
			}
			continue
		}
		if tokens <= t.MaxInput && (best == nil || t.MaxInput < best.MaxInput) {
			best = t
		}
	}
	if best != nil {
		return *best, true
	}
	if unbounded != nil {
		return *unbounded, true
	}
	return Tier{}, false
}

// Book maps model ids to their tier ladders.
type Book map[string]ModelPrices

// Usage is the canonical, normalized view of one request.
//
// OriginalInput is the client's pre-compression input (what PAYG charges).
// Input is the compressed, non-cached input actually sent upstream.
// CacheRead/CacheWrite are the provider-reported cache components.
type Usage struct {
	OriginalInput     int64
	Input             int64
	Output            int64
	CacheRead         int64
	CacheWrite        int64
	RetailTierInput   int64 // original prompt including cache, for the retail tier
	ProviderTierInput int64 // compressed prompt including cache, for the provider tier
}

// PlanBasis says what a subscription allowance is denominated in.
type PlanBasis string

const (
	// BasisProvider means the allowance is provider-list USD (for FYT CodePlan).
	BasisProvider PlanBasis = "provider"
	// BasisOfficial means the allowance is the model vendor's official USD.
	BasisOfficial PlanBasis = "official"
)

// Plan allocates a fixed subscription fee over its included usage allowance.
// Both values are in micro-USD. A zero Plan leaves provider cost unallocated.
type Plan struct {
	ID              string    `json:"id"`
	CostMicros      int64     `json:"cost_micros"`
	AllowanceMicros int64     `json:"allowance_micros"`
	Basis           PlanBasis `json:"basis"`
}

// Request is one pricing input.
type Request struct {
	Model string
	Usage Usage
	// Peak selects a provider-declared peak tier ("高峰"). Empty uses the
	// default/non-peak row.
	Peak    string
	PeakBPS int64
}

// Quote is the complete cost/retail/margin view of one request.
type Quote struct {
	Model           string
	RetailTier      Tier
	ProviderTier    Tier
	RetailPrice     Price
	ProviderPrice   Price
	RetailMinor     int64
	RetailCredits   int64
	ProviderMinor   int64
	ProviderMicros  int64
	AllocatedMinor  int64
	AllocatedMicros int64
	GrossMinor      int64
	MarginBPS       int64
}

// Calculator is a pair of price books plus one subscription plan.
type Calculator struct {
	Retail   Book
	Provider Book
	Official Book
	Plan     Plan
}

// Quote prices one request.
//
// The user is charged on OriginalInput + Output, and never on cache components
// separately: the original input already contains cached tokens, so charging
// cache again would double-bill. The provider is charged on compressed Input,
// cache components, and Output.
func (c Calculator) Quote(req Request) (Quote, error) {
	if req.Model == "" {
		return Quote{}, fmt.Errorf("%w: model is required", ErrInvalidQuote)
	}
	retailTiers, ok := c.Retail[req.Model]
	if !ok {
		return Quote{}, fmt.Errorf("%w: no retail price for %s", ErrInvalidQuote, req.Model)
	}
	costBook := c.Provider
	if c.Plan.Basis == BasisOfficial {
		costBook = c.Official
	}
	costTiers, ok := costBook[req.Model]
	if !ok {
		return Quote{}, fmt.Errorf("%w: no provider price for %s", ErrInvalidQuote, req.Model)
	}

	u := req.Usage
	if u.OriginalInput <= 0 {
		u.OriginalInput = u.Input
	}
	if u.RetailTierInput <= 0 {
		u.RetailTierInput = u.OriginalInput
	}
	if u.ProviderTierInput <= 0 {
		u.ProviderTierInput = u.Input + u.CacheRead
	}
	if req.Peak != "" && req.PeakBPS > DefaultPeakBPS {
		return Quote{}, fmt.Errorf("%w: peak tier and peak multiplier are mutually exclusive", ErrInvalidQuote)
	}
	peak := req.PeakBPS
	if peak <= 0 {
		peak = DefaultPeakBPS
	}

	retailTier, err := retailTiers.TierForPeak(u.RetailTierInput, req.Peak)
	if err != nil {
		return Quote{}, err
	}
	providerTier, err := costTiers.TierForPeak(u.ProviderTierInput, req.Peak)
	if err != nil {
		return Quote{}, err
	}

	// User bill: pre-compression input + output. No cache components.
	retailMicros := mulDivRound(u.OriginalInput, retailTier.Price.Input, MicroPerUSD) +
		mulDivRound(u.Output, retailTier.Price.Output, MicroPerUSD)
	retailMicros = mulDivRound(retailMicros, peak, DefaultPeakBPS)
	retailMinor := usdMicrosToMinor(retailMicros)

	// Provider cost: compressed input + actual cache + output.
	providerMicros := mulDivRound(u.Input, providerTier.Price.Input, MicroPerUSD) +
		mulDivRound(u.CacheRead, providerTier.Price.CacheRead, MicroPerUSD) +
		mulDivRound(u.CacheWrite, providerTier.Price.CacheWrite, MicroPerUSD) +
		mulDivRound(u.Output, providerTier.Price.Output, MicroPerUSD)
	providerMicros = mulDivRound(providerMicros, peak, DefaultPeakBPS)
	providerMinor := usdMicrosToMinor(providerMicros)

	allocatedMinor := providerMinor
	allocatedMicros := providerMicros
	if c.Plan.AllowanceMicros > 0 {
		allocatedMinor = mulDivRound(providerMinor, c.Plan.CostMicros, c.Plan.AllowanceMicros)
		allocatedMicros = mulDivRound(providerMicros, c.Plan.CostMicros, c.Plan.AllowanceMicros)
	}
	grossMinor := retailMinor - allocatedMinor
	marginBPS := int64(0)
	if retailMinor > 0 {
		marginBPS = mulDivRoundSigned(grossMinor, 10_000, retailMinor)
	}
	return Quote{
		Model:           req.Model,
		RetailTier:      retailTier,
		ProviderTier:    providerTier,
		RetailPrice:     retailTier.Price,
		ProviderPrice:   providerTier.Price,
		RetailMinor:     retailMinor,
		RetailCredits:   creditsFromMinor(retailMinor),
		ProviderMinor:   providerMinor,
		ProviderMicros:  providerMicros,
		AllocatedMinor:  allocatedMinor,
		AllocatedMicros: allocatedMicros,
		GrossMinor:      grossMinor,
		MarginBPS:       marginBPS,
	}, nil
}

// CostCreditRate is a convenience for reporting: provider USD per 1M tokens
// converted to credits, after the subscription coefficient.
func (c Calculator) CostCreditRate(providerMicrosPerMillion int64) int64 {
	minor := usdMicrosToMinor(providerMicrosPerMillion)
	if c.Plan.AllowanceMicros > 0 {
		minor = mulDivRound(minor, c.Plan.CostMicros, c.Plan.AllowanceMicros)
	}
	return minor
}

func usdMicrosToMinor(micros int64) int64 {
	if micros <= 0 {
		return 0
	}
	return mulDivRound(micros, CreditsPerUSD*MinorPerCredit, MicroPerUSD)
}

func creditsFromMinor(minor int64) int64 {
	if minor <= 0 {
		return 0
	}
	credits := (minor + MinorPerCredit - 1) / MinorPerCredit
	if credits < 1 {
		return 1
	}
	return credits
}

// mulDivRoundSigned is mulDivRound that preserves a negative numerator.
func mulDivRoundSigned(a, b, c int64) int64 {
	if a == 0 || b <= 0 || c <= 0 {
		return 0
	}
	neg := a < 0
	if neg {
		a = -a
	}
	v := mulDivRound(a, b, c)
	if neg {
		return -v
	}
	return v
}

// mulDivRound computes round(a*b/c) half away from zero using big integers.
// All inputs are expected to be non-negative; negative inputs return 0.
func mulDivRound(a, b, c int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 {
		return 0
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	half := new(big.Int).Rsh(big.NewInt(c), 1)
	product.Add(product, half)
	return new(big.Int).Quo(product, big.NewInt(c)).Int64()
}
