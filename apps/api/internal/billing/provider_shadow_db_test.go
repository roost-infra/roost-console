package billing

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestShadowProviderQuoteDB exercises the provider/retail lookup against a
// real Postgres with migrations 0017/0018 applied. It is skipped unless
// ROOST_TEST_DATABASE_URL is set.
func TestShadowProviderQuoteDB(t *testing.T) {
	url := os.Getenv("ROOST_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ROOST_TEST_DATABASE_URL to run the provider shadow DB test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	const model = "shadow-db-model"
	if _, err := tx.Exec(ctx, `insert into billing.providers(id,kind,display_name) values ('shadow-p','transit','Shadow') on conflict do nothing`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `insert into billing.provider_plans(id,provider_id,plan_code,cost_micros,allowance_micros,basis) values ('shadow-plan','shadow-p','starter',169000000,300000000,'provider') on conflict do nothing`); err != nil {
		t.Fatal(err)
	}
	for component, micros := range map[string]int64{"input": 2_000_000, "output": 10_000_000} {
		if _, err := tx.Exec(ctx, `insert into billing.provider_prices(provider_id,version,model,component,tier_max_input,peak,usd_micros_per_million) values ('shadow-p','v-test',$1,$2,0,'',$3)`, model, component, micros); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `insert into billing.retail_prices(version,model,component,tier_max_input,peak,usd_micros_per_million) values ('v-test',$1,$2,0,'',$3)`, model, component, micros*2); err != nil {
			t.Fatal(err)
		}
	}
	q, err := New(pool).shadowProviderQuote(ctx, tx, "shadow-p", "shadow-plan", model,
		TokenUsage{Input: 10_000}, TokenUsage{Input: 5_000})
	if err != nil {
		t.Fatal(err)
	}
	if q == nil || q.RetailMinor != 27_200 || q.ProviderMinor != 6_800 || q.AllocatedMinor != 3_831 {
		t.Fatalf("shadow quote = %+v", q)
	}
}
