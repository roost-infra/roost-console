package billing

import (
	"encoding/csv"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPriceBookSeedGolden validates an exported price-book seed CSV against the
// contract's golden numbers before it is imported. It is skipped unless
// ROOST_PRICE_BOOK_SEED_CSV points at the seed (for example
// worktrees/seeds/price-book-v1.csv).
func TestPriceBookSeedGolden(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("ROOST_PRICE_BOOK_SEED_CSV"))
	if path == "" {
		t.Skip("set ROOST_PRICE_BOOK_SEED_CSV to validate an exported price-book seed")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	defer file.Close()
	rows, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("seed has %d rows", len(rows))
	}
	header := map[string]int{}
	for i, name := range rows[0] {
		header[strings.TrimSpace(name)] = i
	}
	column := func(row []string, name string) string {
		index, ok := header[name]
		if !ok || index >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[index])
	}
	book := PriceBook{Version: "seed", EffectiveAt: time.Date(2026, 11, 25, 0, 0, 0, 0, cstZone)}
	modelIndex := map[string]int{}
	for _, row := range rows[1:] {
		model := column(row, "model")
		if model == "" {
			continue
		}
		idx, ok := modelIndex[model]
		if !ok {
			idx = len(book.Models)
			modelIndex[model] = idx
			book.Models = append(book.Models, PriceModel{Model: model, Currency: "CREDIT"})
		}
		tierMax, err := strconv.ParseInt(column(row, "tier_max_input"), 10, 64)
		if err != nil {
			t.Fatalf("tier for %s: %v", model, err)
		}
		retail, err := strconv.ParseInt(column(row, "retail_minor"), 10, 64)
		if err != nil {
			t.Fatalf("retail for %s: %v", model, err)
		}
		cost, err := strconv.ParseInt(column(row, "cost_minor"), 10, 64)
		if err != nil {
			t.Fatalf("cost for %s: %v", model, err)
		}
		entry := &book.Models[idx]
		var tier *PriceTier
		for i := range entry.Tiers {
			if entry.Tiers[i].MaxInput == tierMax {
				tier = &entry.Tiers[i]
				break
			}
		}
		if tier == nil {
			entry.Tiers = append(entry.Tiers, PriceTier{MaxInput: tierMax, Prices: map[string]ComponentPrice{}})
			tier = &entry.Tiers[len(entry.Tiers)-1]
		}
		tier.Prices[column(row, "component")] = ComponentPrice{
			RetailMinorPerMillion: retail,
			CostMinorPerMillion:   cost,
			CostCurrency:          "CNY",
		}
	}
	book.Finalize()

	settings := DefaultSettings()
	plain, err := book.PriceCharge("claude-sonnet-5.5", TokenUsage{Input: 150_000, Output: 2_000}, time.Time{}, nil, settings, nil)
	if err != nil {
		t.Fatalf("seed plain price: %v", err)
	}
	if plain.AmountMinor != 179_595 || plain.AmountCredits != 180 {
		t.Fatalf("seed plain = %d minor / %d credits, want 179595 / 180", plain.AmountMinor, plain.AmountCredits)
	}
	compressed, err := book.PriceCharge("claude-sonnet-5.5", TokenUsage{Input: 150_000, Output: 2_000}, time.Time{}, &CompressionBasis{
		Mode: "original", BeforeInputTokens: 150_000, AfterInputTokens: 90_000, SummaryCostMinor: 20,
	}, settings, nil)
	if err != nil {
		t.Fatalf("seed compressed price: %v", err)
	}
	if compressed.AmountMinor != 165_283 || compressed.AmountCredits != 166 {
		t.Fatalf("seed compressed = %d minor / %d credits, want 165283 / 166", compressed.AmountMinor, compressed.AmountCredits)
	}
	if compressed.Compression == nil || compressed.Compression.SavedTokens != 63_000 || compressed.Compression.UserShareTokens != 15_750 {
		t.Fatalf("seed compression=%+v", compressed.Compression)
	}
}
