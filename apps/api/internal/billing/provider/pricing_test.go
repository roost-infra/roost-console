package provider

import "testing"

func fixture() Calculator {
	return Calculator{
		Retail: Book{
			"gpt-6-astra": {{MaxInput: 0, Price: Price{Input: 5_000_000, Output: 25_000_000}}},
		},
		Provider: Book{
			"gpt-6-astra": {{MaxInput: 0, Price: Price{Input: 2_000_000, Output: 10_000_000, CacheRead: 200_000, CacheWrite: 2_500_000}}},
		},
		Official: Book{
			"gpt-6-astra": {{MaxInput: 0, Price: Price{Input: 10_000_000, Output: 50_000_000, CacheRead: 1_000_000, CacheWrite: 12_500_000}}},
		},
		Plan: Plan{CostMicros: 169_000_000, AllowanceMicros: 300_000_000, Basis: BasisProvider},
	}
}

func TestQuoteChargesPreCompressionInputAndCompressedCost(t *testing.T) {
	q, err := fixture().Quote(Request{
		Model: "gpt-6-astra",
		Usage: Usage{OriginalInput: 10_000, Input: 5_000, Output: 0, RetailTierInput: 10_000, ProviderTierInput: 5_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.RetailMinor != 34_000 || q.RetailCredits != 34 {
		t.Fatalf("retail minor/credits = %d/%d, want 34000/34", q.RetailMinor, q.RetailCredits)
	}
	if q.ProviderMinor != 6_800 {
		t.Fatalf("provider minor = %d, want 6800", q.ProviderMinor)
	}
	if q.AllocatedMinor != 3_831 {
		t.Fatalf("allocated minor = %d, want 3831", q.AllocatedMinor)
	}
	if q.GrossMinor != 30_169 {
		t.Fatalf("gross minor = %d, want 30169", q.GrossMinor)
	}
	if q.MarginBPS != 8_873 {
		t.Fatalf("margin = %d bps, want 8873", q.MarginBPS)
	}
}

func TestQuoteNoCompressionUsesSameOriginalAndProviderInput(t *testing.T) {
	q, err := fixture().Quote(Request{
		Model: "gpt-6-astra",
		Usage: Usage{OriginalInput: 10_000, Input: 10_000, Output: 0, RetailTierInput: 10_000, ProviderTierInput: 10_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.ProviderMinor != 13_600 || q.AllocatedMinor != 7_661 || q.GrossMinor != 26_339 {
		t.Fatalf("provider/allocated/gross = %d/%d/%d, want 13600/7661/26339", q.ProviderMinor, q.AllocatedMinor, q.GrossMinor)
	}
	if q.MarginBPS != 7_747 {
		t.Fatalf("margin = %d bps, want 7747", q.MarginBPS)
	}
}

func TestQuoteDoesNotChargeUserForCache(t *testing.T) {
	q, err := fixture().Quote(Request{
		Model: "gpt-6-astra",
		Usage: Usage{
			OriginalInput:     10_000,
			Input:             2_000,
			CacheRead:         3_000,
			CacheWrite:        0,
			RetailTierInput:   10_000,
			ProviderTierInput: 5_000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.RetailMinor != 34_000 {
		t.Fatalf("retail = %d, want 34000 (original input only)", q.RetailMinor)
	}
	if q.ProviderMinor != 3_128 || q.AllocatedMinor != 1_762 {
		t.Fatalf("provider/allocated = %d/%d, want 3128/1762", q.ProviderMinor, q.AllocatedMinor)
	}
	if q.GrossMinor != 32_238 {
		t.Fatalf("gross = %d, want 32238", q.GrossMinor)
	}
}

func TestQuoteOfficialBasisChangesCostBook(t *testing.T) {
	c := fixture()
	c.Plan.Basis = BasisOfficial
	q, err := c.Quote(Request{
		Model: "gpt-6-astra",
		Usage: Usage{OriginalInput: 10_000, Input: 5_000, RetailTierInput: 10_000, ProviderTierInput: 5_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.ProviderMinor != 34_000 {
		t.Fatalf("provider minor = %d, want 34000 under official basis", q.ProviderMinor)
	}
	if q.AllocatedMinor != 19_153 {
		t.Fatalf("allocated = %d, want 19153", q.AllocatedMinor)
	}
	if q.MarginBPS != 4_367 {
		t.Fatalf("margin = %d bps, want 4367", q.MarginBPS)
	}
}

func TestTierForPicksSmallestMatchingCeiling(t *testing.T) {
	tiers := ModelPrices{
		{MaxInput: 272_000, Price: Price{Input: 2_000_000}},
		{MaxInput: 0, Price: Price{Input: 4_000_000}},
	}
	got, err := tiers.TierFor(10_000)
	if err != nil || got.Price.Input != 2_000_000 {
		t.Fatalf("10k tier = %+v, %v; want standard", got, err)
	}
	got, err = tiers.TierFor(300_000)
	if err != nil || got.Price.Input != 4_000_000 {
		t.Fatalf("300k tier = %+v, %v; want long", got, err)
	}
}

func TestQuoteRequiresRetailAndProviderPrice(t *testing.T) {
	if _, err := fixture().Quote(Request{Model: "missing"}); err == nil {
		t.Fatal("missing model must error")
	}
	c := fixture()
	c.Provider = Book{}
	if _, err := c.Quote(Request{Model: "gpt-6-astra", Usage: Usage{OriginalInput: 1}}); err == nil {
		t.Fatal("missing provider price must error")
	}
}

func TestPlanAllocationRoundsToNearestMinor(t *testing.T) {
	q, err := fixture().Quote(Request{
		Model: "gpt-6-astra",
		Usage: Usage{OriginalInput: 10_000, Input: 5_000, RetailTierInput: 10_000, ProviderTierInput: 5_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.AllocatedMinor != 3_831 {
		t.Fatalf("allocated = %d, want 3831", q.AllocatedMinor)
	}
}

func TestPeakMultiplierAppliesToRetailAndProvider(t *testing.T) {
	base, err := fixture().Quote(Request{
		Model: "gpt-6-astra",
		Usage: Usage{OriginalInput: 10_000, Input: 5_000, RetailTierInput: 10_000, ProviderTierInput: 5_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	peak, err := fixture().Quote(Request{
		Model:   "gpt-6-astra",
		PeakBPS: 20_000,
		Usage:   Usage{OriginalInput: 10_000, Input: 5_000, RetailTierInput: 10_000, ProviderTierInput: 5_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak.RetailMinor != base.RetailMinor*2 || peak.ProviderMinor != base.ProviderMinor*2 {
		t.Fatalf("peak retail/provider = %d/%d, want 2x of %d/%d", peak.RetailMinor, peak.ProviderMinor, base.RetailMinor, base.ProviderMinor)
	}
	if peak.MarginBPS != base.MarginBPS {
		t.Fatalf("peak margin = %d, want unchanged %d", peak.MarginBPS, base.MarginBPS)
	}
}

func TestPricebookCacheDoesNotDoubleChargeUser(t *testing.T) {
	p := loadFixture(t)
	q, err := p.Calculator().Quote(Request{
		Model: "gpt-6-astra",
		Usage: Usage{
			OriginalInput:     10_000,
			Input:             2_000,
			CacheRead:         3_000,
			RetailTierInput:   10_000,
			ProviderTierInput: 5_000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.RetailMinor != 34_000 {
		t.Fatalf("retail = %d, want 34000 (original input only)", q.RetailMinor)
	}
	if q.ProviderMinor >= 6_800 {
		t.Fatalf("cache hit provider cost = %d, want below all-input cost 6800", q.ProviderMinor)
	}
}
