package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Sandbox usage reporting. Sandbox providers (daytona/e2b behind dslpha and
// market) report the measured seconds and credits for one logical sandbox run.
// The console persists the breakdown in billing.sandbox_usage and debits the
// organization wallet in the same transaction, so a crash can never leave a
// charge without its detail row or a detail row without a charge.
//
// The row is keyed by the product's idempotency key: a retried report returns
// the original row and balance without a second debit. Charges are whole
// credits; the console stores 1000 minor units per credit (100 credits = ¥1).
const (
	SandboxCreditsMinorPerCredit int64 = 1000

	sandboxMaxProductLen  = 64
	sandboxMaxIDLen       = 200
	sandboxMaxProviderLen = 64
	sandboxMaxKeyLen      = 200
	sandboxMaxCurrencyLen = 8
	sandboxMaxUnitPrice   = 8192

	// numeric(14,4) tops out just below 1e10; reject earlier so a bad report is
	// a 400 instead of a database error.
	sandboxMaxCreditsComponent = 1e10
)

// ErrInvalidSandboxUsage is returned for a malformed sandbox usage report. The
// HTTP layer maps it to 400 before any row or ledger entry is written.
var ErrInvalidSandboxUsage = errors.New("invalid sandbox usage")

// SandboxUsageInput is the wire shape of POST /internal/v1/usage/sandbox.
// TotalCredits is optional: when omitted the total is the ceiling of the four
// component credits. UnitPrices is an opaque JSON snapshot kept for display.
type SandboxUsageInput struct {
	OrgID             string          `json:"org_id"`
	UserID            string          `json:"user_id"`
	Product           string          `json:"product"`
	SandboxID         string          `json:"sandbox_id"`
	Provider          string          `json:"provider"`
	ProviderSandboxID string          `json:"provider_sandbox_id"`
	RunID             string          `json:"run_id"`
	StartedAt         *time.Time      `json:"started_at"`
	EndedAt           *time.Time      `json:"ended_at"`
	BillableSeconds   int64           `json:"billable_seconds"`
	CPUSeconds        int64           `json:"cpu_seconds"`
	MemoryMiBSeconds  int64           `json:"memory_mib_seconds"`
	DiskMiBSeconds    int64           `json:"disk_mib_seconds"`
	GPUSeconds        int64           `json:"gpu_seconds"`
	CPUCredits        float64         `json:"cpu_credits"`
	MemoryCredits     float64         `json:"memory_credits"`
	DiskCredits       float64         `json:"disk_credits"`
	GPUCredits        float64         `json:"gpu_credits"`
	TotalCredits      *int64          `json:"total_credits"`
	UnitPrices        json.RawMessage `json:"unit_prices"`
	Currency          string          `json:"currency"`
	IdempotencyKey    string          `json:"idempotency_key"`
}

// SandboxUsage is a normalized, validated row ready to persist.
type SandboxUsage struct {
	ID                int64      `json:"id"`
	OrgID             string     `json:"org_id"`
	UserID            string     `json:"user_id,omitempty"`
	Product           string     `json:"product"`
	SandboxID         string     `json:"sandbox_id"`
	Provider          string     `json:"provider,omitempty"`
	ProviderSandboxID string     `json:"provider_sandbox_id,omitempty"`
	RunID             string     `json:"run_id,omitempty"`
	StartedAt         *time.Time `json:"started_at"`
	EndedAt           *time.Time `json:"ended_at"`
	BillableSeconds   int64      `json:"billable_seconds"`
	CPUSeconds        int64      `json:"cpu_seconds"`
	MemoryMiBSeconds  int64      `json:"memory_mib_seconds"`
	DiskMiBSeconds    int64      `json:"disk_mib_seconds"`
	GPUSeconds        int64      `json:"gpu_seconds"`
	CPUCredits        float64    `json:"cpu_credits"`
	MemoryCredits     float64    `json:"memory_credits"`
	DiskCredits       float64    `json:"disk_credits"`
	GPUCredits        float64    `json:"gpu_credits"`
	TotalCredits      int64      `json:"total_credits"`
	CreditsMinor      int64      `json:"credits_minor"`
	// GPUUnpriced marks a run that used GPU time while V2 has no configured
	// GPU unit price: the seconds are recorded, the component is not billed.
	GPUUnpriced    bool            `json:"gpu_unpriced,omitempty"`
	Currency       string          `json:"currency"`
	UnitPrices     json.RawMessage `json:"unit_prices,omitempty"`
	IdempotencyKey string          `json:"idempotency_key"`
	CreatedAt      time.Time       `json:"created_at"`
}

// SandboxChargeResult is the service response body. Replayed is true when the
// idempotency key already existed and nothing was debited by this call.
type SandboxChargeResult struct {
	Usage          SandboxUsage `json:"usage"`
	ChargedCredits int64        `json:"charged_credits"`
	BalanceCredits int64        `json:"balance_credits"`
	Replayed       bool         `json:"replayed"`
}

func invalidSandbox(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSandboxUsage, fmt.Sprintf(format, args...))
}

func trimSandboxText(name, value string, max int) (string, error) {
	out := strings.TrimSpace(value)
	if utf8.RuneCountInString(out) > max {
		return "", invalidSandbox("%s must be at most %d characters", name, max)
	}
	return out, nil
}

func checkSandboxSeconds(name string, value int64) error {
	if value < 0 {
		return invalidSandbox("%s must be >= 0", name)
	}
	return nil
}

func checkSandboxCredits(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return invalidSandbox("%s must be a finite number", name)
	}
	if value < 0 {
		return invalidSandbox("%s must be >= 0", name)
	}
	if value >= sandboxMaxCreditsComponent {
		return invalidSandbox("%s is out of range", name)
	}
	return nil
}

// NormalizeSandboxUsage validates the report and resolves the billed totals.
// The product is lower-cased to match the ledger's product keys; text ids keep
// their case. A malformed report never reaches a transaction.
func NormalizeSandboxUsage(in SandboxUsageInput) (SandboxUsage, error) {
	var err error
	u := SandboxUsage{
		OrgID:            strings.TrimSpace(in.OrgID),
		Product:          strings.ToLower(strings.TrimSpace(in.Product)),
		BillableSeconds:  in.BillableSeconds,
		CPUSeconds:       in.CPUSeconds,
		MemoryMiBSeconds: in.MemoryMiBSeconds,
		DiskMiBSeconds:   in.DiskMiBSeconds,
		GPUSeconds:       in.GPUSeconds,
		CPUCredits:       in.CPUCredits,
		MemoryCredits:    in.MemoryCredits,
		DiskCredits:      in.DiskCredits,
		GPUCredits:       in.GPUCredits,
		StartedAt:        in.StartedAt,
		EndedAt:          in.EndedAt,
		Currency:         strings.ToUpper(strings.TrimSpace(in.Currency)),
	}
	if u.OrgID == "" {
		return SandboxUsage{}, invalidSandbox("org_id is required")
	}
	if _, err := uuid.Parse(u.OrgID); err != nil {
		return SandboxUsage{}, invalidSandbox("org_id must be a uuid")
	}
	if u.Product == "" {
		return SandboxUsage{}, invalidSandbox("product is required")
	}
	if u.Product, err = trimSandboxText("product", u.Product, sandboxMaxProductLen); err != nil {
		return SandboxUsage{}, err
	}
	if u.SandboxID, err = trimSandboxText("sandbox_id", in.SandboxID, sandboxMaxIDLen); err != nil {
		return SandboxUsage{}, err
	}
	if u.SandboxID == "" {
		return SandboxUsage{}, invalidSandbox("sandbox_id is required")
	}
	if u.UserID, err = trimSandboxText("user_id", in.UserID, sandboxMaxIDLen); err != nil {
		return SandboxUsage{}, err
	}
	if u.Provider, err = trimSandboxText("provider", strings.ToLower(in.Provider), sandboxMaxProviderLen); err != nil {
		return SandboxUsage{}, err
	}
	if u.ProviderSandboxID, err = trimSandboxText("provider_sandbox_id", in.ProviderSandboxID, sandboxMaxIDLen); err != nil {
		return SandboxUsage{}, err
	}
	if u.RunID, err = trimSandboxText("run_id", in.RunID, sandboxMaxIDLen); err != nil {
		return SandboxUsage{}, err
	}
	if u.IdempotencyKey, err = trimSandboxText("idempotency_key", in.IdempotencyKey, sandboxMaxKeyLen); err != nil {
		return SandboxUsage{}, err
	}
	if u.IdempotencyKey == "" {
		return SandboxUsage{}, invalidSandbox("idempotency_key is required")
	}
	if u.Currency == "" {
		u.Currency = "CNY"
	}
	if utf8.RuneCountInString(u.Currency) > sandboxMaxCurrencyLen {
		return SandboxUsage{}, invalidSandbox("currency must be at most %d characters", sandboxMaxCurrencyLen)
	}

	for _, field := range []struct {
		name  string
		value int64
	}{
		{"billable_seconds", u.BillableSeconds},
		{"cpu_seconds", u.CPUSeconds},
		{"memory_mib_seconds", u.MemoryMiBSeconds},
		{"disk_mib_seconds", u.DiskMiBSeconds},
		{"gpu_seconds", u.GPUSeconds},
	} {
		if err := checkSandboxSeconds(field.name, field.value); err != nil {
			return SandboxUsage{}, err
		}
	}
	for _, field := range []struct {
		name  string
		value float64
	}{
		{"cpu_credits", u.CPUCredits},
		{"memory_credits", u.MemoryCredits},
		{"disk_credits", u.DiskCredits},
		{"gpu_credits", u.GPUCredits},
	} {
		if err := checkSandboxCredits(field.name, field.value); err != nil {
			return SandboxUsage{}, err
		}
	}

	if u.StartedAt != nil && u.EndedAt != nil && u.EndedAt.Before(*u.StartedAt) {
		return SandboxUsage{}, invalidSandbox("ended_at must not be before started_at")
	}

	// unit_prices is a display snapshot. Keep it opaquely as jsonb but require
	// a valid JSON object so the detail page can render it as key/value pairs.
	if len(in.UnitPrices) > 0 && string(in.UnitPrices) != "null" {
		if len(in.UnitPrices) > sandboxMaxUnitPrice {
			return SandboxUsage{}, invalidSandbox("unit_prices is too large")
		}
		if !json.Valid(in.UnitPrices) {
			return SandboxUsage{}, invalidSandbox("unit_prices must be valid json")
		}
		trimmed := strings.TrimSpace(string(in.UnitPrices))
		if !strings.HasPrefix(trimmed, "{") {
			return SandboxUsage{}, invalidSandbox("unit_prices must be a json object")
		}
		u.UnitPrices = json.RawMessage(trimmed)
	}

	if in.TotalCredits != nil {
		if *in.TotalCredits <= 0 {
			return SandboxUsage{}, invalidSandbox("total_credits must be > 0")
		}
		u.TotalCredits = *in.TotalCredits
	} else {
		// A V2 product reports raw usage only; the console prices it from
		// settings after normalization. Client component credits are kept for
		// the audit snapshot but never drive the charge.
		components := u.CPUCredits + u.MemoryCredits + u.DiskCredits + u.GPUCredits
		if components > 0 {
			u.TotalCredits = int64(math.Ceil(components))
		}
	}
	if u.TotalCredits > math.MaxInt64/SandboxCreditsMinorPerCredit {
		return SandboxUsage{}, invalidSandbox("total_credits is out of range")
	}
	u.CreditsMinor = u.TotalCredits * SandboxCreditsMinorPerCredit
	return u, nil
}

// Daytona sandbox unit prices (USD) from the billing V2 plan. They are the
// authoritative cost basis: products no longer supply unit prices.
const (
	sandboxCPUUSDPerCoreHour  = 0.0504
	sandboxMemoryUSDPerGBHour = 0.0162
	sandboxDiskUSDPerGBHour   = 0.000108
)

func roundSandboxCredits(value float64) float64 {
	return math.Round(value*10000) / 10000
}

// PriceSandboxUsage replaces the product-reported credit figures with the
// console's own calculation: usage seconds × Daytona USD unit price ×
// settings.sandbox.markup_bps × settings.fx.usd_to_credits, ceil'd to whole
// credits. The reported values stay available in UnitPrices for the audit
// difference report; GPU time is recorded unpriced instead of being billed at
// an invented rate.
func PriceSandboxUsage(u SandboxUsage, settings Settings) (SandboxUsage, error) {
	hours := float64(u.BillableSeconds) / 3600
	if hours <= 0 {
		hours = math.Max(
			math.Max(float64(u.CPUSeconds), float64(u.MemoryMiBSeconds)/1024),
			float64(u.DiskMiBSeconds)/1024,
		) / 3600
	}
	cpuUSD := float64(u.CPUSeconds) / 3600 * sandboxCPUUSDPerCoreHour
	memoryUSD := float64(u.MemoryMiBSeconds) / 1024 / 3600 * sandboxMemoryUSDPerGBHour
	diskGBHours := float64(u.DiskMiBSeconds) / 1024 / 3600
	freeGBHours := float64(settings.SandboxDiskFreeGiB) * hours
	diskUSD := math.Max(0, diskGBHours-freeGBHours) * sandboxDiskUSDPerGBHour
	totalUSD := cpuUSD + memoryUSD + diskUSD
	markup := float64(settings.SandboxMarkupBPS) / 10000
	fx := float64(settings.FXUSDToCredits)

	u.GPUUnpriced = u.GPUSeconds > 0
	u.CPUCredits = roundSandboxCredits(cpuUSD * markup * fx)
	u.MemoryCredits = roundSandboxCredits(memoryUSD * markup * fx)
	u.DiskCredits = roundSandboxCredits(diskUSD * markup * fx)
	u.GPUCredits = 0
	credits := int64(math.Ceil(totalUSD * markup * fx))
	if credits <= 0 {
		return u, invalidSandbox("no billable sandbox usage")
	}
	u.TotalCredits = credits
	u.CreditsMinor = credits * SandboxCreditsMinorPerCredit
	return u, nil
}

type sandboxUsageQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// querySandboxUsageByKey loads an existing row for the idempotent replay path.
func querySandboxUsageByKey(ctx context.Context, q sandboxUsageQuerier, key string) (SandboxUsage, bool, error) {
	var u SandboxUsage
	var rawUnitPrices string
	err := q.QueryRow(ctx, `
		select id,org_id::text,coalesce(user_id,''),product,sandbox_id,
		       coalesce(provider,''),coalesce(provider_sandbox_id,''),coalesce(run_id,''),
		       started_at,ended_at,billable_seconds,cpu_seconds,memory_mib_seconds,
		       disk_mib_seconds,gpu_seconds,cpu_credits::float8,memory_credits::float8,
		       disk_credits::float8,gpu_credits::float8,total_credits,credits_minor,
		       currency,coalesce(unit_prices::text,''),idempotency_key,created_at
		from billing.sandbox_usage where idempotency_key=$1`, key).
		Scan(&u.ID, &u.OrgID, &u.UserID, &u.Product, &u.SandboxID,
			&u.Provider, &u.ProviderSandboxID, &u.RunID,
			&u.StartedAt, &u.EndedAt, &u.BillableSeconds, &u.CPUSeconds, &u.MemoryMiBSeconds,
			&u.DiskMiBSeconds, &u.GPUSeconds, &u.CPUCredits, &u.MemoryCredits,
			&u.DiskCredits, &u.GPUCredits, &u.TotalCredits, &u.CreditsMinor,
			&u.Currency, &rawUnitPrices, &u.IdempotencyKey, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SandboxUsage{}, false, nil
	}
	if err != nil {
		return SandboxUsage{}, false, err
	}
	if rawUnitPrices != "" {
		u.UnitPrices = json.RawMessage(rawUnitPrices)
		var snapshot struct {
			GPUUnpriced bool `json:"gpu_unpriced"`
		}
		if err := json.Unmarshal(u.UnitPrices, &snapshot); err == nil {
			u.GPUUnpriced = snapshot.GPUUnpriced
		}
	}
	return u, true, nil
}

func nullSandboxText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullSandboxJSON(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

// RecordSandboxUsage persists one sandbox report and debits the wallet. The row
// insert, ledger entry and wallet debit commit together. A replay of the same
// idempotency key returns the original row and the current balance without a
// second debit. Insufficient credits returns ErrInsufficientCredits and rolls
// the insert back, so a refused report leaves no trace.
func (s *Store) RecordSandboxUsage(ctx context.Context, in SandboxUsageInput) (SandboxChargeResult, error) {
	u, err := NormalizeSandboxUsage(in)
	if err != nil {
		return SandboxChargeResult{}, err
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		return SandboxChargeResult{}, err
	}
	reported := map[string]any{
		"reported_total_credits":  in.TotalCredits,
		"reported_cpu_credits":    in.CPUCredits,
		"reported_memory_credits": in.MemoryCredits,
		"reported_disk_credits":   in.DiskCredits,
		"reported_gpu_credits":    in.GPUCredits,
		"reported_unit_prices":    json.RawMessage(in.UnitPrices),
	}
	u, err = PriceSandboxUsage(u, settings)
	if err != nil {
		return SandboxChargeResult{}, err
	}
	// Keep the product's report next to the authoritative rate snapshot for
	// the transition difference report; unit_prices stays display-only.
	snapshot := map[string]any{}
	if len(u.UnitPrices) > 0 {
		_ = json.Unmarshal(u.UnitPrices, &snapshot)
	}
	snapshot["sandbox_markup_bps"] = settings.SandboxMarkupBPS
	snapshot["fx_usd_to_credits"] = settings.FXUSDToCredits
	snapshot["gpu_unpriced"] = u.GPUUnpriced
	snapshot["authoritative_cpu_credits"] = u.CPUCredits
	snapshot["authoritative_memory_credits"] = u.MemoryCredits
	snapshot["authoritative_disk_credits"] = u.DiskCredits
	snapshot["authoritative_total_credits"] = u.TotalCredits
	for key, value := range reported {
		if raw, ok := value.(json.RawMessage); ok {
			if len(raw) > 0 && string(raw) != "null" {
				snapshot[key] = raw
			}
			continue
		}
		snapshot[key] = value
	}
	if merged, mErr := json.Marshal(snapshot); mErr == nil {
		u.UnitPrices = merged
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return SandboxChargeResult{}, err
	}
	defer tx.Rollback(ctx)

	// Fast replay path before taking the wallet lock.
	if existing, found, err := querySandboxUsageByKey(ctx, tx, u.IdempotencyKey); err != nil {
		return SandboxChargeResult{}, err
	} else if found {
		if existing.OrgID != u.OrgID {
			return SandboxChargeResult{}, invalidSandbox("idempotency_key is already used by another organization")
		}
		var balance int64
		if err := tx.QueryRow(ctx, `
			select coalesce((select balance_credits from billing.wallets where org_id=$1),0)`,
			u.OrgID).Scan(&balance); err != nil {
			return SandboxChargeResult{}, err
		}
		return SandboxChargeResult{Usage: existing, ChargedCredits: existing.TotalCredits, BalanceCredits: balance, Replayed: true}, tx.Commit(ctx)
	}

	var balance int64
	if err := tx.QueryRow(ctx, `
		select balance_credits from billing.wallets where org_id=$1 for update`,
		u.OrgID).Scan(&balance); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SandboxChargeResult{}, ErrNotFound
		}
		return SandboxChargeResult{}, err
	}

	// Insert before the balance check: a concurrent replay of the same key must
	// be recognized as a replay even if the wallet has since been drained.
	var id int64
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		insert into billing.sandbox_usage
			(org_id,user_id,product,sandbox_id,provider,provider_sandbox_id,run_id,
			 started_at,ended_at,billable_seconds,cpu_seconds,memory_mib_seconds,
			 disk_mib_seconds,gpu_seconds,cpu_credits,memory_credits,disk_credits,
			 gpu_credits,total_credits,credits_minor,currency,unit_prices,idempotency_key)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		on conflict (idempotency_key) do nothing
		returning id, created_at`,
		u.OrgID, nullSandboxText(u.UserID), u.Product, u.SandboxID,
		nullSandboxText(u.Provider), nullSandboxText(u.ProviderSandboxID), nullSandboxText(u.RunID),
		u.StartedAt, u.EndedAt, u.BillableSeconds, u.CPUSeconds, u.MemoryMiBSeconds,
		u.DiskMiBSeconds, u.GPUSeconds, u.CPUCredits, u.MemoryCredits, u.DiskCredits,
		u.GPUCredits, u.TotalCredits, u.CreditsMinor, u.Currency,
		nullSandboxJSON(u.UnitPrices), u.IdempotencyKey).
		Scan(&id, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another transaction inserted this key while we waited for the wallet
		// lock. No debit happened here; return the winner's row.
		existing, found, qerr := querySandboxUsageByKey(ctx, tx, u.IdempotencyKey)
		if qerr != nil {
			return SandboxChargeResult{}, qerr
		}
		if !found {
			return SandboxChargeResult{}, errors.New("sandbox usage: idempotency conflict without a row")
		}
		if existing.OrgID != u.OrgID {
			return SandboxChargeResult{}, invalidSandbox("idempotency_key is already used by another organization")
		}
		if qerr := tx.QueryRow(ctx, `
			select balance_credits from billing.wallets where org_id=$1`, u.OrgID).Scan(&balance); qerr != nil {
			return SandboxChargeResult{}, qerr
		}
		return SandboxChargeResult{Usage: existing, ChargedCredits: existing.TotalCredits, BalanceCredits: balance, Replayed: true}, tx.Commit(ctx)
	}
	if err != nil {
		return SandboxChargeResult{}, err
	}

	if balance < u.TotalCredits {
		return SandboxChargeResult{}, ErrInsufficientCredits
	}

	if _, err := tx.Exec(ctx, `
		update billing.wallets
		set balance_credits=balance_credits-$2,version=version+1,updated_at=now()
		where org_id=$1`, u.OrgID, u.TotalCredits); err != nil {
		return SandboxChargeResult{}, err
	}

	refID := u.RunID
	if refID == "" {
		refID = u.SandboxID
	}
	tag, err := tx.Exec(ctx, `
		insert into billing.ledger (org_id,product,delta_credits,reason,ref_id,idempotency_key)
		values ($1,$2,$3,'sandbox_usage',$4,$5)
		on conflict (idempotency_key) do nothing`,
		u.OrgID, u.Product, -u.TotalCredits, refID, "sandbox:"+u.IdempotencyKey)
	if err != nil {
		return SandboxChargeResult{}, err
	}
	if tag.RowsAffected() == 0 {
		// A ledger key without its sandbox row would mean the two streams had
		// drifted; refuse rather than debit without a matching charge record.
		return SandboxChargeResult{}, errors.New("sandbox usage: ledger idempotency conflict")
	}

	if err := tx.Commit(ctx); err != nil {
		return SandboxChargeResult{}, err
	}
	u.ID = id
	u.CreatedAt = createdAt
	return SandboxChargeResult{
		Usage:          u,
		ChargedCredits: u.TotalCredits,
		BalanceCredits: balance - u.TotalCredits,
	}, nil
}
