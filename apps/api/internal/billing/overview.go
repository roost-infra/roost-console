package billing

import (
	"context"
	"sort"
	"time"
)

// Platform overview reporting. This is the admin-only, cross-organization view
// of the same tables the per-organization dashboards read: wallets and ledger
// for spend, usage_events for tokens, orders for revenue, and the identity
// tables for organization/user counts. Nothing here writes.

const (
	AdminOverviewDaysDefault = 30
	AdminOverviewDaysMax     = 365

	AdminOverviewTopOrgsLimit    = 10
	AdminOverviewUsersLimit      = 100
	AdminOverviewModelsLimit     = 100
	AdminOverviewRecentOrdersMax = 20
)

// The console's product keys. Ledger rows carry the product on usage charges;
// the relay writes reason='relay_usage' while the other products write
// reason='usage'.
const (
	AdminProductRelay        = "relay"
	AdminProductMarket       = "market"
	AdminProductCausalMemory = "causal-memory"
	// The ledger's product column uses the app's real name, "dsalpha". The
	// admin API keeps the dslpha_credits field names for wire compatibility.
	AdminProductDslpha = "dsalpha"
)

const adminSpendReasons = `reason in ('usage','relay_usage','sandbox_usage')`

// adminSince is the UTC calendar-day start of the trailing range. $1 is the day
// count; callers keep the same placeholder in every query.
const adminSince = `(date_trunc('day', now() at time zone 'UTC') at time zone 'UTC') - make_interval(days => $1 - 1)`

// adminOrgActivityCTEs aggregates the per-organization billing, order and token
// streams once so the top-organization and accountable-user tables cannot drift
// apart. $1 is the day count in every reference.
const adminOrgActivityCTEs = `
	with spend as (
		select org_id,
		       coalesce(sum(abs(delta_credits)) filter (where product='relay'),0)::bigint as relay_credits,
		       coalesce(sum(abs(delta_credits)) filter (where product='market'),0)::bigint as market_credits,
		       coalesce(sum(abs(delta_credits)) filter (where product='causal-memory'),0)::bigint as memory_credits,
		       coalesce(sum(abs(delta_credits)) filter (where product='dsalpha'),0)::bigint as dslpha_credits,
		       coalesce(sum(abs(delta_credits)),0)::bigint as total_credits
		from billing.ledger
		where delta_credits < 0 and ` + adminSpendReasons + ` and created_at >= ` + adminSince + `
		group by org_id
	), ord as (
		select org_id, count(*)::bigint as orders,
		       coalesce(sum(amount_cents) filter (where status in ('paid','fulfilled')),0)::bigint as revenue_cents
		from billing.orders
		where created_at >= ` + adminSince + `
		group by org_id
	), tok as (
		select org_id,
		       coalesce(sum(requests),0)::bigint as requests,
		       coalesce(sum(input_tokens),0)::bigint as input_tokens,
		       coalesce(sum(output_tokens),0)::bigint as output_tokens,
		       coalesce(sum(cache_read_tokens),0)::bigint as cache_read_tokens,
		       coalesce(sum(cache_write_tokens),0)::bigint as cache_write_tokens,
		       coalesce(sum(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens),0)::bigint as total_tokens
		from billing.usage_events
		where created_at >= ` + adminSince + `
		group by org_id
	), sbx as (
		select org_id, coalesce(sum(total_credits),0)::bigint as sandbox_credits
		from billing.sandbox_usage
		where created_at >= ` + adminSince + `
		group by org_id
	)`

// AdminOverviewTokens is the platform-wide token aggregate. TrackedRequests is
// the number of usage_events requests in range: zero means the range has no
// tracked relay usage at all, which is why the cache hit rate is also zero.
type AdminOverviewTokens struct {
	TrackedRequests  int64   `json:"tracked_requests"`
	InputTokens      int64   `json:"input"`
	OutputTokens     int64   `json:"output"`
	CacheReadTokens  int64   `json:"cache_read"`
	CacheWriteTokens int64   `json:"cache_write"`
	TotalTokens      int64   `json:"total"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
}

// AdminOverviewTotals is the headline aggregate of the selected range.
type AdminOverviewTotals struct {
	Orgs            int64               `json:"orgs"`
	Users           int64               `json:"users"`
	ActiveOrgs      int64               `json:"active_orgs"`
	Orders          int64               `json:"orders"`
	PaidOrders      int64               `json:"paid_orders"`
	RevenueCents    int64               `json:"revenue_cents"`
	CreditsConsumed int64               `json:"credits_consumed"`
	Tokens          AdminOverviewTokens `json:"tokens"`

	// ProductSpendCredits is keyed by product. Every console product is present
	// even at zero so the client can render a stable breakdown.
	ProductSpendCredits map[string]int64 `json:"product_spend_credits"`
}

// AdminOverviewDaily is one UTC day that has at least one data point. The
// frontend zero-fills the calendar days the range covers.
type AdminOverviewDaily struct {
	Date         string `json:"date"`
	Relay        int64  `json:"relay"`
	Market       int64  `json:"market"`
	CausalMemory int64  `json:"causal-memory"`
	Dslpha       int64  `json:"dslpha"`
	Tokens       int64  `json:"tokens"`
	RevenueCents int64  `json:"revenue_cents"`
}

// AdminTopOrg is one organization's cross-product activity in the range. Billing
// is org-scoped, so the owner columns identify the accountable user instead of
// pretending usage is split per member.
type AdminTopOrg struct {
	OrgID            string  `json:"org_id"`
	Name             string  `json:"name"`
	OwnerUserID      string  `json:"owner_user_id"`
	OwnerEmail       string  `json:"owner_email"`
	PlanCode         string  `json:"plan_code"`
	BalanceCredits   int64   `json:"balance_credits"`
	Orders           int64   `json:"orders"`
	RevenueCents     int64   `json:"revenue_cents"`
	RelayCredits     int64   `json:"relay_credits"`
	MarketCredits    int64   `json:"market_credits"`
	MemoryCredits    int64   `json:"memory_credits"`
	DslphaCredits    int64   `json:"dslpha_credits"`
	SandboxCredits   int64   `json:"sandbox_credits"`
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	TopModel         string  `json:"top_model"`
}

// AdminModelRow is one model's platform-wide usage in the range.
type AdminModelRow struct {
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
}

// AdminUserRow is one accountable user (the organization owner) with the
// organization's billing and usage. Per-member attribution is deliberately not
// invented: an org has one usage stream, so the owner is the reporting unit.
type AdminUserRow struct {
	OrgID            string  `json:"org_id"`
	OrgName          string  `json:"org_name"`
	UserID           string  `json:"user_id"`
	Email            string  `json:"email"`
	PlanCode         string  `json:"plan_code"`
	BalanceCredits   int64   `json:"balance_credits"`
	Orders           int64   `json:"orders"`
	RevenueCents     int64   `json:"revenue_cents"`
	RelayCredits     int64   `json:"relay_credits"`
	MarketCredits    int64   `json:"market_credits"`
	MemoryCredits    int64   `json:"memory_credits"`
	DslphaCredits    int64   `json:"dslpha_credits"`
	SandboxCredits   int64   `json:"sandbox_credits"`
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	TopModel         string  `json:"top_model"`
}

// AdminRecentOrder is one order in the recent-orders table.
type AdminRecentOrder struct {
	ID          string     `json:"id"`
	OrgID       string     `json:"org_id"`
	OrgName     string     `json:"org_name"`
	SKU         string     `json:"sku"`
	AmountCents int64      `json:"amount_cents"`
	Credits     int64      `json:"credits"`
	Status      string     `json:"status"`
	Provider    string     `json:"provider"`
	CreatedAt   time.Time  `json:"created_at"`
	PaidAt      *time.Time `json:"paid_at"`
}

// AdminOverview is the platform-admin global overview payload.
type AdminOverview struct {
	Days int `json:"days"`

	// UsageTrackingStart is the first usage event ever recorded. Tokens before
	// it are genuinely unknown, so the UI labels the historical gap.
	UsageTrackingStart *time.Time `json:"usage_tracking_start"`

	Totals       AdminOverviewTotals  `json:"totals"`
	Daily        []AdminOverviewDaily `json:"daily"`
	Models       []AdminModelRow      `json:"models"`
	TopOrgs      []AdminTopOrg        `json:"top_orgs"`
	Users        []AdminUserRow       `json:"users"`
	RecentOrders []AdminRecentOrder   `json:"recent_orders"`
}

// clampAdminDays keeps the report range inside the documented window.
func clampAdminDays(days int) int {
	if days < 1 {
		return 1
	}
	if days > AdminOverviewDaysMax {
		return AdminOverviewDaysMax
	}
	return days
}

func adminEmptyProductSpend() map[string]int64 {
	return map[string]int64{
		AdminProductRelay:        0,
		AdminProductMarket:       0,
		AdminProductCausalMemory: 0,
		AdminProductDslpha:       0,
	}
}

func adminDailyBucket(buckets map[string]*AdminOverviewDaily, date string) *AdminOverviewDaily {
	if row := buckets[date]; row != nil {
		return row
	}
	row := &AdminOverviewDaily{Date: date}
	buckets[date] = row
	return row
}

// adminTopModelsByOrg resolves the model with the most tokens per organization
// in range. Postgres DISTINCT ON avoids aggregating models into JSON and keeps
// the "top model" choice deterministic (tokens desc, then name).
func (s *Store) adminTopModelsByOrg(ctx context.Context, days int) (map[string]string, error) {
	rows, err := s.Pool.Query(ctx, `
		select distinct on (org_id) org_id, model
		from (
			select org_id, coalesce(model,'') as model,
			       coalesce(sum(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens),0)::bigint as total_tokens
			from billing.usage_events
			where created_at >= `+adminSince+`
			group by org_id, coalesce(model,'')
		) m
		order by org_id, total_tokens desc, model asc`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var orgID, model string
		if err := rows.Scan(&orgID, &model); err != nil {
			return nil, err
		}
		out[orgID] = model
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// adminOwnerEmails resolves owner emails in one round trip. auth.users is
// readable by the application role in the deployed project; if that grant is
// missing the overview still renders and the email cells stay empty.
func (s *Store) adminOwnerEmails(ctx context.Context, ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return out
	}
	rows, err := s.Pool.Query(ctx, `
		select id::text, coalesce(email,'')
		from auth.users
		where id::text = any($1::text[])`, unique)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, email string
		if err := rows.Scan(&id, &email); err != nil {
			return out
		}
		out[id] = email
	}
	return out
}

// AdminOverview aggregates the global platform state for the trailing N UTC
// calendar days. The queries are intentionally separate and readable: each one
// is an independent aggregate over a single source table.
func (s *Store) AdminOverview(ctx context.Context, days int) (AdminOverview, error) {
	days = clampAdminDays(days)
	out := AdminOverview{
		Days:         days,
		Totals:       AdminOverviewTotals{ProductSpendCredits: adminEmptyProductSpend()},
		Daily:        make([]AdminOverviewDaily, 0),
		Models:       make([]AdminModelRow, 0),
		TopOrgs:      make([]AdminTopOrg, 0),
		Users:        make([]AdminUserRow, 0),
		RecentOrders: make([]AdminRecentOrder, 0),
	}

	if err := s.Pool.QueryRow(ctx, `select count(*)::bigint from public.organizations`).Scan(&out.Totals.Orgs); err != nil {
		return AdminOverview{}, err
	}
	// auth.users is the authoritative user count when the application role can
	// read it. Supabase projects that revoke that access fall back to distinct
	// organization membership, which is the closest cross-org equivalent.
	if err := s.Pool.QueryRow(ctx, `select count(*)::bigint from auth.users`).Scan(&out.Totals.Users); err != nil {
		if err := s.Pool.QueryRow(ctx, `select count(distinct user_id)::bigint from public.organization_members`).Scan(&out.Totals.Users); err != nil {
			return AdminOverview{}, err
		}
	}

	if err := s.Pool.QueryRow(ctx, `
		select count(*)::bigint from (
			select org_id from billing.ledger
			where delta_credits < 0 and `+adminSpendReasons+` and created_at >= `+adminSince+`
			union
			select org_id from billing.usage_events where created_at >= `+adminSince+`
		) a`, days).Scan(&out.Totals.ActiveOrgs); err != nil {
		return AdminOverview{}, err
	}

	if err := s.Pool.QueryRow(ctx, `
		select count(*)::bigint,
		       count(*) filter (where status in ('paid','fulfilled'))::bigint,
		       coalesce(sum(amount_cents) filter (where status in ('paid','fulfilled')),0)::bigint
		from billing.orders
		where created_at >= `+adminSince, days).
		Scan(&out.Totals.Orders, &out.Totals.PaidOrders, &out.Totals.RevenueCents); err != nil {
		return AdminOverview{}, err
	}

	if err := s.Pool.QueryRow(ctx, `
		select coalesce(sum(requests),0)::bigint,
		       coalesce(sum(input_tokens),0)::bigint,
		       coalesce(sum(output_tokens),0)::bigint,
		       coalesce(sum(cache_read_tokens),0)::bigint,
		       coalesce(sum(cache_write_tokens),0)::bigint
		from billing.usage_events
		where created_at >= `+adminSince, days).
		Scan(&out.Totals.Tokens.TrackedRequests, &out.Totals.Tokens.InputTokens,
			&out.Totals.Tokens.OutputTokens, &out.Totals.Tokens.CacheReadTokens,
			&out.Totals.Tokens.CacheWriteTokens); err != nil {
		return AdminOverview{}, err
	}
	out.Totals.Tokens.TotalTokens = out.Totals.Tokens.InputTokens + out.Totals.Tokens.OutputTokens +
		out.Totals.Tokens.CacheReadTokens + out.Totals.Tokens.CacheWriteTokens
	out.Totals.Tokens.CacheHitRate = cacheHitRate(out.Totals.Tokens.CacheReadTokens, out.Totals.Tokens.InputTokens)

	// Model breakdown is a single grouped query; the response is ordered by
	// total tokens and capped so a misconfigured model name cannot bloat it.
	modelRows, err := s.Pool.Query(ctx, `
		select coalesce(model,'') as model,
		       coalesce(sum(requests),0)::bigint as requests,
		       coalesce(sum(input_tokens),0)::bigint as input_tokens,
		       coalesce(sum(output_tokens),0)::bigint as output_tokens,
		       coalesce(sum(cache_read_tokens),0)::bigint as cache_read_tokens,
		       coalesce(sum(cache_write_tokens),0)::bigint as cache_write_tokens,
		       coalesce(sum(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens),0)::bigint as total_tokens
		from billing.usage_events
		where created_at >= `+adminSince+`
		group by 1
		order by total_tokens desc, model asc
		limit $2`, days, AdminOverviewModelsLimit)
	if err != nil {
		return AdminOverview{}, err
	}
	for modelRows.Next() {
		var row AdminModelRow
		if err := modelRows.Scan(&row.Model, &row.Requests, &row.InputTokens, &row.OutputTokens,
			&row.CacheReadTokens, &row.CacheWriteTokens, &row.TotalTokens); err != nil {
			modelRows.Close()
			return AdminOverview{}, err
		}
		row.CacheHitRate = cacheHitRate(row.CacheReadTokens, row.InputTokens)
		out.Models = append(out.Models, row)
	}
	if err := modelRows.Err(); err != nil {
		modelRows.Close()
		return AdminOverview{}, err
	}
	modelRows.Close()

	if err := s.Pool.QueryRow(ctx, `select min(created_at) from billing.usage_events`).Scan(&out.UsageTrackingStart); err != nil {
		return AdminOverview{}, err
	}

	// Spend is the absolute ledger movement of usage charges. Adjustments,
	// grants and payments are deliberately excluded: they move credits without
	// consuming a product.
	spendRows, err := s.Pool.Query(ctx, `
		select coalesce(product,''), coalesce(sum(abs(delta_credits)),0)::bigint
		from billing.ledger
		where delta_credits < 0 and `+adminSpendReasons+` and created_at >= `+adminSince+`
		group by 1`, days)
	if err != nil {
		return AdminOverview{}, err
	}
	for spendRows.Next() {
		var product string
		var credits int64
		if err := spendRows.Scan(&product, &credits); err != nil {
			spendRows.Close()
			return AdminOverview{}, err
		}
		if product == "" {
			product = "other"
		}
		out.Totals.ProductSpendCredits[product] += credits
		out.Totals.CreditsConsumed += credits
	}
	if err := spendRows.Err(); err != nil {
		spendRows.Close()
		return AdminOverview{}, err
	}
	spendRows.Close()

	buckets := map[string]*AdminOverviewDaily{}
	dailySpend, err := s.Pool.Query(ctx, `
		select to_char(created_at at time zone 'UTC','YYYY-MM-DD'),
		       coalesce(product,''), coalesce(sum(abs(delta_credits)),0)::bigint
		from billing.ledger
		where delta_credits < 0 and `+adminSpendReasons+` and created_at >= `+adminSince+`
		group by 1,2`, days)
	if err != nil {
		return AdminOverview{}, err
	}
	for dailySpend.Next() {
		var date, product string
		var credits int64
		if err := dailySpend.Scan(&date, &product, &credits); err != nil {
			dailySpend.Close()
			return AdminOverview{}, err
		}
		bucket := adminDailyBucket(buckets, date)
		switch product {
		case AdminProductRelay:
			bucket.Relay += credits
		case AdminProductMarket:
			bucket.Market += credits
		case AdminProductCausalMemory:
			bucket.CausalMemory += credits
		case AdminProductDslpha:
			bucket.Dslpha += credits
		}
	}
	if err := dailySpend.Err(); err != nil {
		dailySpend.Close()
		return AdminOverview{}, err
	}
	dailySpend.Close()

	dailyTokens, err := s.Pool.Query(ctx, `
		select to_char(created_at at time zone 'UTC','YYYY-MM-DD'),
		       coalesce(sum(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens),0)::bigint
		from billing.usage_events
		where created_at >= `+adminSince+`
		group by 1`, days)
	if err != nil {
		return AdminOverview{}, err
	}
	for dailyTokens.Next() {
		var date string
		var tokens int64
		if err := dailyTokens.Scan(&date, &tokens); err != nil {
			dailyTokens.Close()
			return AdminOverview{}, err
		}
		adminDailyBucket(buckets, date).Tokens += tokens
	}
	if err := dailyTokens.Err(); err != nil {
		dailyTokens.Close()
		return AdminOverview{}, err
	}
	dailyTokens.Close()

	dailyRevenue, err := s.Pool.Query(ctx, `
		select to_char(created_at at time zone 'UTC','YYYY-MM-DD'),
		       coalesce(sum(amount_cents),0)::bigint
		from billing.orders
		where status in ('paid','fulfilled') and created_at >= `+adminSince+`
		group by 1`, days)
	if err != nil {
		return AdminOverview{}, err
	}
	for dailyRevenue.Next() {
		var date string
		var cents int64
		if err := dailyRevenue.Scan(&date, &cents); err != nil {
			dailyRevenue.Close()
			return AdminOverview{}, err
		}
		adminDailyBucket(buckets, date).RevenueCents += cents
	}
	if err := dailyRevenue.Err(); err != nil {
		dailyRevenue.Close()
		return AdminOverview{}, err
	}
	dailyRevenue.Close()

	for _, bucket := range buckets {
		out.Daily = append(out.Daily, *bucket)
	}
	sort.Slice(out.Daily, func(i, j int) bool { return out.Daily[i].Date < out.Daily[j].Date })

	// Top organizations are ranked by consumed credits, then revenue, and only
	// include orgs with activity in the range. The owner is the billing owner,
	// not a per-member attribution.
	topModels, err := s.adminTopModelsByOrg(ctx, days)
	if err != nil {
		return AdminOverview{}, err
	}
	topRows, err := s.Pool.Query(ctx, adminOrgActivityCTEs+`
		select o.id, o.name, coalesce(o.owner_user_id::text,''), o.plan_code,
		       coalesce(w.balance_credits,0)::bigint,
		       coalesce(ord.orders,0), coalesce(ord.revenue_cents,0),
		       coalesce(sp.relay_credits,0), coalesce(sp.market_credits,0),
		       coalesce(sp.memory_credits,0), coalesce(sp.dslpha_credits,0),
		       coalesce(sbx.sandbox_credits,0),
		       coalesce(tok.requests,0),
		       coalesce(tok.input_tokens,0), coalesce(tok.output_tokens,0),
		       coalesce(tok.cache_read_tokens,0), coalesce(tok.cache_write_tokens,0),
		       coalesce(tok.total_tokens,0)
		from public.organizations o
		left join billing.wallets w on w.org_id = o.id
		left join spend sp on sp.org_id = o.id
		left join ord on ord.org_id = o.id
		left join tok on tok.org_id = o.id
		left join sbx on sbx.org_id = o.id
		where coalesce(sp.total_credits,0) > 0
		   or coalesce(ord.orders,0) > 0
		   or coalesce(tok.total_tokens,0) > 0
		   or coalesce(sbx.sandbox_credits,0) > 0
		order by coalesce(sp.total_credits,0) desc, coalesce(ord.revenue_cents,0) desc, o.name asc
		limit $2`, days, AdminOverviewTopOrgsLimit)
	if err != nil {
		return AdminOverview{}, err
	}
	for topRows.Next() {
		var row AdminTopOrg
		if err := topRows.Scan(&row.OrgID, &row.Name, &row.OwnerUserID, &row.PlanCode,
			&row.BalanceCredits, &row.Orders, &row.RevenueCents, &row.RelayCredits,
			&row.MarketCredits, &row.MemoryCredits, &row.DslphaCredits, &row.SandboxCredits,
			&row.Requests, &row.InputTokens, &row.OutputTokens, &row.CacheReadTokens,
			&row.CacheWriteTokens, &row.TotalTokens); err != nil {
			topRows.Close()
			return AdminOverview{}, err
		}
		row.CacheHitRate = cacheHitRate(row.CacheReadTokens, row.InputTokens)
		row.TopModel = topModels[row.OrgID]
		out.TopOrgs = append(out.TopOrgs, row)
	}
	if err := topRows.Err(); err != nil {
		topRows.Close()
		return AdminOverview{}, err
	}
	topRows.Close()

	// Accountable users: one row per organization owner, ordered by tokens then
	// spend. Usage and orders are org-scoped, so the owner is the only honest
	// per-user attribution the data supports.
	userRows, err := s.Pool.Query(ctx, adminOrgActivityCTEs+`
		select o.id, o.name, coalesce(o.owner_user_id::text,''), o.plan_code,
		       coalesce(w.balance_credits,0)::bigint,
		       coalesce(ord.orders,0), coalesce(ord.revenue_cents,0),
		       coalesce(sp.relay_credits,0), coalesce(sp.market_credits,0),
		       coalesce(sp.memory_credits,0), coalesce(sp.dslpha_credits,0),
		       coalesce(sbx.sandbox_credits,0),
		       coalesce(tok.requests,0),
		       coalesce(tok.input_tokens,0), coalesce(tok.output_tokens,0),
		       coalesce(tok.cache_read_tokens,0), coalesce(tok.cache_write_tokens,0),
		       coalesce(tok.total_tokens,0)
		from public.organizations o
		left join billing.wallets w on w.org_id = o.id
		left join spend sp on sp.org_id = o.id
		left join ord on ord.org_id = o.id
		left join tok on tok.org_id = o.id
		left join sbx on sbx.org_id = o.id
		where coalesce(sp.total_credits,0) > 0
		   or coalesce(ord.orders,0) > 0
		   or coalesce(tok.total_tokens,0) > 0
		   or coalesce(sbx.sandbox_credits,0) > 0
		order by coalesce(tok.total_tokens,0) desc,
		         coalesce(sp.total_credits,0) desc,
		         coalesce(ord.revenue_cents,0) desc,
		         o.name asc
		limit $2`, days, AdminOverviewUsersLimit)
	if err != nil {
		return AdminOverview{}, err
	}
	for userRows.Next() {
		var row AdminUserRow
		if err := userRows.Scan(&row.OrgID, &row.OrgName, &row.UserID, &row.PlanCode,
			&row.BalanceCredits, &row.Orders, &row.RevenueCents, &row.RelayCredits,
			&row.MarketCredits, &row.MemoryCredits, &row.DslphaCredits, &row.SandboxCredits,
			&row.Requests, &row.InputTokens, &row.OutputTokens, &row.CacheReadTokens,
			&row.CacheWriteTokens, &row.TotalTokens); err != nil {
			userRows.Close()
			return AdminOverview{}, err
		}
		row.CacheHitRate = cacheHitRate(row.CacheReadTokens, row.InputTokens)
		row.TopModel = topModels[row.OrgID]
		out.Users = append(out.Users, row)
	}
	if err := userRows.Err(); err != nil {
		userRows.Close()
		return AdminOverview{}, err
	}
	userRows.Close()

	// Resolve emails once for both tables. Missing auth.users access leaves the
	// addresses blank rather than failing the whole overview.
	ownerIDs := make([]string, 0, len(out.TopOrgs)+len(out.Users))
	for _, row := range out.TopOrgs {
		ownerIDs = append(ownerIDs, row.OwnerUserID)
	}
	for _, row := range out.Users {
		ownerIDs = append(ownerIDs, row.UserID)
	}
	emails := s.adminOwnerEmails(ctx, ownerIDs)
	for i := range out.TopOrgs {
		out.TopOrgs[i].OwnerEmail = emails[out.TopOrgs[i].OwnerUserID]
	}
	for i := range out.Users {
		out.Users[i].Email = emails[out.Users[i].UserID]
	}

	orderRows, err := s.Pool.Query(ctx, `
		select o.id, o.org_id, coalesce(org.name,''), o.sku, o.amount_cents::bigint,
		       o.credits, o.status, coalesce(o.provider,'xunhupay'), o.created_at, o.paid_at
		from billing.orders o
		left join public.organizations org on org.id = o.org_id
		where o.created_at >= `+adminSince+`
		order by o.created_at desc, o.id desc
		limit $2`, days, AdminOverviewRecentOrdersMax)
	if err != nil {
		return AdminOverview{}, err
	}
	for orderRows.Next() {
		var row AdminRecentOrder
		if err := orderRows.Scan(&row.ID, &row.OrgID, &row.OrgName, &row.SKU,
			&row.AmountCents, &row.Credits, &row.Status, &row.Provider, &row.CreatedAt, &row.PaidAt); err != nil {
			orderRows.Close()
			return AdminOverview{}, err
		}
		out.RecentOrders = append(out.RecentOrders, row)
	}
	if err := orderRows.Err(); err != nil {
		orderRows.Close()
		return AdminOverview{}, err
	}
	orderRows.Close()

	return out, nil
}
