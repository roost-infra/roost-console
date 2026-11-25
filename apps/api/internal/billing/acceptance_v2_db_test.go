package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestBillingV2Acceptance is the repeatable billing V2 acceptance suite. It
// needs a disposable Postgres with the migrations applied:
//
//	ROOST_TEST_DATABASE_URL=postgres://... go test ./internal/billing \
//	    -run TestBillingV2Acceptance -count=1 -v
//
// It covers the P1 acceptance list end to end at the store layer (the same
// functions the relay/dslpha HTTP handlers call):
//
//  1. the same model+usage through the relay path and the dslpha path differs
//     by at most one credit;
//  2. an expired reservation still settles for its usage;
//  3. a wallet that cannot cover the charge is partially debited and the
//     remainder is booked as a debt;
//  4. a changed price version is rejected before any money moves (the HTTP
//     layer maps ErrPriceVersionMismatch to 409);
//  5. compression reports a positive net gain and charges less than the plain
//     settlement.
func TestBillingV2Acceptance(t *testing.T) {
	pool := pricingV2TestPool(t)
	seedPricingV2Settings(t, pool)
	seedPricingV2Book(t, pool)
	store := New(pool)
	ctx := context.Background()

	usage := TokenUsage{Input: 150_000, Output: 2_000, CacheRead: 1_000, CacheWrite: 500}

	// 1. Parity: relay and dslpha price the same model+usage from the same
	// book and must not disagree by more than one credit (they should agree
	// exactly; the one-credit tolerance is the contract's acceptance bar).
	t.Run("relay and dslpha agree within one credit", func(t *testing.T) {
		relayOrg := newSandboxTestOrg(t, pool, 1_000)
		relayRequest := "req-" + uuid.NewString()
		reservation, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
			OrgID: relayOrg, RequestID: relayRequest, Amount: 500,
			Currency: RelayCurrency, TTL: time.Minute, PriceVersion: testPriceBookVersion,
		})
		if err != nil {
			t.Fatalf("relay reserve: %v", err)
		}
		relayOp, err := store.RelaySettleV2(ctx, RelaySettleInput{
			ReservationID:  reservation.ID,
			SettlementID:   "stl-" + uuid.NewString(),
			IdempotencyKey: "settle:" + relayRequest,
			Model:          "claude-sonnet-5.5",
			PriceVersion:   testPriceBookVersion,
			Usage:          &usage,
		})
		if err != nil {
			t.Fatalf("relay settle: %v", err)
		}

		dslphaOrg := newSandboxTestOrg(t, pool, 1_000)
		dslphaRun := "run-" + uuid.NewString()
		insertDSAlphaLease(t, pool, dslphaOrg, dslphaRun, 500, "active")
		dslphaResult, err := store.SettleByRunV2(ctx, SettleRunInput{
			OrgID: dslphaOrg, Product: "dsalpha", RunRef: dslphaRun,
			ModelUsage: []ModelUsage{{
				Model: "claude-sonnet-5.5", Input: usage.Input, Output: usage.Output,
				CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite, Calls: 1,
			}},
		})
		if err != nil {
			t.Fatalf("dslpha settle: %v", err)
		}
		if !dslphaResult.Settled {
			t.Fatal("dslpha settlement was not booked")
		}
		diff := relayOp.Amount - dslphaResult.ChargedCredits
		if diff < 0 {
			diff = -diff
		}
		if diff > 1 {
			t.Fatalf("relay=%d credits dslpha=%d credits differ by %d (>1)",
				relayOp.Amount, dslphaResult.ChargedCredits, diff)
		}
		if relayOp.AmountMinor != dslphaResult.AmountMinor {
			t.Fatalf("relay minor=%d dslpha minor=%d must match exactly",
				relayOp.AmountMinor, dslphaResult.AmountMinor)
		}
		if relayOp.Amount != 180 || relayOp.AmountMinor != 179_653 {
			t.Fatalf("contract golden changed: credits=%d minor=%d want 180/179653",
				relayOp.Amount, relayOp.AmountMinor)
		}
	})

	// A well-formed but unknown id is a 404 at the HTTP layer, not a 500.
	t.Run("unknown ids are not found", func(t *testing.T) {
		unknown := uuid.NewString()
		if _, err := store.RelayRelease(ctx, unknown); !errors.Is(err, ErrRelayNotFound) {
			t.Fatalf("relay release err=%v want ErrRelayNotFound", err)
		}
		if err := store.Release(ctx, unknown); !errors.Is(err, ErrNotFound) {
			t.Fatalf("legacy release err=%v want ErrNotFound", err)
		}
	})

	// 2. An expired reservation still settles: ExpireLeases already released
	// its hold, so the settlement must charge the spend without touching the
	// reserved counter again.
	t.Run("expired reservation still settles", func(t *testing.T) {
		orgID := newSandboxTestOrg(t, pool, 200)
		requestID := "req-" + uuid.NewString()
		reservation, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
			OrgID: orgID, RequestID: requestID, Amount: 50,
			Currency: RelayCurrency, TTL: time.Minute, PriceVersion: testPriceBookVersion,
		})
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		// Simulate ExpireLeases: release the hold, mark the lease expired.
		if _, err := pool.Exec(ctx, `
			update billing.wallets set reserved_credits=reserved_credits-$2,version=version+1 where org_id=$1`,
			orgID, 50); err != nil {
			t.Fatalf("release expired hold: %v", err)
		}
		if _, err := pool.Exec(ctx, `update billing.usage_leases set status='expired' where id=$1`, reservation.ID); err != nil {
			t.Fatalf("expire lease: %v", err)
		}
		op, err := store.RelaySettleV2(ctx, RelaySettleInput{
			ReservationID:  reservation.ID,
			SettlementID:   "stl-" + uuid.NewString(),
			IdempotencyKey: "settle:" + requestID,
			Model:          "claude-sonnet-5.5",
			PriceVersion:   testPriceBookVersion,
			Usage:          &TokenUsage{Input: 1_000, Output: 10},
		})
		if err != nil {
			t.Fatalf("settle expired: %v", err)
		}
		if op.Amount != 2 || op.AmountMinor != 1_179 {
			t.Fatalf("expired op=%+v want 2 credits / 1179 minor", op)
		}
		balance, reserved := walletState(t, pool, orgID)
		if balance != 198 || reserved != 0 {
			t.Fatalf("wallet balance=%d reserved=%d want 198/0", balance, reserved)
		}
	})

	// 3. Insufficient balance: the affordable part is debited, the remainder
	// is an open debt, and the ledger never goes negative.
	t.Run("insufficient balance becomes partial debit plus debt", func(t *testing.T) {
		orgID := newSandboxTestOrg(t, pool, 150)
		requestID := "req-" + uuid.NewString()
		reservation, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
			OrgID: orgID, RequestID: requestID, Amount: 150,
			Currency: RelayCurrency, TTL: time.Minute, PriceVersion: testPriceBookVersion,
		})
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		op, err := store.RelaySettleV2(ctx, RelaySettleInput{
			ReservationID:  reservation.ID,
			SettlementID:   "stl-" + uuid.NewString(),
			IdempotencyKey: "settle:" + requestID,
			Model:          "claude-sonnet-5.5",
			PriceVersion:   testPriceBookVersion,
			Usage:          &usage,
		})
		if err != nil {
			t.Fatalf("settle: %v", err)
		}
		if op.Amount != 180 || len(op.Debts) != 1 || op.Debts[0].AmountCredits != 30 {
			t.Fatalf("op=%+v want due=180 debt=30", op)
		}
		var ledgerDelta int64
		if err := pool.QueryRow(ctx, `
			select delta_credits from billing.ledger where org_id=$1 and reason='relay_usage'`, orgID).
			Scan(&ledgerDelta); err != nil {
			t.Fatalf("read ledger: %v", err)
		}
		if ledgerDelta != -150 {
			t.Fatalf("ledger delta=%d want -150 (no negative balance)", ledgerDelta)
		}
		balance, reserved := walletState(t, pool, orgID)
		if balance != 0 || reserved != 0 {
			t.Fatalf("wallet balance=%d reserved=%d want 0/0", balance, reserved)
		}
	})

	// 4. A price change between reserve and settle is a 409 at the HTTP layer
	// (relayStoreError) and must not move money here.
	t.Run("price version change is rejected before charging", func(t *testing.T) {
		orgID := newSandboxTestOrg(t, pool, 1_000)
		requestID := "req-" + uuid.NewString()
		reservation, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
			OrgID: orgID, RequestID: requestID, Amount: 200,
			Currency: RelayCurrency, TTL: time.Minute, PriceVersion: testPriceBookVersion,
		})
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		base := RelaySettleInput{
			ReservationID:  reservation.ID,
			SettlementID:   "stl-" + uuid.NewString(),
			IdempotencyKey: "settle:" + requestID,
			Model:          "claude-sonnet-5.5",
			PriceVersion:   "changed-price-version",
			Usage:          &usage,
		}
		if _, err := store.RelaySettleV2(ctx, base); !errors.Is(err, ErrPriceVersionMismatch) {
			t.Fatalf("err=%v want ErrPriceVersionMismatch (HTTP 409)", err)
		}
		balance, reserved := walletState(t, pool, orgID)
		if balance != 1_000 || reserved != 200 {
			t.Fatalf("rejected settle moved money: balance=%d reserved=%d", balance, reserved)
		}
		var ledgerRows int64
		if err := pool.QueryRow(ctx, `select count(*) from billing.ledger where org_id=$1`, orgID).Scan(&ledgerRows); err != nil {
			t.Fatalf("count ledger: %v", err)
		}
		if ledgerRows != 0 {
			t.Fatalf("rejected settle booked %d ledger rows", ledgerRows)
		}
		// The same request with the reserved version still settles.
		ok := base
		ok.PriceVersion = testPriceBookVersion
		if _, err := store.RelaySettleV2(ctx, ok); err != nil {
			t.Fatalf("settle with reserved version: %v", err)
		}
	})

	// 5. Compression: a positive user-facing net gain and a lower charge than
	// the identical uncompressed request.
	t.Run("compression reports net gain and lowers the charge", func(t *testing.T) {
		plainOrg := newSandboxTestOrg(t, pool, 1_000)
		plainRequest := "req-" + uuid.NewString()
		plainReservation, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
			OrgID: plainOrg, RequestID: plainRequest, Amount: 500,
			Currency: RelayCurrency, TTL: time.Minute, PriceVersion: testPriceBookVersion,
		})
		if err != nil {
			t.Fatalf("plain reserve: %v", err)
		}
		plain, err := store.RelaySettleV2(ctx, RelaySettleInput{
			ReservationID: plainReservation.ID, SettlementID: "stl-" + uuid.NewString(),
			IdempotencyKey: "settle:" + plainRequest, Model: "claude-sonnet-5.5",
			PriceVersion: testPriceBookVersion, Usage: &usage,
		})
		if err != nil {
			t.Fatalf("plain settle: %v", err)
		}

		compressedOrg := newSandboxTestOrg(t, pool, 1_000)
		compressedRequest := "req-" + uuid.NewString()
		compressedReservation, err := store.RelayReserveWithDetails(ctx, RelayReserveInput{
			OrgID: compressedOrg, RequestID: compressedRequest, Amount: 500,
			Currency: RelayCurrency, TTL: time.Minute, PriceVersion: testPriceBookVersion,
		})
		if err != nil {
			t.Fatalf("compressed reserve: %v", err)
		}
		compressed, err := store.RelaySettleV2(ctx, RelaySettleInput{
			ReservationID: compressedReservation.ID, SettlementID: "stl-" + uuid.NewString(),
			IdempotencyKey: "settle:" + compressedRequest, Model: "claude-sonnet-5.5",
			PriceVersion: testPriceBookVersion, Usage: &usage,
			Basis: &CompressionBasis{
				Mode: "original", BeforeInputTokens: 150_000, AfterInputTokens: 90_000,
				SummaryCostMinor: 20,
			},
		})
		if err != nil {
			t.Fatalf("compressed settle: %v", err)
		}
		if compressed.Pricing == nil || compressed.Pricing.Compression == nil {
			t.Fatalf("compression report missing: %+v", compressed.Pricing)
		}
		report := compressed.Pricing.Compression
		if report.NetGainMinor <= 0 {
			t.Fatalf("net gain=%d want > 0", report.NetGainMinor)
		}
		if compressed.AmountMinor >= plain.AmountMinor {
			t.Fatalf("compressed minor=%d not below plain minor=%d", compressed.AmountMinor, plain.AmountMinor)
		}
		if compressed.Amount >= plain.Amount {
			t.Fatalf("compressed credits=%d not below plain credits=%d", compressed.Amount, plain.Amount)
		}
		// The persisted breakdown must agree with the response.
		var persistedNetGain int64
		if err := pool.QueryRow(ctx, `
			select net_gain_minor from billing.usage_compression where lease_id=$1`,
			compressedReservation.ID).Scan(&persistedNetGain); err != nil {
			t.Fatalf("read persisted compression: %v", err)
		}
		if persistedNetGain != report.NetGainMinor {
			t.Fatalf("persisted net gain=%d report=%d", persistedNetGain, report.NetGainMinor)
		}
	})
}
