package billing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// sonnetBook is the contract's golden model: Claude Sonnet 5.5 with the retail
// prices from section 2.1 and cost from the v1 price-book seed.
func sonnetBook() PriceBook {
	book := PriceBook{
		Version:         "2026-11-25.1",
		SettingsVersion: DefaultSettingsVersion,
		EffectiveAt:     time.Date(2026, 11, 25, 0, 0, 0, 0, cstZone),
		FX:              FXSettings{USDToCredits: 720, CreditYuan: 0.01},
		Models: []PriceModel{{
			Model:    "claude-sonnet-5.5",
			Currency: "CREDIT",
			Tiers: []PriceTier{{
				MaxInput: 0,
				Prices: map[string]ComponentPrice{
					ComponentInput:      {RetailMinorPerMillion: 1_122_500, CostMinorPerMillion: 544_000, CostCurrency: "CNY"},
					ComponentOutput:     {RetailMinorPerMillion: 5_610_000, CostMinorPerMillion: 2_720_000, CostCurrency: "CNY"},
					ComponentCacheRead:  {RetailMinorPerMillion: 57_500, CostMinorPerMillion: 27_200, CostCurrency: "CNY"},
					ComponentCacheWrite: {RetailMinorPerMillion: 0, CostMinorPerMillion: 680_000, CostCurrency: "CNY"},
				},
			}},
		}},
	}
	book.Finalize()
	return book
}

func TestGoldenSonnetPlain(t *testing.T) {
	book := sonnetBook()
	charge, err := book.PriceCharge("claude-sonnet-5.5", TokenUsage{Input: 150_000, Output: 2_000}, time.Time{}, nil, DefaultSettings(), nil)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if charge.AmountMinor != 179_595 || charge.AmountCredits != 180 {
		t.Fatalf("amount=%d minor=%d credits want 179595/180", charge.AmountMinor, charge.AmountCredits)
	}
	if charge.InputMinor != 168_375 || charge.OutputMinor != 11_220 {
		t.Fatalf("components input=%d output=%d", charge.InputMinor, charge.OutputMinor)
	}
	if charge.TierMaxInput != 0 || charge.PeakMultiplierBPS != 10000 {
		t.Fatalf("tier=%d peak=%d", charge.TierMaxInput, charge.PeakMultiplierBPS)
	}
}

func TestGoldenSonnetCompressed(t *testing.T) {
	book := sonnetBook()
	basis := &CompressionBasis{
		Mode:              "original",
		BeforeInputTokens: 150_000,
		AfterInputTokens:  90_000,
		SummaryCostMinor:  20,
	}
	charge, err := book.PriceCharge("claude-sonnet-5.5", TokenUsage{Input: 150_000, Output: 2_000}, time.Time{}, basis, DefaultSettings(), nil)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if charge.AmountMinor != 165_283 || charge.AmountCredits != 166 {
		t.Fatalf("amount=%d credits=%d want 165283/166", charge.AmountMinor, charge.AmountCredits)
	}
	if charge.Compression == nil {
		t.Fatal("compression report missing")
	}
	compression := charge.Compression
	if compression.BeforeInputTokens != 150_000 || compression.BufferedInputTokens != 153_000 {
		t.Fatalf("before=%d buffered=%d", compression.BeforeInputTokens, compression.BufferedInputTokens)
	}
	if compression.SavedTokens != 63_000 || compression.UserShareTokens != 15_750 {
		t.Fatalf("saved=%d share=%d want 63000/15750", compression.SavedTokens, compression.UserShareTokens)
	}
	if compression.ChargeInputTokens != 137_250 {
		t.Fatalf("charge input=%d want 137250", compression.ChargeInputTokens)
	}
	// net = saved×cost − share×retail − summary
	//     = 34272 − 17679 − 20 = 16573 minor.
	if compression.NetGainMinor != 16_573 {
		t.Fatalf("net gain=%d want 16573", compression.NetGainMinor)
	}
	// Margins round independently from the contract's net-gain formula, so the
	// two can differ by at most one minor unit.
	if diff := compression.CompressedMarginMinor - compression.NoCompressionMarginMinor; diff < compression.NetGainMinor-1 || diff > compression.NetGainMinor+1 {
		t.Fatalf("margin delta=%d net=%d", diff, compression.NetGainMinor)
	}
}

func TestTierBoundary(t *testing.T) {
	book := PriceBook{Version: "tiers", Models: []PriceModel{{
		Model: "tiered",
		Tiers: []PriceTier{
			{MaxInput: 200_000, Prices: map[string]ComponentPrice{
				ComponentInput: {RetailMinorPerMillion: 1_000_000},
			}},
			{MaxInput: 0, Prices: map[string]ComponentPrice{
				ComponentInput: {RetailMinorPerMillion: 2_000_000},
			}},
		},
	}}}
	book.Finalize()
	cases := map[int64]struct {
		tier   int64
		minor  int64
		credit int64
	}{
		199_999: {200_000, 199_999, 200},
		200_000: {200_000, 200_000, 200},
		200_001: {0, 400_002, 401},
	}
	for input, want := range cases {
		charge, err := book.PriceCharge("tiered", TokenUsage{Input: input}, time.Time{}, nil, DefaultSettings(), nil)
		if err != nil {
			t.Fatalf("input=%d price: %v", input, err)
		}
		if charge.TierMaxInput != want.tier || charge.AmountMinor != want.minor || charge.AmountCredits != want.credit {
			t.Fatalf("input=%d tier=%d minor=%d credits=%d want tier=%d minor=%d credits=%d",
				input, charge.TierMaxInput, charge.AmountMinor, charge.AmountCredits, want.tier, want.minor, want.credit)
		}
	}
}

func TestPeakWindowMultiplier(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	book := sonnetBook()
	book.Models[0].Peak = []PeakWindow{{
		Weekdays:      []int{1, 2, 3, 4, 5},
		StartMinute:   540,
		EndMinute:     1080,
		Timezone:      "Asia/Shanghai",
		MultiplierBPS: 12000,
	}}
	book.Finalize()
	if book.Models[0].Peak[0].MultiplierBPS != 12000 {
		t.Fatalf("peak window lost during finalize: %+v", book.Models[0].Peak)
	}
	monday10 := time.Date(2026, 11, 23, 10, 0, 0, 0, shanghai) // Monday, in window.
	peak, err := book.PriceCharge("claude-sonnet-5.5", TokenUsage{Input: 150_000, Output: 2_000}, monday10, nil, DefaultSettings(), nil)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if peak.PeakMultiplierBPS != 12000 {
		t.Fatalf("peak bps=%d want 12000", peak.PeakMultiplierBPS)
	}
	// 179595 × 1.2 = 215514 minor.
	if peak.AmountMinor != 215_514 || peak.AmountCredits != 216 {
		t.Fatalf("peak minor=%d credits=%d want 215514/216", peak.AmountMinor, peak.AmountCredits)
	}
	for name, at := range map[string]time.Time{
		"monday evening": time.Date(2026, 11, 23, 19, 0, 0, 0, shanghai),
		"sunday morning": time.Date(2026, 11, 22, 10, 0, 0, 0, shanghai),
	} {
		charge, err := book.PriceCharge("claude-sonnet-5.5", TokenUsage{Input: 150_000, Output: 2_000}, at, nil, DefaultSettings(), nil)
		if err != nil {
			t.Fatalf("%s price: %v", name, err)
		}
		if charge.PeakMultiplierBPS != 10000 || charge.AmountMinor != 179_595 {
			t.Fatalf("%s peak=%d minor=%d", name, charge.PeakMultiplierBPS, charge.AmountMinor)
		}
	}
}

func TestRoundingCredits(t *testing.T) {
	cases := map[int64]int64{
		0:      0,
		1:      1,
		999:    1,
		1000:   1,
		1001:   2,
		1400:   2,
		165283: 166,
		179595: 180,
	}
	for minor, want := range cases {
		if got := CreditsFromMinor(minor); got != want {
			t.Fatalf("CreditsFromMinor(%d)=%d want %d", minor, got, want)
		}
	}
}

func TestRoundingHalfAwayFromZero(t *testing.T) {
	book := PriceBook{Version: "round", Models: []PriceModel{{
		Model: "round",
		Tiers: []PriceTier{{Prices: map[string]ComponentPrice{
			ComponentInput: {RetailMinorPerMillion: 1},
		}}},
	}}}
	book.Finalize()
	// One token at 0.5 minor rounds up to 1 minor (half away from zero), so the
	// settlement's one-credit minimum applies.
	charge, err := book.PriceCharge("round", TokenUsage{Input: 500_000}, time.Time{}, nil, DefaultSettings(), nil)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if charge.AmountMinor != 1 || charge.AmountCredits != 1 {
		t.Fatalf("minor=%d credits=%d want 1/1", charge.AmountMinor, charge.AmountCredits)
	}
	// 0.4 minor rounds down to 0 and the model charges nothing.
	charge, err = book.PriceCharge("round", TokenUsage{Input: 400_000}, time.Time{}, nil, DefaultSettings(), nil)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if charge.AmountMinor != 0 || charge.AmountCredits != 0 {
		t.Fatalf("minor=%d credits=%d want 0/0", charge.AmountMinor, charge.AmountCredits)
	}
}

func TestToolChargesPricedFromSettings(t *testing.T) {
	book := sonnetBook()
	settings := DefaultSettings()
	tools := []ToolCharge{
		{Tool: "search", Unit: "call", Quantity: 2, Status: ToolChargeStatusSucceeded},
		{Tool: "unknown-tool", Unit: "call", Quantity: 3, Status: ToolChargeStatusSucceeded},
	}
	charge, err := book.PriceCharge("claude-sonnet-5.5", TokenUsage{}, time.Time{}, nil, settings, tools)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	// search 7 minor × 2; unknown tool is unconfigured and prices at zero.
	if charge.ToolsMinor != 14 || charge.AmountMinor != 14 || charge.AmountCredits != 1 {
		t.Fatalf("tools=%d minor=%d credits=%d", charge.ToolsMinor, charge.AmountMinor, charge.AmountCredits)
	}
	priced := PriceToolCharges(settings, tools)
	if priced[0].AmountMinor != 14 || priced[0].UnitPriceMicro != 7000 {
		t.Fatalf("priced search=%+v", priced[0])
	}
	if priced[1].AmountMinor != 0 || priced[1].UnitPriceMicro != 0 {
		t.Fatalf("unknown tool priced: %+v", priced[1])
	}
}

func TestUnknownModelErrors(t *testing.T) {
	book := sonnetBook()
	_, err := book.PriceCharge("does-not-exist", TokenUsage{Input: 1}, time.Time{}, nil, DefaultSettings(), nil)
	if !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("err=%v", err)
	}
}

func TestPriceBookHashIgnoresCost(t *testing.T) {
	left := sonnetBook()
	right := sonnetBook()
	for component, price := range right.Models[0].Tiers[0].Prices {
		price.CostMinorPerMillion += 1
		right.Models[0].Tiers[0].Prices[component] = price
	}
	right.Finalize()
	if left.Hash == "" || left.Hash != right.Hash {
		t.Fatalf("hashes differ: %q vs %q (cost must not change the public hash)", left.Hash, right.Hash)
	}
	if !strings.HasPrefix(left.Hash, "sha256:") || len(left.Hash) != len("sha256:")+64 {
		t.Fatalf("hash shape=%q", left.Hash)
	}
	// A retail change must change the hash.
	retail := sonnetBook()
	price := retail.Models[0].Tiers[0].Prices[ComponentInput]
	price.RetailMinorPerMillion += 1
	retail.Models[0].Tiers[0].Prices[ComponentInput] = price
	retail.Finalize()
	if retail.Hash == left.Hash {
		t.Fatal("retail change did not change the hash")
	}
}

func TestPublicViewOmitsCostInternalIncludesIt(t *testing.T) {
	book := sonnetBook()
	publicJSON, err := json.Marshal(book.PublicView())
	if err != nil {
		t.Fatalf("marshal public: %v", err)
	}
	if strings.Contains(string(publicJSON), "cost") {
		t.Fatalf("public view leaks cost: %s", publicJSON)
	}
	if !strings.Contains(string(publicJSON), `"input":1122500`) {
		t.Fatalf("public view lost retail price: %s", publicJSON)
	}
	internalJSON, err := json.Marshal(book.InternalView())
	if err != nil {
		t.Fatalf("marshal internal: %v", err)
	}
	if !strings.Contains(string(internalJSON), `"cost_minor_per_million":544000`) {
		t.Fatalf("internal view missing cost: %s", internalJSON)
	}
	if !strings.Contains(string(internalJSON), `"settings_version":"2026-11-25.1"`) {
		t.Fatalf("internal view missing settings version: %s", internalJSON)
	}
	public := book.PublicView()
	internal := book.InternalView()
	if public.Hash != internal.Hash || public.Version != internal.Version {
		t.Fatalf("public/internal hash mismatch: %q vs %q", public.Hash, internal.Hash)
	}
	if public.EffectiveAt != "2026-11-25T00:00:00+08:00" {
		t.Fatalf("effective_at=%q", public.EffectiveAt)
	}
}

func TestSettingsDefaultsAndOverrides(t *testing.T) {
	settings := DefaultSettings()
	if settings.FXUSDToCredits != 720 || settings.CompressionUserShareBPS != 2500 ||
		settings.SandboxMarkupBPS != 13000 || settings.Version != DefaultSettingsVersion {
		t.Fatalf("defaults=%+v", settings)
	}
	if price, ok := settings.ToolPriceMinor("search"); !ok || price != 7 {
		t.Fatalf("search price=%d ok=%v", price, ok)
	}
	overrides := map[string][]byte{
		"fx.usd_to_credits":          []byte(`700`),
		"compression.user_share_bps": []byte(`3000`),
		"settings.version":           []byte(`"2026-11-26.1"`),
		"tools.fetch.price_minor":    []byte(`9`),
		"billing.credit_yuan":        []byte(`0.02`),
	}
	for key, raw := range overrides {
		if err := settings.apply(key, raw); err != nil {
			t.Fatalf("apply %s: %v", key, err)
		}
	}
	if settings.FXUSDToCredits != 700 || settings.CompressionUserShareBPS != 3000 ||
		settings.Version != "2026-11-26.1" || settings.CreditYuan != 0.02 {
		t.Fatalf("overridden=%+v", settings)
	}
	if price, _ := settings.ToolPriceMinor("fetch"); price != 9 {
		t.Fatalf("fetch price=%d", price)
	}
	if err := settings.apply("fx.usd_to_credits", []byte(`"not-a-number"`)); err == nil {
		t.Fatal("invalid settings value accepted")
	}
}

func TestModelNameDotHyphenEquivalent(t *testing.T) {
	book := sonnetBook()
	for _, name := range []string{"claude-sonnet-5.5", "claude-sonnet-5-5", "CLAUDE-SONNET-5-5", " claude-sonnet-5.5 "} {
		charge, err := book.PriceCharge(name, TokenUsage{Input: 150_000, Output: 2_000}, time.Time{}, nil, DefaultSettings(), nil)
		if err != nil {
			t.Fatalf("model %q: %v", name, err)
		}
		if charge.AmountMinor != 179_595 {
			t.Fatalf("model %q amount=%d", name, charge.AmountMinor)
		}
	}
	hyphenBook := PriceBook{Version: "hyphen", Models: []PriceModel{{
		Model: "claude-sonnet-5-6",
		Tiers: []PriceTier{{Prices: map[string]ComponentPrice{
			ComponentInput: {RetailMinorPerMillion: 1_000_000},
		}}},
	}}}
	hyphenBook.Finalize()
	charge, err := hyphenBook.PriceCharge("claude-sonnet-5.6", TokenUsage{Input: 1_000}, time.Time{}, nil, DefaultSettings(), nil)
	if err != nil {
		t.Fatalf("dot query against hyphen book: %v", err)
	}
	if charge.AmountMinor != 1_000 {
		t.Fatalf("amount=%d", charge.AmountMinor)
	}
}
