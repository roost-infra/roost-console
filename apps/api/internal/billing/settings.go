package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// Billing settings are the mutable knobs of the V2 pricing engine: the USD
// conversion used only by USD-metered products, the rounding policy, the
// compression revenue share and the sandbox markup. Every value is a JSON
// scalar stored in billing.settings so an operator can change one key without a
// migration. Reads always start from the contract defaults below, so a missing
// row never means "zero".
const (
	SettingFXUSDToCredits               = "fx.usd_to_credits"
	SettingCreditYuan                   = "billing.credit_yuan"
	SettingRounding                     = "billing.rounding"
	SettingMinChargeCredits             = "billing.min_charge_credits"
	SettingCompressionUserShareBPS      = "compression.user_share_bps"
	SettingCompressionMinNetGainCredits = "compression.min_net_gain_credits"
	SettingSandboxMarkupBPS             = "sandbox.markup_bps"
	SettingSandboxDiskFreeGiB           = "sandbox.disk_free_gib"
	SettingMemoryCallPriceCredits       = "memory.call.price_credits"
	SettingVersion                      = "settings.version"
	SettingToolsPrefix                  = "tools."
	SettingToolsSuffix                  = ".price_minor"

	// DefaultSettingsVersion is the settings version shipped by
	// 0015_pricing_v2.sql. It is only the fallback when the settings table has
	// no version row yet.
	DefaultSettingsVersion = "2026-11-25.1"
)

// settingsSnapshotMisses counts settlements that had to fall back to the live
// settings row because the lease's locked settings_version had no snapshot.
// The number is surfaced by SettingsSnapshotMisses for metrics and alerts.
var settingsSnapshotMisses atomic.Int64

// SettingsSnapshotMisses reports the number of settings snapshot fallbacks
// since process start.
func SettingsSnapshotMisses() int64 { return settingsSnapshotMisses.Load() }

// Settings is the resolved billing configuration. Tools maps a tool name to
// its per-call price in minor units (settings key tools.<name>.price_minor).
type Settings struct {
	FXUSDToCredits               int64            `json:"fx_usd_to_credits"`
	CreditYuan                   float64          `json:"credit_yuan"`
	Rounding                     string           `json:"rounding"`
	MinChargeCredits             int64            `json:"min_charge_credits"`
	CompressionUserShareBPS      int64            `json:"compression_user_share_bps"`
	CompressionMinNetGainCredits int64            `json:"compression_min_net_gain_credits"`
	SandboxMarkupBPS             int64            `json:"sandbox_markup_bps"`
	SandboxDiskFreeGiB           int64            `json:"sandbox_disk_free_gib"`
	MemoryCallPriceCredits       int64            `json:"memory_call_price_credits"`
	Tools                        map[string]int64 `json:"tools"`
	Version                      string           `json:"version"`
}

// DefaultSettings returns the contract defaults. Reads apply stored overrides
// on top of this value.
func DefaultSettings() Settings {
	return Settings{
		FXUSDToCredits:               720,
		CreditYuan:                   0.01,
		Rounding:                     "ceil",
		MinChargeCredits:             1,
		CompressionUserShareBPS:      2500,
		CompressionMinNetGainCredits: 1,
		SandboxMarkupBPS:             13000,
		SandboxDiskFreeGiB:           5,
		MemoryCallPriceCredits:       1,
		Tools:                        map[string]int64{"search": 7, "fetch": 7},
		Version:                      DefaultSettingsVersion,
	}
}

// ToolPriceMinor returns the configured minor-unit price of one tool call.
func (s Settings) ToolPriceMinor(tool string) (int64, bool) {
	name := strings.ToLower(strings.TrimSpace(tool))
	for key, price := range s.Tools {
		if strings.ToLower(key) == name {
			return price, true
		}
	}
	return 0, false
}

// FX mirrors the public price-book fx block: the USD conversion used only by
// USD-metered products and the yuan value of one credit.
type FXSettings struct {
	USDToCredits int64   `json:"usd_to_credits"`
	CreditYuan   float64 `json:"credit_yuan"`
}

// settingsQuerier is the pgx surface shared by *pgxpool.Pool and pgx.Tx.
type settingsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func jsonInt64(raw []byte) (int64, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return 0, err
	}
	switch v := value.(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, nil
		}
		f, err := v.Float64()
		if err != nil {
			return 0, err
		}
		return int64(f), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("not an integer: %s", strings.TrimSpace(string(raw)))
}

func jsonFloat64(raw []byte) (float64, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return 0, err
	}
	switch v := value.(type) {
	case json.Number:
		return v.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(v), 64)
	}
	return 0, fmt.Errorf("not a number: %s", strings.TrimSpace(string(raw)))
}

func jsonText(raw []byte) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

// apply overlays one stored settings row onto s. Unknown keys are ignored so a
// newer deployment can store keys an older binary does not understand.
func (s *Settings) apply(key string, raw []byte) error {
	var err error
	switch key {
	case SettingFXUSDToCredits:
		s.FXUSDToCredits, err = jsonInt64(raw)
	case SettingCreditYuan:
		s.CreditYuan, err = jsonFloat64(raw)
	case SettingRounding:
		s.Rounding, err = jsonText(raw)
	case SettingMinChargeCredits:
		s.MinChargeCredits, err = jsonInt64(raw)
	case SettingCompressionUserShareBPS:
		s.CompressionUserShareBPS, err = jsonInt64(raw)
	case SettingCompressionMinNetGainCredits:
		s.CompressionMinNetGainCredits, err = jsonInt64(raw)
	case SettingSandboxMarkupBPS:
		s.SandboxMarkupBPS, err = jsonInt64(raw)
	case SettingSandboxDiskFreeGiB:
		s.SandboxDiskFreeGiB, err = jsonInt64(raw)
	case SettingMemoryCallPriceCredits:
		s.MemoryCallPriceCredits, err = jsonInt64(raw)
	case SettingVersion:
		s.Version, err = jsonText(raw)
	default:
		if strings.HasPrefix(key, SettingToolsPrefix) && strings.HasSuffix(key, SettingToolsSuffix) {
			name := strings.TrimSuffix(strings.TrimPrefix(key, SettingToolsPrefix), SettingToolsSuffix)
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				return nil
			}
			var price int64
			price, err = jsonInt64(raw)
			if err == nil {
				if s.Tools == nil {
					s.Tools = map[string]int64{}
				}
				s.Tools[name] = price
			}
		}
	}
	if err != nil {
		return fmt.Errorf("billing settings: invalid %s: %w", key, err)
	}
	return nil
}

// loadSettings reads every settings row and applies it over the defaults.
func loadSettings(ctx context.Context, q settingsQuerier) (Settings, error) {
	out := DefaultSettings()
	rows, err := q.Query(ctx, `select key, value from billing.settings`)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return Settings{}, err
		}
		if err := out.apply(key, raw); err != nil {
			return Settings{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return Settings{}, err
	}
	return out, nil
}

// Settings returns the effective settings (stored overrides on top of the
// contract defaults).
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	return loadSettings(ctx, s.Pool)
}

// SettingsVersion returns just the effective settings version.
func (s *Store) SettingsVersion(ctx context.Context) (string, error) {
	settings, err := s.Settings(ctx)
	if err != nil {
		return "", err
	}
	return settings.Version, nil
}

// settingsFromSnapshot decodes one immutable settings snapshot. found is false
// when the version has no snapshot row.
func settingsFromSnapshot(ctx context.Context, q settingsQuerier, version string) (Settings, bool, error) {
	if strings.TrimSpace(version) == "" {
		return DefaultSettings(), false, nil
	}
	var raw []byte
	err := q.QueryRow(ctx, `select snapshot from billing.settings_versions where version=$1`, version).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultSettings(), false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	out := DefaultSettings()
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return Settings{}, false, fmt.Errorf("billing settings: invalid snapshot %q: %w", version, err)
	}
	for key, value := range values {
		if err := out.apply(key, value); err != nil {
			return Settings{}, false, err
		}
	}
	if out.Version == "" {
		out.Version = version
	}
	return out, true, nil
}

// SettingsForVersion loads the snapshot locked by a lease. A missing snapshot
// falls back to the live settings row and increments the fallback counter so an
// operator can alert on it; settlements must never silently use a different
// revenue share.
func (s *Store) SettingsForVersion(ctx context.Context, version string) (Settings, error) {
	out, found, err := settingsFromSnapshot(ctx, s.Pool, version)
	if err != nil {
		return Settings{}, err
	}
	if found {
		return out, nil
	}
	settingsSnapshotMisses.Add(1)
	return s.Settings(ctx)
}

// UpdateSettings atomically upserts settings keys and snapshots the version
// they belong to. A settings change must publish a new settings.version:
// existing snapshots are immutable so leases and ledger rows that reference an
// old version keep reproducing the old charge.
func (s *Store) UpdateSettings(ctx context.Context, updates map[string]any) (Settings, error) {
	version, ok := updates[SettingVersion].(string)
	version = strings.TrimSpace(version)
	if !ok || version == "" {
		return Settings{}, fmt.Errorf("%w: updates must include %s", ErrInvalidSettings, SettingVersion)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Settings{}, err
	}
	defer tx.Rollback(ctx)
	for key, value := range updates {
		raw, err := json.Marshal(value)
		if err != nil {
			return Settings{}, fmt.Errorf("%w: %s: %v", ErrInvalidSettings, key, err)
		}
		// Validate known keys before writing so a typo cannot change money.
		probe := DefaultSettings()
		if err := probe.apply(key, raw); err != nil {
			return Settings{}, err
		}
		if _, err := tx.Exec(ctx, `
			insert into billing.settings (key, value, updated_at)
			values ($1, $2, now())
			on conflict (key) do update set value=excluded.value, updated_at=now()`,
			key, string(raw)); err != nil {
			return Settings{}, err
		}
	}
	tag, err := tx.Exec(ctx, `
		insert into billing.settings_versions (version, snapshot)
		select $1, coalesce(jsonb_object_agg(key, value), '{}'::jsonb) from billing.settings
		on conflict (version) do nothing`, version)
	if err != nil {
		return Settings{}, err
	}
	if tag.RowsAffected() == 0 {
		return Settings{}, fmt.Errorf("%w: %s", ErrSettingsVersionExists, version)
	}
	if err := tx.Commit(ctx); err != nil {
		return Settings{}, err
	}
	return s.Settings(ctx)
}

// loadSettlementSettings resolves the settings snapshot a lease locked. An
// empty version (lease created before V2) uses the live settings without
// counting a miss; a version that has no snapshot falls back, counts the miss
// for alerts, and keeps the lease's version on the booking for forensics.
func loadSettlementSettings(ctx context.Context, q settingsQuerier, version string) (Settings, string, error) {
	if strings.TrimSpace(version) == "" {
		settings, err := loadSettings(ctx, q)
		if err != nil {
			return Settings{}, "", err
		}
		return settings, settings.Version, nil
	}
	out, found, err := settingsFromSnapshot(ctx, q, version)
	if err != nil {
		return Settings{}, "", err
	}
	if found {
		return out, version, nil
	}
	settingsSnapshotMisses.Add(1)
	settings, err := loadSettings(ctx, q)
	if err != nil {
		return Settings{}, "", err
	}
	return settings, version, nil
}

// SettingsFlat returns the effective settings as a flat a.b key map plus the
// special `settings_version` key, which is the shape product services consume
// (GET /internal/v1/billing/settings). Stored rows win over the contract
// defaults; values keep their JSON scalar type (numbers stay numbers).
func (s *Store) SettingsFlat(ctx context.Context) (map[string]any, error) {
	defaults := DefaultSettings()
	out := map[string]any{
		"settings_version":                  defaults.Version,
		SettingFXUSDToCredits:               defaults.FXUSDToCredits,
		SettingCreditYuan:                   defaults.CreditYuan,
		SettingRounding:                     defaults.Rounding,
		SettingMinChargeCredits:             defaults.MinChargeCredits,
		SettingCompressionUserShareBPS:      defaults.CompressionUserShareBPS,
		SettingCompressionMinNetGainCredits: defaults.CompressionMinNetGainCredits,
		SettingSandboxMarkupBPS:             defaults.SandboxMarkupBPS,
		SettingSandboxDiskFreeGiB:           defaults.SandboxDiskFreeGiB,
		SettingMemoryCallPriceCredits:       defaults.MemoryCallPriceCredits,
	}
	for tool, price := range defaults.Tools {
		out[SettingToolsPrefix+tool+SettingToolsSuffix] = price
	}
	rows, err := s.Pool.Query(ctx, `select key, value from billing.settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	version := defaults.Version
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		if key == SettingVersion {
			decoded, err := jsonText(raw)
			if err != nil {
				return nil, fmt.Errorf("billing settings: invalid %s: %w", key, err)
			}
			if decoded != "" {
				version = decoded
			}
			continue
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("billing settings: invalid %s: %w", key, err)
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out["settings_version"] = version
	return out, nil
}
