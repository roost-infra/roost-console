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
	if len(p.Models) != 39 {
		t.Fatalf("models = %d, want 39 non-peak rows", len(p.Models))
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
