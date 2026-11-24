package billing

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	testPriceBookVersion = "test-v2-book"
	testSettingsVersion  = "test-settings-v2"
)

// pricingV2TestPool connects to the optional test database and skips when the
// V2 schema is not applied yet.
func pricingV2TestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("ROOST_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ROOST_TEST_DATABASE_URL to run the pricing v2 database tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	var table string
	if err := pool.QueryRow(ctx, `select coalesce(to_regclass('billing.price_book')::text,'')`).Scan(&table); err != nil {
		pool.Close()
		t.Fatalf("check schema: %v", err)
	}
	if table == "" {
		pool.Close()
		t.Skip("billing.price_book is missing: apply migrations through 0015")
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedPricingV2Settings writes the contract defaults, publishes a settings
// snapshot under testSettingsVersion and points settings.version at it.
func seedPricingV2Settings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	keys := map[string]string{
		"fx.usd_to_credits":                "720",
		"billing.credit_yuan":              "0.01",
		"billing.rounding":                 `"ceil"`,
		"billing.min_charge_credits":       "1",
		"compression.user_share_bps":       "2500",
		"compression.min_net_gain_credits": "1",
		"sandbox.markup_bps":               "13000",
		"sandbox.disk_free_gib":            "5",
		"tools.search.price_minor":         "7",
		"tools.fetch.price_minor":          "7",
		"memory.call.price_credits":        "1",
	}
	for key, value := range keys {
		if _, err := pool.Exec(ctx, `
			insert into billing.settings(key,value) values ($1,$2::jsonb)
			on conflict (key) do update set value=excluded.value, updated_at=now()`, key, value); err != nil {
			t.Fatalf("seed setting %s: %v", key, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		insert into billing.settings(key,value) values ('settings.version', to_jsonb($1::text))
		on conflict (key) do update set value=excluded.value, updated_at=now()`, testSettingsVersion); err != nil {
		t.Fatalf("seed settings.version: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into billing.settings_versions(version,snapshot)
		select $1, coalesce(jsonb_object_agg(key,value), '{}'::jsonb) from billing.settings
		on conflict (version) do nothing`, testSettingsVersion); err != nil {
		t.Fatalf("publish settings snapshot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `delete from billing.settings_versions where version=$1`, testSettingsVersion)
	})
}

// seedPricingV2Book inserts one immutable book version with the contract's
// golden Sonnet 5.5 prices and no peak window (tests are time independent).
func seedPricingV2Book(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	effective := time.Now().Add(-time.Hour).UTC()
	rows := []struct {
		component string
		retail    int64
		cost      int64
	}{
		{ComponentInput, 1_122_500, 544_000},
		{ComponentOutput, 5_610_000, 2_720_000},
		{ComponentCacheRead, 57_500, 27_200},
		{ComponentCacheWrite, 0, 680_000},
	}
	for _, row := range rows {
		if _, err := pool.Exec(ctx, `
			insert into billing.price_book
				(version,model,currency,component,tier_max_input,retail_minor_per_million,
				 cost_minor_per_million,cost_currency,effective_from)
			values ($1,'claude-sonnet-5.5','CREDIT',$2,0,$3,$4,'CNY',$5)
			on conflict (version,model,component,tier_max_input) do nothing`,
			testPriceBookVersion, row.component, row.retail, row.cost, effective); err != nil {
			t.Fatalf("seed price book: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `delete from billing.price_book where version=$1`, testPriceBookVersion)
	})
}

func insertDSAlphaLease(t *testing.T, pool *pgxpool.Pool, orgID, runRef string, reserved int64, status string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx, `
		insert into billing.usage_leases
			(org_id,product,run_ref,reserved_credits,status,expires_at,price_version,settings_version)
		values ($1,'dsalpha',$2,$3,$4,now()+interval '1 hour',$5,$6)
		returning id`, orgID, runRef, reserved, status, testPriceBookVersion, testSettingsVersion).Scan(&id); err != nil {
		t.Fatalf("insert dslpha lease: %v", err)
	}
	if status == "active" {
		if _, err := pool.Exec(ctx, `
			update billing.wallets set reserved_credits=reserved_credits+$2,version=version+1 where org_id=$1`,
			orgID, reserved); err != nil {
			t.Fatalf("reserve for dslpha lease: %v", err)
		}
	}
	return id
}

func walletState(t *testing.T, pool *pgxpool.Pool, orgID string) (balance, reserved int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		select balance_credits,reserved_credits from billing.wallets where org_id=$1`, orgID).
		Scan(&balance, &reserved); err != nil {
		t.Fatalf("read wallet: %v", err)
	}
	return balance, reserved
}

func TestRelaySettleV2DBPartialDebtAndExpired(t *testing.T) {
	pool := pricingV2TestPool(t)
	seedPricingV2Settings(t, pool)
	seedPricingV2Book(t, pool)
	store := New(pool)
	ctx := context.Background()

	// The wallet is fully held by the reservation: 180 credits due, only 150
	// can be debited, the remaining 30 become a debt.
	orgID := newSandboxTestOrg(t, pool, 150)
	requestID := "req-" + uuid.NewString()
	reservation, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
		OrgID: orgID, RequestID: requestID, Amount: 150, Currency: RelayCurrency,
		TTL: time.Minute, PriceVersion: testPriceBookVersion,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if reservation.PriceVersion != testPriceBookVersion {
		t.Fatalf("reservation price version=%q", reservation.PriceVersion)
	}
	expected := int64(179_595)
	op, err := store.RelaySettleV2(ctx, RelaySettleInput{
		ReservationID:  reservation.ID,
		SettlementID:   "stl-" + uuid.NewString(),
		IdempotencyKey: "settle:" + requestID,
		Model:          "claude-sonnet-5.5",
		UpstreamID:     "fyt",
		PriceVersion:   testPriceBookVersion,
		Usage:          &TokenUsage{Input: 150_000, Output: 2_000},
		AmountMinor:    &expected,
	})
	if err != nil {
		t.Fatalf("settle v2: %v", err)
	}
	if !op.Found || op.Amount != 180 || op.AmountMinor != 179_595 {
		t.Fatalf("op=%+v", op)
	}
	if len(op.Debts) != 1 || op.Debts[0].AmountCredits != 30 || op.Debts[0].Reason != DebtReasonInsufficientBalance {
		t.Fatalf("debts=%+v", op.Debts)
	}
	balance, reserved := walletState(t, pool, orgID)
	if balance != 0 || reserved != 0 {
		t.Fatalf("wallet balance=%d reserved=%d", balance, reserved)
	}
	var ledgerDelta int64
	if err := pool.QueryRow(ctx, `
		select delta_credits from billing.ledger where org_id=$1 and reason='relay_usage'`, orgID).Scan(&ledgerDelta); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ledgerDelta != -150 {
		t.Fatalf("ledger delta=%d want -150", ledgerDelta)
	}
	var settled int64
	var status string
	if err := pool.QueryRow(ctx, `
		select status,coalesce(settled_credits,0) from billing.usage_leases where id=$1`, reservation.ID).
		Scan(&status, &settled); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if status != "settled" || settled != 180 {
		t.Fatalf("lease status=%q settled=%d", status, settled)
	}
	var upstreamID, policyVersion string
	if err := pool.QueryRow(ctx, `
		select coalesce(upstream_id,''),coalesce(policy_version,'') from billing.usage_events where lease_id=$1`,
		reservation.ID).Scan(&upstreamID, &policyVersion); err != nil {
		t.Fatalf("read usage event: %v", err)
	}
	if upstreamID != "fyt" || policyVersion != testPriceBookVersion {
		t.Fatalf("usage provenance upstream=%q policy=%q", upstreamID, policyVersion)
	}

	// An expired lease still settles: ExpireLeases already released the hold,
	// so only the spend is charged.
	expiredOrg := newSandboxTestOrg(t, pool, 500)
	expiredRequest := "req-" + uuid.NewString()
	expiredReservation, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
		OrgID: expiredOrg, RequestID: expiredRequest, Amount: 50, Currency: RelayCurrency,
		TTL: time.Minute, PriceVersion: testPriceBookVersion,
	})
	if err != nil {
		t.Fatalf("reserve expired: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		update billing.wallets set reserved_credits=reserved_credits-$2,version=version+1 where org_id=$1`,
		expiredOrg, 50); err != nil {
		t.Fatalf("release expired hold: %v", err)
	}
	if _, err := pool.Exec(ctx, `update billing.usage_leases set status='expired' where id=$1`, expiredReservation.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	expiredOp, err := store.RelaySettleV2(ctx, RelaySettleInput{
		ReservationID:  expiredReservation.ID,
		SettlementID:   "stl-" + uuid.NewString(),
		IdempotencyKey: "settle:" + expiredRequest,
		Model:          "claude-sonnet-5.5",
		PriceVersion:   testPriceBookVersion,
		Usage:          &TokenUsage{Input: 1_000, Output: 10},
	})
	if err != nil {
		t.Fatalf("settle expired: %v", err)
	}
	// 1000×1.1225 + 10×5.61 = 1178.6 -> 1179 minor -> 2 credits.
	if expiredOp.Amount != 2 || expiredOp.AmountMinor != 1_179 {
		t.Fatalf("expired op=%+v", expiredOp)
	}
	balance, reserved = walletState(t, pool, expiredOrg)
	if balance != 498 || reserved != 0 {
		t.Fatalf("expired wallet balance=%d reserved=%d", balance, reserved)
	}
}

func TestRelaySettleV2DBVersionAndAmountMismatch(t *testing.T) {
	pool := pricingV2TestPool(t)
	seedPricingV2Settings(t, pool)
	seedPricingV2Book(t, pool)
	store := New(pool)
	ctx := context.Background()

	orgID := newSandboxTestOrg(t, pool, 1_000)
	requestID := "req-" + uuid.NewString()
	reservation, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
		OrgID: orgID, RequestID: requestID, Amount: 200, Currency: RelayCurrency,
		TTL: time.Minute, PriceVersion: testPriceBookVersion,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	usage := &TokenUsage{Input: 150_000, Output: 2_000}
	base := RelaySettleInput{
		ReservationID:  reservation.ID,
		SettlementID:   "stl-" + uuid.NewString(),
		IdempotencyKey: "settle:" + requestID,
		Model:          "claude-sonnet-5.5",
		PriceVersion:   testPriceBookVersion,
		Usage:          usage,
	}
	mismatched := base
	mismatched.PriceVersion = "some-other-version"
	if _, err := store.RelaySettleV2(ctx, mismatched); !errors.Is(err, ErrPriceVersionMismatch) {
		t.Fatalf("price version err=%v want ErrPriceVersionMismatch", err)
	}
	wrongAmount := base
	wrong := int64(1)
	wrongAmount.AmountMinor = &wrong
	if _, err := store.RelaySettleV2(ctx, wrongAmount); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("amount err=%v want ErrAmountMismatch", err)
	}
	// Neither rejected attempt may debit or book anything.
	balance, reserved := walletState(t, pool, orgID)
	if balance != 1_000 || reserved != 200 {
		t.Fatalf("rejected settle changed wallet: balance=%d reserved=%d", balance, reserved)
	}
	var ledgerRows int64
	if err := pool.QueryRow(ctx, `select count(*) from billing.ledger where org_id=$1`, orgID).Scan(&ledgerRows); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ledgerRows != 0 {
		t.Fatalf("rejected settle booked %d ledger rows", ledgerRows)
	}
	var status string
	if err := pool.QueryRow(ctx, `select status from billing.usage_leases where id=$1`, reservation.ID).Scan(&status); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if status != "active" {
		t.Fatalf("rejected settle changed lease status to %q", status)
	}

	// The correct settlement books once; a second settlement with a new key is
	// a conflict, and a released lease cannot settle at all.
	if _, err := store.RelaySettleV2(ctx, base); err != nil {
		t.Fatalf("settle: %v", err)
	}
	again := base
	again.IdempotencyKey = "settle:" + requestID + ":again"
	again.SettlementID = "stl-" + uuid.NewString()
	if _, err := store.RelaySettleV2(ctx, again); !errors.Is(err, ErrRelayConflict) {
		t.Fatalf("second settle err=%v want ErrRelayConflict", err)
	}
	// Replaying the original key returns the original booking.
	replay, err := store.RelaySettleV2(ctx, base)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Found || replay.Amount != 180 || replay.AmountMinor != 180_000 {
		t.Fatalf("replay op=%+v", replay)
	}

	releasedRequest := "req-" + uuid.NewString()
	released, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
		OrgID: orgID, RequestID: releasedRequest, Amount: 10, Currency: RelayCurrency,
		TTL: time.Minute, PriceVersion: testPriceBookVersion,
	})
	if err != nil {
		t.Fatalf("reserve released: %v", err)
	}
	if _, err := store.RelayRelease(ctx, released.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := store.RelaySettleV2(ctx, RelaySettleInput{
		ReservationID: released.ID, SettlementID: "stl-" + uuid.NewString(),
		IdempotencyKey: "settle:" + releasedRequest, Model: "claude-sonnet-5.5",
		PriceVersion: testPriceBookVersion, Usage: &TokenUsage{Input: 1},
	}); !errors.Is(err, ErrRelayReleased) {
		t.Fatalf("released settle err=%v want ErrRelayReleased", err)
	}
}

func TestSettleByRunV2DBModelUsage(t *testing.T) {
	pool := pricingV2TestPool(t)
	seedPricingV2Settings(t, pool)
	seedPricingV2Book(t, pool)
	store := New(pool)
	ctx := context.Background()

	orgID := newSandboxTestOrg(t, pool, 1_000)
	runRef := "run-" + uuid.NewString()
	leaseID := insertDSAlphaLease(t, pool, orgID, runRef, 200, "active")
	result, err := store.SettleByRunV2(ctx, SettleRunInput{
		OrgID: orgID, Product: "dsalpha", RunRef: runRef,
		ModelUsage: []ModelUsage{{Model: "claude-sonnet-5.5", Input: 150_000, Output: 2_000, Calls: 3}},
	})
	if err != nil {
		t.Fatalf("settle by run: %v", err)
	}
	if !result.Settled || result.ChargedCredits != 180 || result.AmountMinor != 179_595 ||
		result.PriceVersion != testPriceBookVersion || result.Fallback {
		t.Fatalf("result=%+v", result)
	}
	balance, reserved := walletState(t, pool, orgID)
	if balance != 820 || reserved != 0 {
		t.Fatalf("wallet balance=%d reserved=%d", balance, reserved)
	}
	var ledgerRows int64
	if err := pool.QueryRow(ctx, `
		select count(*) from billing.ledger where org_id=$1 and reason='usage' and delta_credits=-180`, orgID).
		Scan(&ledgerRows); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ledgerRows != 1 {
		t.Fatalf("usage ledger rows=%d", ledgerRows)
	}
	var usageRows int64
	if err := pool.QueryRow(ctx, `
		select count(*) from billing.usage_events where lease_id=$1 and model='claude-sonnet-5.5'`, leaseID).
		Scan(&usageRows); err != nil {
		t.Fatalf("count usage events: %v", err)
	}
	if usageRows != 1 {
		t.Fatalf("usage event rows=%d", usageRows)
	}

	// An unknown model without a USD fallback is a 422-mapped error.
	unknownOrg := newSandboxTestOrg(t, pool, 1_000)
	unknownRun := "run-" + uuid.NewString()
	insertDSAlphaLease(t, pool, unknownOrg, unknownRun, 100, "active")
	_, err = store.SettleByRunV2(ctx, SettleRunInput{
		OrgID: unknownOrg, Product: "dsalpha", RunRef: unknownRun,
		ModelUsage: []ModelUsage{{Model: "not-in-book", Input: 10}},
	})
	if !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown model err=%v want ErrUnknownModel", err)
	}

	// Same unknown model with spent_usd falls back to fx and marks the result.
	spent := 0.1
	fallbackResult, err := store.SettleByRunV2(ctx, SettleRunInput{
		OrgID: unknownOrg, Product: "dsalpha", RunRef: unknownRun,
		ModelUsage: []ModelUsage{{Model: "not-in-book", Input: 10}},
		SpentUSD:   &spent,
	})
	if err != nil {
		t.Fatalf("fallback settle: %v", err)
	}
	if !fallbackResult.Settled || !fallbackResult.Fallback || fallbackResult.ChargedCredits != 72 {
		t.Fatalf("fallback result=%+v", fallbackResult)
	}
}

func TestSettleByRunV2DBByokExpiredPartial(t *testing.T) {
	pool := pricingV2TestPool(t)
	seedPricingV2Settings(t, pool)
	seedPricingV2Book(t, pool)
	store := New(pool)
	ctx := context.Background()

	// BYOK records usage without charging.
	byokOrg := newSandboxTestOrg(t, pool, 1_000)
	byokRun := "run-" + uuid.NewString()
	byokLease := insertDSAlphaLease(t, pool, byokOrg, byokRun, 100, "active")
	byok, err := store.SettleByRunV2(ctx, SettleRunInput{
		OrgID: byokOrg, Product: "dsalpha", RunRef: byokRun, Byok: true,
		ModelUsage: []ModelUsage{{Model: "claude-sonnet-5.5", Input: 150_000, Output: 2_000, Calls: 2}},
	})
	if err != nil {
		t.Fatalf("byok settle: %v", err)
	}
	if !byok.Settled || byok.ChargedCredits != 0 {
		t.Fatalf("byok result=%+v", byok)
	}
	balance, reserved := walletState(t, pool, byokOrg)
	if balance != 1_000 || reserved != 0 {
		t.Fatalf("byok wallet balance=%d reserved=%d", balance, reserved)
	}
	var byokLedger int64
	if err := pool.QueryRow(ctx, `select count(*) from billing.ledger where org_id=$1`, byokOrg).Scan(&byokLedger); err != nil {
		t.Fatalf("count byok ledger: %v", err)
	}
	if byokLedger != 0 {
		t.Fatalf("byok booked %d ledger rows", byokLedger)
	}
	var byokUsage int64
	if err := pool.QueryRow(ctx, `select count(*) from billing.usage_events where lease_id=$1`, byokLease).Scan(&byokUsage); err != nil {
		t.Fatalf("count byok usage: %v", err)
	}
	if byokUsage != 1 {
		t.Fatalf("byok usage rows=%d want 1 (usage is recorded, not charged)", byokUsage)
	}

	// A fully held wallet turns the unaffordable part into a debt.
	poorOrg := newSandboxTestOrg(t, pool, 50)
	poorRun := "run-" + uuid.NewString()
	insertDSAlphaLease(t, pool, poorOrg, poorRun, 50, "active")
	poor, err := store.SettleByRunV2(ctx, SettleRunInput{
		OrgID: poorOrg, Product: "dsalpha", RunRef: poorRun,
		ModelUsage: []ModelUsage{{Model: "claude-sonnet-5.5", Input: 150_000, Output: 2_000}},
	})
	if err != nil {
		t.Fatalf("poor settle: %v", err)
	}
	if poor.ChargedCredits != 50 || len(poor.Debts) != 1 || poor.Debts[0].AmountCredits != 130 {
		t.Fatalf("poor result=%+v", poor)
	}
	balance, reserved = walletState(t, pool, poorOrg)
	if balance != 0 || reserved != 0 {
		t.Fatalf("poor wallet balance=%d reserved=%d", balance, reserved)
	}

	// An expired lease still settles and charges the usage.
	expiredOrg := newSandboxTestOrg(t, pool, 200)
	expiredRun := "run-" + uuid.NewString()
	insertDSAlphaLease(t, pool, expiredOrg, expiredRun, 0, "expired")
	expired, err := store.SettleByRunV2(ctx, SettleRunInput{
		OrgID: expiredOrg, Product: "dsalpha", RunRef: expiredRun,
		ModelUsage: []ModelUsage{{Model: "claude-sonnet-5.5", Input: 1_000, Output: 10}},
	})
	if err != nil {
		t.Fatalf("expired settle: %v", err)
	}
	if !expired.Settled || expired.ChargedCredits != 2 {
		t.Fatalf("expired result=%+v", expired)
	}
	balance, _ = walletState(t, pool, expiredOrg)
	if balance != 198 {
		t.Fatalf("expired wallet balance=%d want 198", balance)
	}
}
