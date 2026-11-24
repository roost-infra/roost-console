package billing

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validSandboxInput() SandboxUsageInput {
	return SandboxUsageInput{
		OrgID:             uuid.NewString(),
		UserID:            "user-1",
		Product:           "market",
		SandboxID:         "sbx-1",
		Provider:          "daytona",
		ProviderSandboxID: "daytona-1",
		RunID:             "run-1",
		BillableSeconds:   3661,
		CPUSeconds:        3661,
		MemoryMiBSeconds:  3661 * 1024,
		DiskMiBSeconds:    3661 * 256,
		GPUSeconds:        0,
		CPUCredits:        0.5,
		MemoryCredits:     1.25,
		DiskCredits:       0.25,
		Currency:          "cny",
		UnitPrices:        json.RawMessage(`{"cpu_credits_per_second":0.0001}`),
		IdempotencyKey:    "sbx-run-1",
	}
}

func TestNormalizeSandboxUsageDerivesTotalAndMinor(t *testing.T) {
	in := validSandboxInput()
	in.Product = "  Market  "
	in.IdempotencyKey = "  sbx-run-1  "
	started := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	ended := started.Add(time.Hour)
	in.StartedAt, in.EndedAt = &started, &ended

	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	// 0.5 + 1.25 + 0.25 = 2.0 credits.
	if u.TotalCredits != 2 || u.CreditsMinor != 2*SandboxCreditsMinorPerCredit {
		t.Fatalf("total=%d minor=%d", u.TotalCredits, u.CreditsMinor)
	}
	if u.Product != "market" || u.IdempotencyKey != "sbx-run-1" {
		t.Fatalf("normalized product=%q key=%q", u.Product, u.IdempotencyKey)
	}
	if u.Currency != "CNY" {
		t.Fatalf("currency=%q want CNY", u.Currency)
	}
	if string(u.UnitPrices) != `{"cpu_credits_per_second":0.0001}` {
		t.Fatalf("unit prices=%s", u.UnitPrices)
	}
	if u.BillableSeconds != 3661 || u.CPUSeconds != 3661 {
		t.Fatalf("seconds=%d cpu=%d", u.BillableSeconds, u.CPUSeconds)
	}
}

func TestNormalizeSandboxUsageRoundsComponentTotalUp(t *testing.T) {
	in := validSandboxInput()
	in.CPUCredits, in.MemoryCredits, in.DiskCredits, in.GPUCredits = 0.1, 0.1, 0.1, 0.1
	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	// 0.4 credits still bills one whole credit rather than zero.
	if u.TotalCredits != 1 || u.CreditsMinor != 1000 {
		t.Fatalf("total=%d minor=%d", u.TotalCredits, u.CreditsMinor)
	}
}

func TestNormalizeSandboxUsageExplicitTotalWins(t *testing.T) {
	in := validSandboxInput()
	five := int64(5)
	in.TotalCredits = &five
	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if u.TotalCredits != 5 || u.CreditsMinor != 5000 {
		t.Fatalf("total=%d minor=%d", u.TotalCredits, u.CreditsMinor)
	}
}

func TestNormalizeSandboxUsageRejectsInvalid(t *testing.T) {
	zero := int64(0)
	negativeTotal := int64(-2)
	tooBig := int64(math.MaxInt64/SandboxCreditsMinorPerCredit + 1)
	started := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	before := started.Add(-time.Minute)

	cases := map[string]func(in *SandboxUsageInput){
		"missing org":            func(in *SandboxUsageInput) { in.OrgID = "" },
		"bad org uuid":           func(in *SandboxUsageInput) { in.OrgID = "org-1" },
		"missing product":        func(in *SandboxUsageInput) { in.Product = "  " },
		"missing sandbox":        func(in *SandboxUsageInput) { in.SandboxID = "" },
		"missing key":            func(in *SandboxUsageInput) { in.IdempotencyKey = " " },
		"long key":               func(in *SandboxUsageInput) { in.IdempotencyKey = strings.Repeat("k", sandboxMaxKeyLen+1) },
		"long sandbox":           func(in *SandboxUsageInput) { in.SandboxID = strings.Repeat("s", sandboxMaxIDLen+1) },
		"negative seconds":       func(in *SandboxUsageInput) { in.CPUSeconds = -1 },
		"negative credits":       func(in *SandboxUsageInput) { in.CPUCredits = -0.5 },
		"nan credits":            func(in *SandboxUsageInput) { in.CPUCredits = math.NaN() },
		"infinite credits":       func(in *SandboxUsageInput) { in.CPUCredits = math.Inf(1) },
		"component out of range": func(in *SandboxUsageInput) { in.CPUCredits = sandboxMaxCreditsComponent },
		"zero total":             func(in *SandboxUsageInput) { in.TotalCredits = &zero },
		"negative total":         func(in *SandboxUsageInput) { in.TotalCredits = &negativeTotal },
		"total overflow":         func(in *SandboxUsageInput) { in.TotalCredits = &tooBig },
		"ended before started":   func(in *SandboxUsageInput) { in.StartedAt, in.EndedAt = &started, &before },
		"unit prices array":      func(in *SandboxUsageInput) { in.UnitPrices = json.RawMessage(`[1,2]`) },
		"unit prices invalid":    func(in *SandboxUsageInput) { in.UnitPrices = json.RawMessage(`{`) },
		"long currency":          func(in *SandboxUsageInput) { in.Currency = strings.Repeat("C", sandboxMaxCurrencyLen+1) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := validSandboxInput()
			mutate(&in)
			if _, err := NormalizeSandboxUsage(in); !errors.Is(err, ErrInvalidSandboxUsage) {
				t.Fatalf("err=%v want ErrInvalidSandboxUsage", err)
			}
		})
	}
}

func TestNormalizeSandboxUsageDefaultsCurrency(t *testing.T) {
	in := validSandboxInput()
	in.Currency = ""
	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if u.Currency != "CNY" {
		t.Fatalf("currency=%q", u.Currency)
	}
}

// TestNormalizeSandboxUsageAllowsUsageOnlyReport proves a V2 product may send
// raw usage with no client-side credits: pricing happens after normalization.
func TestNormalizeSandboxUsageAllowsUsageOnlyReport(t *testing.T) {
	in := validSandboxInput()
	in.CPUCredits, in.MemoryCredits, in.DiskCredits, in.GPUCredits = 0, 0, 0, 0
	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if u.TotalCredits != 0 || u.CreditsMinor != 0 {
		t.Fatalf("total=%d minor=%d want 0 before pricing", u.TotalCredits, u.CreditsMinor)
	}
}

// TestPriceSandboxUsageFromSettings locks the server-side calculation: usage
// seconds × Daytona USD rates × settings markup × fx, ceil to credits. The
// product-reported credits must not influence the charge.
func TestPriceSandboxUsageFromSettings(t *testing.T) {
	in := validSandboxInput()
	in.BillableSeconds = 3600
	in.CPUSeconds = 3600
	in.MemoryMiBSeconds = 3600 * 1024
	in.DiskMiBSeconds = 0
	in.GPUSeconds = 0
	in.CPUCredits, in.MemoryCredits, in.DiskCredits, in.GPUCredits = 999, 999, 999, 999
	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	priced, err := PriceSandboxUsage(u, DefaultSettings())
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	// cpu: 1 core-hour × $0.0504, memory: 1 GiB-hour × $0.0162.
	// 0.0666 USD × 1.3 × 720 = 62.3376 -> 63 credits.
	if priced.TotalCredits != 63 || priced.CreditsMinor != 63000 {
		t.Fatalf("total=%d minor=%d want 63/63000", priced.TotalCredits, priced.CreditsMinor)
	}
	if priced.CPUCredits != 47.1744 || priced.MemoryCredits != 15.1632 {
		t.Fatalf("cpu=%v memory=%v", priced.CPUCredits, priced.MemoryCredits)
	}
	if priced.DiskCredits != 0 {
		t.Fatalf("disk credits=%v want 0 (within the 5 GiB free tier)", priced.DiskCredits)
	}
	if priced.GPUUnpriced {
		t.Fatal("gpu marked unpriced without gpu seconds")
	}
}

func TestPriceSandboxUsageMarksUnpricedGPU(t *testing.T) {
	in := validSandboxInput()
	in.BillableSeconds = 3600
	in.CPUSeconds = 3600
	in.MemoryMiBSeconds = 3600 * 1024
	in.DiskMiBSeconds = 0
	in.GPUSeconds = 1800
	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	priced, err := PriceSandboxUsage(u, DefaultSettings())
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if !priced.GPUUnpriced {
		t.Fatal("gpu seconds recorded but gpu_unpriced is false")
	}
	if priced.TotalCredits != 63 || priced.GPUCredits != 0 {
		t.Fatalf("total=%d gpu=%v want 63/0", priced.TotalCredits, priced.GPUCredits)
	}
}

func TestPriceSandboxUsageExceedsClientTotal(t *testing.T) {
	// A tiny report that the client priced at 0 still bills the one-credit
	// minimum once the console computes a positive USD cost.
	in := validSandboxInput()
	in.BillableSeconds = 1
	in.CPUSeconds = 1
	in.MemoryMiBSeconds = 0
	in.DiskMiBSeconds = 0
	in.GPUSeconds = 0
	in.CPUCredits, in.MemoryCredits, in.DiskCredits, in.GPUCredits = 0, 0, 0, 0
	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	priced, err := PriceSandboxUsage(u, DefaultSettings())
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if priced.TotalCredits != 1 {
		t.Fatalf("total=%d want 1 (ceil of a sub-credit charge)", priced.TotalCredits)
	}
}

func TestPriceSandboxUsageRejectsEmptyReport(t *testing.T) {
	in := validSandboxInput()
	in.BillableSeconds = 0
	in.CPUSeconds = 0
	in.MemoryMiBSeconds = 0
	in.DiskMiBSeconds = 0
	in.GPUSeconds = 0
	in.CPUCredits, in.MemoryCredits, in.DiskCredits, in.GPUCredits = 0, 0, 0, 0
	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if _, err := PriceSandboxUsage(u, DefaultSettings()); !errors.Is(err, ErrInvalidSandboxUsage) {
		t.Fatalf("err=%v want ErrInvalidSandboxUsage", err)
	}
}
