package billing

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestPricingV2Migration is a static guard for 0015_pricing_v2.sql. CI has no
// database to inspect, so this keeps the tables, columns, checks and indexes
// the pricing engine depends on from drifting out of the migration.
func TestPricingV2Migration(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "migrations", "0015_pricing_v2.sql")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)

	for _, table := range []string{
		"create table if not exists billing.settings",
		"create table if not exists billing.price_book",
		"create table if not exists billing.price_book_peak",
		"create table if not exists billing.debts",
		"create table if not exists billing.settings_versions",
	} {
		if !strings.Contains(sql, table) {
			t.Errorf("migration is missing %q", table)
		}
	}

	for _, setting := range []string{
		"'fx.usd_to_credits','720'",
		`'billing.credit_yuan','0.01'`,
		`'billing.rounding','"ceil"'`,
		"'billing.min_charge_credits','1'",
		"'compression.user_share_bps','2500'",
		"'compression.min_net_gain_credits','1'",
		"'sandbox.markup_bps','13000'",
		"'sandbox.disk_free_gib','5'",
		"'tools.search.price_minor','7'",
		"'tools.fetch.price_minor','7'",
		"'memory.call.price_credits','1'",
		`'settings.version','"2026-11-25.1"'`,
	} {
		if !strings.Contains(sql, setting) {
			t.Errorf("migration is missing seed %s", setting)
		}
	}

	priceBook := migrationTableBlock(t, sql, "create table if not exists billing.price_book")
	for _, column := range []string{
		"id", "version", "model", "currency", "component", "tier_max_input",
		"retail_minor_per_million", "cost_minor_per_million", "cost_currency", "effective_from",
	} {
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(column) + `\s+`).MatchString(priceBook) {
			t.Errorf("price_book is missing column %q", column)
		}
	}
	for _, check := range []string{
		"component in ('input','output','cache_read','cache_write')",
		"retail_minor_per_million >= 0",
		"cost_minor_per_million >= 0",
		"unique (version, model, component, tier_max_input)",
	} {
		if !strings.Contains(priceBook, check) {
			t.Errorf("price_book is missing %q", check)
		}
	}

	peak := migrationTableBlock(t, sql, "create table if not exists billing.price_book_peak")
	for _, column := range []string{"version", "model", "weekdays", "start_minute", "end_minute", "timezone", "multiplier_bps"} {
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(column) + `\s+`).MatchString(peak) {
			t.Errorf("price_book_peak is missing column %q", column)
		}
	}
	if !strings.Contains(peak, "multiplier_bps > 0") {
		t.Error("price_book_peak is missing the positive multiplier check")
	}

	debts := migrationTableBlock(t, sql, "create table if not exists billing.debts")
	for _, column := range []string{"id", "org_id", "product", "run_ref", "amount_credits", "reason", "status", "created_at", "resolved_at"} {
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(column) + `\s+`).MatchString(debts) {
			t.Errorf("debts is missing column %q", column)
		}
	}
	if !strings.Contains(debts, "amount_credits > 0") {
		t.Error("debts is missing the positive amount check")
	}
	if !strings.Contains(sql, "create unique index if not exists debts_once on billing.debts(org_id, product, run_ref, reason)") {
		t.Error("migration is missing the debts_once unique index")
	}
	if !strings.Contains(sql, "create index if not exists price_book_lookup") {
		t.Error("migration is missing the price_book_lookup index")
	}

	for _, column := range []string{
		"alter table billing.ledger add column if not exists policy_version text",
		"alter table billing.ledger add column if not exists settings_version text",
		"alter table billing.usage_events add column if not exists policy_version text",
		"alter table billing.usage_events add column if not exists upstream_id text",
		"alter table billing.usage_leases add column if not exists price_version text",
		"alter table billing.usage_leases add column if not exists settings_version text",
		"alter table billing.usage_leases add column if not exists upstream_id text",
	} {
		if !strings.Contains(sql, column) {
			t.Errorf("migration is missing %q", column)
		}
	}

	for _, column := range []string{
		"charge_input_tokens", "upstream_input_tokens", "user_share_tokens", "user_share_bps",
		"retail_minor_per_million", "cost_minor_per_million", "summary_cost_minor",
		"cache_loss_minor", "no_compression_margin_minor", "compressed_margin_minor", "net_gain_minor",
	} {
		if !regexp.MustCompile(`(?m)^\s*add column if not exists ` + regexp.QuoteMeta(column) + `\s+`).MatchString(sql) {
			t.Errorf("usage_compression alter is missing column %q", column)
		}
	}

	for _, table := range []string{"settings", "price_book", "price_book_peak", "debts", "settings_versions"} {
		if !strings.Contains(sql, "alter table billing."+table+" enable row level security") {
			t.Errorf("%s does not enable row level security", table)
		}
		if !strings.Contains(sql, "revoke all on billing."+table+" from anon, authenticated") {
			t.Errorf("%s does not revoke anon/authenticated access", table)
		}
		if !strings.Contains(sql, "grant select, insert, update, delete on billing."+table+" to roost_api") {
			t.Errorf("%s is not granted to roost_api", table)
		}
	}
}

func migrationTableBlock(t *testing.T, sql, marker string) string {
	t.Helper()
	start := strings.Index(sql, marker)
	if start < 0 {
		t.Fatalf("migration is missing %q", marker)
	}
	end := strings.Index(sql[start:], ");")
	if end < 0 {
		t.Fatalf("%q table definition is not terminated", marker)
	}
	return sql[start : start+end]
}
