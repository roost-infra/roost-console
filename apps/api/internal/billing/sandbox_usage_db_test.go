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

// newSandboxTestOrg creates a user, organization and funded wallet for the
// database-backed billing tests. Cleanup removes the organization (cascading to
// sandbox_usage and ledger) and the auth user.
func newSandboxTestOrg(t *testing.T, pool *pgxpool.Pool, balance int64) string {
	t.Helper()
	ctx := context.Background()
	userID := uuid.NewString()
	orgID := uuid.NewString()
	if _, err := pool.Exec(ctx, `insert into auth.users (id,email) values ($1,$2)`, userID, userID+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `delete from public.organizations where id=$1`, orgID)
		_, _ = pool.Exec(context.Background(), `delete from auth.users where id=$1`, userID)
	})
	if _, err := pool.Exec(ctx, `
		insert into public.organizations (id,name,slug,owner_user_id,plan_code)
		values ($1,'Sandbox Test',$2,$3,'free')`, orgID, "sandbox-"+userID[:8], userID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into billing.wallets (org_id,balance_credits) values ($1,$2)`, orgID, balance); err != nil {
		t.Fatalf("insert wallet: %v", err)
	}
	return orgID
}

// TestSandboxUsagePersistenceDB exercises the sandbox charge transaction, its
// idempotent replay, the insufficient-credit refusal and the user billing detail
// aggregation against a real Postgres. It is skipped unless
// ROOST_TEST_DATABASE_URL points at a database with migrations 0002..0010
// applied.
func TestSandboxUsagePersistenceDB(t *testing.T) {
	databaseURL := os.Getenv("ROOST_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ROOST_TEST_DATABASE_URL to run the sandbox usage database test")
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
	t.Cleanup(pool.Close)
	store := New(pool)

	orgID := newSandboxTestOrg(t, pool, 1000)
	started := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	ended := started.Add(time.Hour)
	idempotencyKey := "sbx-" + uuid.NewString()

	in := SandboxUsageInput{
		OrgID:             orgID,
		UserID:            "user-1",
		Product:           "market",
		SandboxID:         "sbx-1",
		Provider:          "daytona",
		ProviderSandboxID: "daytona-1",
		RunID:             "run-1",
		StartedAt:         &started,
		EndedAt:           &ended,
		BillableSeconds:   3600,
		CPUSeconds:        3600,
		MemoryMiBSeconds:  3600 * 1024,
		DiskMiBSeconds:    3600 * 256,
		GPUSeconds:        0,
		CPUCredits:        1.5,
		MemoryCredits:     1.5,
		DiskCredits:       1,
		GPUCredits:        0,
		UnitPrices:        []byte(`{"cpu_credits_per_second":0.0004}`),
		IdempotencyKey:    idempotencyKey,
	}
	result, err := store.RecordSandboxUsage(ctx, in)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	// The console prices the report from settings (Daytona rates × markup × fx):
	// 0.0666 USD × 1.3 × 720 = 62.3376 credits -> ceil 63. The product-reported
	// 4 credits are kept only as an audit snapshot.
	if result.Usage.ID == 0 || result.ChargedCredits != 63 || result.Usage.CreditsMinor != 63000 ||
		result.BalanceCredits != 937 || result.Replayed {
		t.Fatalf("record result=%+v", result)
	}

	var (
		gotTotal, gotMinor, gotBillable int64
		gotProduct, gotKey, gotRef      string
		gotCPU                          float64
	)
	if err := pool.QueryRow(ctx, `
		select total_credits,credits_minor,billable_seconds,product,idempotency_key,
		       coalesce(run_id,''),cpu_credits::float8
		from billing.sandbox_usage where id=$1`, result.Usage.ID).
		Scan(&gotTotal, &gotMinor, &gotBillable, &gotProduct, &gotKey, &gotRef, &gotCPU); err != nil {
		t.Fatalf("read sandbox row: %v", err)
	}
	if gotTotal != 63 || gotMinor != 63000 || gotBillable != 3600 ||
		gotProduct != "market" || gotKey != idempotencyKey || gotRef != "run-1" || gotCPU != 47.1744 {
		t.Fatalf("persisted row total=%d minor=%d billable=%d product=%q key=%q ref=%q cpu=%v",
			gotTotal, gotMinor, gotBillable, gotProduct, gotKey, gotRef, gotCPU)
	}

	var ledgerDelta int64
	var ledgerReason, ledgerRef, ledgerKey string
	if err := pool.QueryRow(ctx, `
		select delta_credits,reason,coalesce(ref_id,''),coalesce(idempotency_key,'')
		from billing.ledger where org_id=$1 and reason='sandbox_usage'`, orgID).
		Scan(&ledgerDelta, &ledgerReason, &ledgerRef, &ledgerKey); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if ledgerDelta != -63 || ledgerReason != "sandbox_usage" || ledgerRef != "run-1" ||
		ledgerKey != "sandbox:"+idempotencyKey {
		t.Fatalf("ledger delta=%d reason=%q ref=%q key=%q", ledgerDelta, ledgerReason, ledgerRef, ledgerKey)
	}

	// Replay: same key, a different (larger) reported total must not charge
	// again or change the stored row.
	replay := in
	larger := int64(99)
	replay.TotalCredits = &larger
	again, err := store.RecordSandboxUsage(ctx, replay)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.Replayed || again.Usage.ID != result.Usage.ID ||
		again.ChargedCredits != 63 || again.BalanceCredits != 937 {
		t.Fatalf("replay result=%+v", again)
	}
	var rows, ledgerRows int64
	if err := pool.QueryRow(ctx, `select count(*) from billing.sandbox_usage where org_id=$1`, orgID).Scan(&rows); err != nil {
		t.Fatalf("count sandbox rows: %v", err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from billing.ledger where org_id=$1 and reason='sandbox_usage'`, orgID).Scan(&ledgerRows); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	var balance int64
	if err := pool.QueryRow(ctx, `select balance_credits from billing.wallets where org_id=$1`, orgID).Scan(&balance); err != nil {
		t.Fatalf("read wallet: %v", err)
	}
	if rows != 1 || ledgerRows != 1 || balance != 937 {
		t.Fatalf("after replay rows=%d ledger=%d balance=%d", rows, ledgerRows, balance)
	}

	// A key reused by another organization is rejected instead of returning (or
	// charging) the first organization's row.
	otherOrgID := newSandboxTestOrg(t, pool, 100)
	crossOrg := in
	crossOrg.OrgID = otherOrgID
	if _, err := store.RecordSandboxUsage(ctx, crossOrg); !errors.Is(err, ErrInvalidSandboxUsage) {
		t.Fatalf("cross-org replay err=%v want ErrInvalidSandboxUsage", err)
	}
	var otherBalance int64
	if err := pool.QueryRow(ctx, `select balance_credits from billing.wallets where org_id=$1`, otherOrgID).Scan(&otherBalance); err != nil {
		t.Fatalf("cross-org wallet: %v", err)
	}
	if otherBalance != 100 {
		t.Fatalf("cross-org replay debited: balance=%d", otherBalance)
	}

	// Insufficient credits: 402 at the store boundary, and the insert rolls
	// back. The charge is the console-priced 63 credits, so a 1-credit wallet
	// cannot cover it.
	poorOrgID := newSandboxTestOrg(t, pool, 1)
	refusedKey := "sbx-refused-" + uuid.NewString()
	refused := in
	refused.OrgID = poorOrgID
	refused.IdempotencyKey = refusedKey
	if _, err := store.RecordSandboxUsage(ctx, refused); !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("refused err=%v want ErrInsufficientCredits", err)
	}
	var refusedRows int64
	if err := pool.QueryRow(ctx, `select count(*) from billing.sandbox_usage where idempotency_key=$1`, refusedKey).Scan(&refusedRows); err != nil {
		t.Fatalf("count refused row: %v", err)
	}
	if refusedRows != 0 {
		t.Fatalf("refused report left %d rows", refusedRows)
	}

	// Token usage in the same range: settle one relay request with a usage
	// event, then assert the end-user detail joins tokens and sandbox spend.
	requestID := "req-" + uuid.NewString()
	lease, err := store.RelayReserve(ctx, orgID, requestID, 10, RelayCurrency, time.Minute)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	usage := UsageEvent{Model: "deepseek-v4-pro", Requests: 1, InputTokens: 1000, OutputTokens: 250}
	if _, err := store.RelaySettleWithDetails(ctx, lease.ID, "stl-"+uuid.NewString(),
		"settle:"+requestID, 3, nil, nil, &usage); err != nil {
		t.Fatalf("settle: %v", err)
	}

	details, err := store.BillingDetails(ctx, orgID, 30)
	if err != nil {
		t.Fatalf("billing details: %v", err)
	}
	if details.Summary.TokenCredits != 3 || details.Summary.SandboxCredits != 63 ||
		details.Summary.TotalCredits != 66 || details.Summary.TotalMinor != 66000 ||
		details.Summary.SandboxCount != 1 || details.Summary.Requests != 1 ||
		details.Summary.TotalTokens != 1250 {
		t.Fatalf("summary=%+v", details.Summary)
	}
	if len(details.Tokens) != 1 || details.Tokens[0].Model != "deepseek-v4-pro" ||
		details.Tokens[0].CreditsMinor == nil || *details.Tokens[0].CreditsMinor != 3000 ||
		details.Tokens[0].TotalTokens != 1250 {
		t.Fatalf("tokens=%+v", details.Tokens)
	}
	if len(details.Sandboxes) != 1 || details.Sandboxes[0].SandboxID != "sbx-1" ||
		details.Sandboxes[0].CreditsMinor != 63000 || details.Sandboxes[0].Yuan != 0.63 ||
		details.Sandboxes[0].BillableHours != 1 {
		t.Fatalf("sandboxes=%+v", details.Sandboxes)
	}
	if string(details.Sandboxes[0].UnitPrices) == "" {
		t.Fatal("unit prices were not returned")
	}

	// The admin overview exposes sandbox spend separately from token spend so
	// platform operators can see both streams.
	overview, err := store.AdminOverview(ctx, 30)
	if err != nil {
		t.Fatalf("admin overview: %v", err)
	}
	var topSandbox int64
	for _, row := range overview.TopOrgs {
		if row.OrgID == orgID {
			topSandbox = row.SandboxCredits
		}
	}
	if topSandbox != 63 {
		t.Fatalf("admin top org sandbox credits=%d", topSandbox)
	}
	var userSandbox int64
	for _, row := range overview.Users {
		if row.OrgID == orgID {
			userSandbox = row.SandboxCredits
		}
	}
	if userSandbox != 63 {
		t.Fatalf("admin user sandbox credits=%d", userSandbox)
	}
}

// TestBillingDetailsMultiModelLeaseDB proves the per-model credits stay null
// when one lease carries several models, while the token spend total still
// reflects the lease's single ledger charge.
func TestBillingDetailsMultiModelLeaseDB(t *testing.T) {
	databaseURL := os.Getenv("ROOST_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set ROOST_TEST_DATABASE_URL to run the billing detail database test")
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
	t.Cleanup(pool.Close)
	store := New(pool)
	orgID := newSandboxTestOrg(t, pool, 100)

	requestID := "req-" + uuid.NewString()
	lease, err := store.RelayReserve(ctx, orgID, requestID, 10, RelayCurrency, time.Minute)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	usage := UsageEvent{Model: "deepseek-v4-pro", Requests: 1, InputTokens: 1000, OutputTokens: 250}
	if _, err := store.RelaySettleWithDetails(ctx, lease.ID, "stl-"+uuid.NewString(),
		"settle:"+requestID, 3, nil, nil, &usage); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// A second settlement event for the same lease with another model: the
	// ledger has one combined charge, so neither model can claim an exact split.
	if _, err := pool.Exec(ctx, `
		insert into billing.usage_events
			(org_id,request_id,settlement_id,lease_id,model,requests,input_tokens,output_tokens)
		values ($1,$2,$3,$4,'deepseek-v4-flash',1,500,50)`,
		orgID, requestID+"-2", "stl-"+uuid.NewString(), lease.ID); err != nil {
		t.Fatalf("insert second usage event: %v", err)
	}

	details, err := store.BillingDetails(ctx, orgID, 30)
	if err != nil {
		t.Fatalf("billing details: %v", err)
	}
	if details.Summary.TokenCredits != 3 {
		t.Fatalf("token credits=%d", details.Summary.TokenCredits)
	}
	if len(details.Tokens) != 2 {
		t.Fatalf("tokens=%+v", details.Tokens)
	}
	for _, row := range details.Tokens {
		if row.CreditsMinor != nil {
			t.Fatalf("multi-model row %q invented credits %d", row.Model, *row.CreditsMinor)
		}
	}
}
