package provider

import (
	"os"
	"strings"
	"testing"
)

func loadFixture(t *testing.T) Pricebook {
	t.Helper()
	f, err := os.Open("testdata/pricebook.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := LoadPricebook(f)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPricebookAndQuote(t *testing.T) {
	p := loadFixture(t)
	if p.Version != "2026-11-25.1" || p.Plan.Basis != BasisProvider {
		t.Fatalf("unexpected pricebook header: %+v", p)
	}
	if len(p.Models) != 40 {
		t.Fatalf("models = %d, want 40 rows", len(p.Models))
	}
	c := p.Calculator()
	q, err := c.Quote(Request{
		Model: "gpt-6-astra",
		Usage: Usage{OriginalInput: 10_000, Input: 5_000, RetailTierInput: 10_000, ProviderTierInput: 5_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.RetailMinor != 34_000 || q.ProviderMinor != 6_800 || q.AllocatedMinor != 3_831 {
		t.Fatalf("retail/provider/allocated = %d/%d/%d", q.RetailMinor, q.ProviderMinor, q.AllocatedMinor)
	}
}

func TestLoadPricebookRejectsDuplicateTier(t *testing.T) {
	doc := `{"version":"v","models":[
		{"model":"m","max_input":0,"provider":{"input":1},"retail":{"input":2}},
		{"model":"m","max_input":0,"provider":{"input":1},"retail":{"input":2}}
	]}`
	if _, err := LoadPricebook(stringsReader(doc)); err == nil {
		t.Fatal("duplicate tier must fail")
	}
}

func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

func TestPeakTierFromPricebook(t *testing.T) {
	c := loadFixture(t).Calculator()
	base, err := c.Quote(Request{Model: "deepseek-v4-pro", Usage: Usage{OriginalInput: 1_000, Input: 1_000}})
	if err != nil {
		t.Fatal(err)
	}
	peak, err := c.Quote(Request{Model: "deepseek-v4-pro", Peak: "高峰", Usage: Usage{OriginalInput: 1_000, Input: 1_000}})
	if err != nil {
		t.Fatal(err)
	}
	if peak.RetailPrice.Input != 1_320_000 || base.RetailPrice.Input != 660_000 {
		t.Fatalf("base/peak retail input = %d/%d, want 660000/1320000", base.RetailPrice.Input, peak.RetailPrice.Input)
	}
}

func TestUnitPrices(t *testing.T) {
	p := loadFixture(t)
	img, err := p.QuoteUnit("image", "gpt-image-2.5", "")
	if err != nil {
		t.Fatal(err)
	}
	if img.ProviderMinor != 6_800 || img.RetailMinor != 17_000 || img.MarginBPS != 6_000 {
		t.Fatalf("image quote = %+v", img)
	}
	video, err := p.QuoteUnit("video", "MiniMax-H3", "1080P")
	if err != nil {
		t.Fatal(err)
	}
	if video.ProviderMinor != 39_984 || video.RetailMinor != 99_960 {
		t.Fatalf("video quote = %+v", video)
	}
	if _, err := p.QuoteUnit("video", "MiniMax-H3", "missing"); err == nil {
		t.Fatal("missing video tier must error")
	}
}
