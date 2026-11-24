package billing

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"
)

// Billing V2 money math. 1 credit = ¥0.01; 1000 minor = 1 credit; prices are
// minor per 1,000,000 tokens. A component charge rounds half away from zero,
// the settlement total is ceil'd to whole credits and a non-zero settlement
// costs at least one credit. All arithmetic is integer (big.Int for the
// token×price×peak product) so a long-context request cannot lose a minor unit
// to float64 rounding.
const (
	MinorPerMillion      int64 = 1_000_000
	MinorPerCredit       int64 = 1000
	PeakBaseBPS          int64 = 10000
	peakDenominator      int64 = 10_000_000_000 // price(1e6) × peak bps(1e4)
	compressionBufferBPS int64 = 200            // relay's 2% original-input buffer
)

// TokenUsage is the settle-v2 usage object.
type TokenUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// IsZero reports whether every counter is zero.
func (u TokenUsage) IsZero() bool {
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0
}

// Validate rejects negative counters.
func (u TokenUsage) Validate() error {
	for name, value := range map[string]int64{
		"input":       u.Input,
		"output":      u.Output,
		"cache_read":  u.CacheRead,
		"cache_write": u.CacheWrite,
	} {
		if value < 0 {
			return fmt.Errorf("%w: usage.%s must be >= 0", ErrInvalidPricing, name)
		}
	}
	return nil
}

func (u TokenUsage) tokens(component string) int64 {
	switch component {
	case ComponentInput:
		return u.Input
	case ComponentOutput:
		return u.Output
	case ComponentCacheRead:
		return u.CacheRead
	case ComponentCacheWrite:
		return u.CacheWrite
	}
	return 0
}

// ModelUsage is one model's aggregated token usage in a dslpha release.
type ModelUsage struct {
	Model      string `json:"model"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	Calls      int64  `json:"calls"`
}

// NormalizeModelUsage validates and normalizes a product's per-model usage
// list. Models are trimmed and a missing call count defaults to one.
func NormalizeModelUsage(in []ModelUsage) ([]ModelUsage, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]ModelUsage, len(in))
	for i, usage := range in {
		usage.Model = strings.TrimSpace(usage.Model)
		if usage.Model == "" {
			return nil, fmt.Errorf("%w: model_usage[%d].model is required", ErrInvalidPricing, i)
		}
		if usage.Input < 0 || usage.Output < 0 || usage.CacheRead < 0 || usage.CacheWrite < 0 {
			return nil, fmt.Errorf("%w: model_usage[%d] tokens must be >= 0", ErrInvalidPricing, i)
		}
		if usage.Calls < 0 {
			return nil, fmt.Errorf("%w: model_usage[%d].calls must be >= 0", ErrInvalidPricing, i)
		}
		if usage.Calls == 0 {
			usage.Calls = 1
		}
		out[i] = usage
	}
	return out, nil
}

// CompressionBasis is the counterfactual compression report of a settle-v2
// request. The user share is deliberately absent: the console reads
// compression.user_share_bps from settings, never from the client.
type CompressionBasis struct {
	Mode              string `json:"mode"`
	BeforeInputTokens int64  `json:"before_input_tokens"`
	AfterInputTokens  int64  `json:"after_input_tokens"`
	SummaryCostMinor  int64  `json:"summary_cost_minor"`
	CacheLossMinor    int64  `json:"cache_loss_minor"`
}

// NormalizeCompressionBasis validates the optional basis object. An empty mode
// means "original", the only mode with a counterfactual charge adjustment.
func NormalizeCompressionBasis(b *CompressionBasis) (*CompressionBasis, error) {
	if b == nil {
		return nil, nil
	}
	out := *b
	out.Mode = strings.ToLower(strings.TrimSpace(out.Mode))
	if out.Mode == "" {
		out.Mode = "original"
	}
	for name, value := range map[string]int64{
		"before_input_tokens": out.BeforeInputTokens,
		"after_input_tokens":  out.AfterInputTokens,
		"summary_cost_minor":  out.SummaryCostMinor,
		"cache_loss_minor":    out.CacheLossMinor,
	} {
		if value < 0 {
			return nil, fmt.Errorf("%w: basis.%s must be >= 0", ErrInvalidPricing, name)
		}
	}
	return &out, nil
}

// CompressionCharge is the counterfactual compression accounting of one
// settlement. NetGainMinor is the incremental margin versus no compression:
// saved×cost − user-share×retail − summary − cache-loss.
type CompressionCharge struct {
	Applied                    bool
	BeforeInputTokens          int64
	AfterInputTokens           int64
	BufferedInputTokens        int64
	ChargeInputTokens          int64
	SavedTokens                int64
	UserShareTokens            int64
	UserShareBPS               int64
	UserShareMinor             int64
	RetailInputMinorPerMillion int64
	CostInputMinorPerMillion   int64
	SummaryCostMinor           int64
	CacheLossMinor             int64
	NoCompressionMarginMinor   int64
	CompressedMarginMinor      int64
	NetGainMinor               int64
}

// Charge is one fully-priced settlement.
type Charge struct {
	AmountMinor       int64
	AmountCredits     int64
	Model             string
	PriceVersion      string
	SettingsVersion   string
	TierMaxInput      int64
	PeakMultiplierBPS int64
	ChargeInputTokens int64
	InputMinor        int64
	OutputMinor       int64
	CacheReadMinor    int64
	CacheWriteMinor   int64
	ToolsMinor        int64
	Compression       *CompressionCharge
}

// CreditsFromMinor converts minor units to whole credits, ceil rounding and a
// one-credit minimum for any non-zero settlement.
func CreditsFromMinor(minor int64) int64 {
	if minor <= 0 {
		return 0
	}
	credits := (minor + MinorPerCredit - 1) / MinorPerCredit
	if credits < 1 {
		credits = 1
	}
	return credits
}

// roundHalfAwayDiv returns round(numerator/denominator) with half away from
// zero. Both values are non-negative.
func roundHalfAwayDiv(numerator, denominator *big.Int) int64 {
	if denominator.Sign() == 0 {
		return 0
	}
	half := new(big.Int).Rsh(new(big.Int).Set(denominator), 1)
	value := new(big.Int).Add(new(big.Int).Set(numerator), half)
	value.Quo(value, denominator)
	return value.Int64()
}

// mulDivRoundHalfAway computes round(a×b/c) exactly.
func mulDivRoundHalfAway(a, b, c int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 {
		return 0
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return roundHalfAwayDiv(product, big.NewInt(c))
}

// mul3DivRoundHalfAway computes round(a×b×c/denominator) exactly.
func mul3DivRoundHalfAway(a, b, c, denominator int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 || denominator <= 0 {
		return 0
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	product.Mul(product, big.NewInt(c))
	return roundHalfAwayDiv(product, big.NewInt(denominator))
}

// ToolChargesMinor prices a settlement's tool calls from settings. Only
// succeeded calls are billable and an unconfigured tool prices at zero.
func ToolChargesMinor(settings Settings, charges []ToolCharge) int64 {
	var total int64
	for _, charge := range charges {
		if strings.ToLower(strings.TrimSpace(charge.Status)) != ToolChargeStatusSucceeded {
			continue
		}
		price, ok := settings.ToolPriceMinor(charge.Tool)
		if !ok || price <= 0 {
			continue
		}
		total += int64(math.Round(charge.Quantity * float64(price)))
	}
	return total
}

// PriceToolCharges returns a copy of the breakdown with the authoritative
// unit price and amount filled in from settings, for persistence and response.
func PriceToolCharges(settings Settings, charges []ToolCharge) []ToolCharge {
	if len(charges) == 0 {
		return nil
	}
	out := make([]ToolCharge, len(charges))
	copy(out, charges)
	for i := range out {
		price, ok := settings.ToolPriceMinor(out[i].Tool)
		if !ok {
			out[i].UnitPriceMicro = 0
			out[i].AmountMinor = 0
			continue
		}
		out[i].UnitPriceMicro = price * 1000 // 1 minor = 1000 micro-credit
		out[i].AmountMinor = int64(math.Round(out[i].Quantity * float64(price)))
	}
	return out
}

// PriceCharge prices one model request with the console's single algorithm:
// pick the tier by input tokens, apply the peak window to every component,
// apply the compression counterfactual when requested, add tool charges, then
// ceil to whole credits.
func (b PriceBook) PriceCharge(model string, usage TokenUsage, startedAt time.Time, basis *CompressionBasis, settings Settings, toolCharges []ToolCharge) (Charge, error) {
	model = strings.TrimSpace(model)
	entry, ok := b.Model(model)
	if !ok {
		return Charge{}, fmt.Errorf("%w: %s", ErrUnknownModel, model)
	}
	tier, ok := entry.TierFor(usage.Input)
	if !ok {
		return Charge{}, fmt.Errorf("%w: %s", ErrPriceTierNotFound, model)
	}
	peakBPS := entry.PeakMultiplierBPS(startedAt)
	chargeInput := usage.Input
	var compression *CompressionCharge
	if basis != nil && strings.EqualFold(basis.Mode, "original") && basis.BeforeInputTokens > 0 {
		before := basis.BeforeInputTokens
		beforeBuffered := before + (before*compressionBufferBPS+PeakBaseBPS-1)/PeakBaseBPS
		after := basis.AfterInputTokens
		saved := beforeBuffered - after
		if saved < 0 {
			saved = 0
		}
		share := saved * settings.CompressionUserShareBPS / PeakBaseBPS
		if share < 0 {
			share = 0
		}
		chargeInput = beforeBuffered - share
		if after > chargeInput {
			chargeInput = after
		}
		if chargeInput < 0 {
			chargeInput = 0
		}
		inputPrice := tier.Price(ComponentInput)
		compression = &CompressionCharge{
			Applied:                    true,
			BeforeInputTokens:          before,
			AfterInputTokens:           after,
			BufferedInputTokens:        beforeBuffered,
			ChargeInputTokens:          chargeInput,
			SavedTokens:                saved,
			UserShareTokens:            share,
			UserShareBPS:               settings.CompressionUserShareBPS,
			UserShareMinor:             mulDivRoundHalfAway(share, inputPrice.RetailMinorPerMillion, MinorPerMillion),
			RetailInputMinorPerMillion: inputPrice.RetailMinorPerMillion,
			CostInputMinorPerMillion:   inputPrice.CostMinorPerMillion,
			SummaryCostMinor:           basis.SummaryCostMinor,
			CacheLossMinor:             basis.CacheLossMinor,
			NoCompressionMarginMinor: mulDivRoundHalfAway(beforeBuffered, inputPrice.RetailMinorPerMillion, MinorPerMillion) -
				mulDivRoundHalfAway(beforeBuffered, inputPrice.CostMinorPerMillion, MinorPerMillion),
			CompressedMarginMinor: mulDivRoundHalfAway(chargeInput, inputPrice.RetailMinorPerMillion, MinorPerMillion) -
				mulDivRoundHalfAway(after, inputPrice.CostMinorPerMillion, MinorPerMillion) -
				basis.SummaryCostMinor - basis.CacheLossMinor,
		}
		compression.NetGainMinor = mulDivRoundHalfAway(saved, inputPrice.CostMinorPerMillion, MinorPerMillion) -
			mulDivRoundHalfAway(share, inputPrice.RetailMinorPerMillion, MinorPerMillion) -
			basis.SummaryCostMinor - basis.CacheLossMinor
	}

	components := map[string]int64{}
	for _, component := range PriceComponents {
		tokens := usage.tokens(component)
		if component == ComponentInput {
			tokens = chargeInput
		}
		price := tier.Price(component)
		components[component] = mul3DivRoundHalfAway(tokens, price.RetailMinorPerMillion, peakBPS, peakDenominator)
	}
	toolsMinor := ToolChargesMinor(settings, toolCharges)
	total := toolsMinor
	for _, component := range PriceComponents {
		total += components[component]
	}
	return Charge{
		AmountMinor:       total,
		AmountCredits:     CreditsFromMinor(total),
		Model:             model,
		PriceVersion:      b.Version,
		SettingsVersion:   settings.Version,
		TierMaxInput:      tier.MaxInput,
		PeakMultiplierBPS: peakBPS,
		ChargeInputTokens: chargeInput,
		InputMinor:        components[ComponentInput],
		OutputMinor:       components[ComponentOutput],
		CacheReadMinor:    components[ComponentCacheRead],
		CacheWriteMinor:   components[ComponentCacheWrite],
		ToolsMinor:        toolsMinor,
		Compression:       compression,
	}, nil
}

// EstimatedCostMinor estimates the upstream CNY cost of a token usage from the
// book's cost column. It is stored in usage_events when the relay/product did
// not report a cost, so margin reporting never reads a missing cost as zero.
func (b PriceBook) EstimatedCostMinor(model string, usage TokenUsage) int64 {
	entry, ok := b.Model(model)
	if !ok {
		return 0
	}
	tier, ok := entry.TierFor(usage.Input)
	if !ok {
		return 0
	}
	var total int64
	for _, component := range PriceComponents {
		total += mulDivRoundHalfAway(usage.tokens(component), tier.Price(component).CostMinorPerMillion, MinorPerMillion)
	}
	return total
}
