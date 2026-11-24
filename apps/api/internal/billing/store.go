package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const CreditPerYuan = 100

var ErrNotFound = errors.New("not found")
var ErrInsufficientCredits = errors.New("insufficient credits")
var ErrProductNotEnabled = errors.New("product not enabled")
var ErrProductCap = errors.New("product monthly cap reached")
var ErrAddonNotEligible = errors.New("addon not available on this plan")

type Store struct{ Pool *pgxpool.Pool }

type Org struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	PlanCode string `json:"plan_code"`
	Role     string `json:"role"`
}

type Plan struct {
	Code                  string           `json:"code"`
	Name                  string           `json:"name"`
	PriceCents            int              `json:"price_cents"`
	DurationDays          int              `json:"duration_days"`
	IncludedCredits       int64            `json:"included_credits"`
	ProductCaps           map[string]int64 `json:"product_caps"`
	Features              map[string]any   `json:"features"`
	Product               string           `json:"product"`
	TrialDays             int              `json:"trial_days"`
	FirstMonthDiscountBps int              `json:"first_month_discount_bps"`
	AnnualDiscountBps     int              `json:"annual_discount_bps"`
}

type Wallet struct {
	BalanceCredits  int64 `json:"balance_credits"`
	ReservedCredits int64 `json:"reserved_credits"`
	Available       int64 `json:"available_credits"`
}

type ProductState struct {
	Product       string `json:"product"`
	Enabled       bool   `json:"enabled"`
	MonthlyCap    *int64 `json:"monthly_cap_credits"`
	UsedThisMonth int64  `json:"used_this_month"`
}

type BillingView struct {
	Org           Org            `json:"org"`
	Plan          Plan           `json:"plan"`
	Wallet        Wallet         `json:"wallet"`
	Products      []ProductState `json:"products"`
	AlwaysOnUntil *time.Time     `json:"always_on_until,omitempty"`
}

type Order struct {
	ID              string     `json:"id"`
	OrgID           string     `json:"org_id"`
	OutTradeNo      string     `json:"out_trade_no"`
	SKU             string     `json:"sku"`
	AmountCents     int        `json:"amount_cents"`
	Credits         int64      `json:"credits"`
	Status          string     `json:"status"`
	Provider        string     `json:"provider"`
	QRURL           string     `json:"qr_url,omitempty"`
	PayURL          string     `json:"pay_url,omitempty"`
	ProviderOrderID string     `json:"-"`
	PaidAt          *time.Time `json:"paid_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type Lease struct {
	ID              string    `json:"lease_id"`
	OrgID           string    `json:"org_id"`
	Product         string    `json:"product"`
	RunRef          string    `json:"run_ref"`
	ReservedCredits int64     `json:"reserved_credits"`
	Status          string    `json:"status"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

func slugFor(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "org"
	}
	if b.Len() > 32 {
		return b.String()[:32]
	}
	return b.String()
}

// EnsureOrg returns the user's first organization, creating a personal org,
// wallet and free entitlement on first use. No historical users exist.
func (s *Store) EnsureOrg(ctx context.Context, userID, email string) (Org, error) {
	org, err := s.orgForUser(ctx, userID)
	if err == nil {
		return org, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Org{}, err
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Org{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext($1))", "roost-org:"+userID); err != nil {
		return Org{}, err
	}
	// Another request may have created it while this one waited.
	org, err = s.orgForUserTx(ctx, tx, userID)
	if err == nil {
		return org, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return Org{}, err
	}

	name := strings.Split(email, "@")[0]
	if strings.TrimSpace(name) == "" {
		name = "Personal"
	}
	slug := slugFor(name) + "-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	var orgID string
	if err := tx.QueryRow(ctx, `
		insert into public.organizations (name, slug, owner_user_id, plan_code)
		values ($1, $2, $3, 'dsalpha_free') returning id`, name, slug, userID).Scan(&orgID); err != nil {
		return Org{}, err
	}
	if _, err := tx.Exec(ctx, `insert into public.organization_members (org_id, user_id, role) values ($1,$2,'owner')`, orgID, userID); err != nil {
		return Org{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into billing.wallets (org_id, balance_credits) values ($1, 1000)
		on conflict (org_id) do nothing`, orgID); err != nil {
		return Org{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into billing.ledger (org_id, delta_credits, reason, idempotency_key)
		values ($1, 1000, 'free_grant', $2)
		on conflict (idempotency_key) do nothing`, orgID, "free:"+orgID); err != nil {
		return Org{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into billing.subscriptions (org_id, plan_code, status, period_start, period_end)
		values ($1, 'dsalpha_free', 'active', now(), now() + interval '100 years')
		on conflict (org_id) do nothing`, orgID); err != nil {
		return Org{}, err
	}
	if err := s.applyEntitlements(ctx, tx, orgID, "dsalpha_free"); err != nil {
		return Org{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Org{}, err
	}
	return Org{ID: orgID, Name: name, Slug: slug, PlanCode: "dsalpha_free", Role: "owner"}, nil
}

func (s *Store) orgForUser(ctx context.Context, userID string) (Org, error) {
	return scanOrg(s.Pool.QueryRow(ctx, `
		select o.id, o.name, o.slug, o.plan_code, m.role
		from public.organizations o
		join public.organization_members m on m.org_id = o.id
		where m.user_id = $1 and o.status = 'active'
		order by m.created_at limit 1`, userID))
}

func (s *Store) orgForUserTx(ctx context.Context, tx pgx.Tx, userID string) (Org, error) {
	return scanOrg(tx.QueryRow(ctx, `
		select o.id, o.name, o.slug, o.plan_code, m.role
		from public.organizations o
		join public.organization_members m on m.org_id = o.id
		where m.user_id = $1 and o.status = 'active'
		order by m.created_at limit 1`, userID))
}

func scanOrg(row pgx.Row) (Org, error) {
	var o Org
	if err := row.Scan(&o.ID, &o.Name, &o.Slug, &o.PlanCode, &o.Role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Org{}, ErrNotFound
		}
		return Org{}, err
	}
	return o, nil
}

func (s *Store) Membership(ctx context.Context, orgID, userID string) (string, error) {
	var role string
	err := s.Pool.QueryRow(ctx, `select role from public.organization_members where org_id=$1 and user_id=$2`, orgID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}

func (s *Store) Plan(ctx context.Context, code string) (Plan, error) {
	var p Plan
	var caps, features []byte
	err := s.Pool.QueryRow(ctx, `select code,name,price_cents,duration_days,included_credits,product_caps,features from billing.plans where code=$1 and active`, code).Scan(&p.Code, &p.Name, &p.PriceCents, &p.DurationDays, &p.IncludedCredits, &caps, &features)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	if err != nil {
		return Plan{}, err
	}
	_ = jsonUnmarshal(caps, &p.ProductCaps)
	_ = jsonUnmarshal(features, &p.Features)
	return p, nil
}

func (s *Store) Plans(ctx context.Context) ([]Plan, error) {
	rows, err := s.Pool.Query(ctx, `select code,name,price_cents,duration_days,included_credits,product_caps,features,product,trial_days,first_month_discount_bps,annual_discount_bps from billing.plans where active order by sort_order, price_cents`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Plan, 0)
	for rows.Next() {
		var p Plan
		var caps, features []byte
		if err := rows.Scan(&p.Code, &p.Name, &p.PriceCents, &p.DurationDays, &p.IncludedCredits, &caps, &features, &p.Product, &p.TrialDays, &p.FirstMonthDiscountBps, &p.AnnualDiscountBps); err != nil {
			return nil, err
		}
		_ = jsonUnmarshal(caps, &p.ProductCaps)
		_ = jsonUnmarshal(features, &p.Features)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Billing(ctx context.Context, orgID string) (BillingView, error) {
	var o Org
	if err := s.Pool.QueryRow(ctx, `select id,name,slug,plan_code from public.organizations where id=$1 and status='active'`, orgID).Scan(&o.ID, &o.Name, &o.Slug, &o.PlanCode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BillingView{}, ErrNotFound
		}
		return BillingView{}, err
	}
	effectivePlan, err := s.currentPlan(ctx, orgID, o.PlanCode)
	if err != nil {
		return BillingView{}, err
	}
	o.PlanCode = effectivePlan
	plan, err := s.Plan(ctx, o.PlanCode)
	if err != nil {
		return BillingView{}, err
	}
	var w Wallet
	if err := s.Pool.QueryRow(ctx, `select balance_credits,reserved_credits from billing.wallets where org_id=$1`, orgID).Scan(&w.BalanceCredits, &w.ReservedCredits); err != nil {
		return BillingView{}, err
	}
	w.Available = w.BalanceCredits - w.ReservedCredits
	rows, err := s.Pool.Query(ctx, `
		select e.product, e.enabled, e.monthly_cap_credits,
		       coalesce((select -sum(l.delta_credits) from billing.ledger l
		                 where l.org_id=e.org_id and l.product=e.product
		                   and l.delta_credits < 0
		                   and l.created_at >= date_trunc('month', now())),0)
		from billing.entitlements e where e.org_id=$1 order by e.product`, orgID)
	if err != nil {
		return BillingView{}, err
	}
	defer rows.Close()
	products := make([]ProductState, 0)
	for rows.Next() {
		var p ProductState
		if err := rows.Scan(&p.Product, &p.Enabled, &p.MonthlyCap, &p.UsedThisMonth); err != nil {
			return BillingView{}, err
		}
		products = append(products, p)
	}
	view := BillingView{Org: o, Plan: plan, Wallet: w, Products: products}
	if until, err := s.AlwaysOnUntil(ctx, orgID); err == nil && until.After(time.Now()) {
		view.AlwaysOnUntil = &until
	}
	return view, rows.Err()
}

// AlwaysOnUntil returns the org's always-on add-on expiry, zero when unset.
// The expiry rides on the fulfilled add-on order's raw payload, so the add-on
// needs no table of its own.
func (s *Store) AlwaysOnUntil(ctx context.Context, orgID string) (time.Time, error) {
	var until *time.Time
	err := s.Pool.QueryRow(ctx, `
		select max((raw->>'addon_until')::timestamptz)
		from billing.orders
		where org_id=$1 and sku=$2 and status='fulfilled' and raw ? 'addon_until'`,
		orgID, AlwaysOnSKU).Scan(&until)
	if err != nil || until == nil {
		return time.Time{}, err
	}
	return *until, nil
}

// AlwaysOnActive reports whether the org currently holds the always-on add-on.
func (s *Store) AlwaysOnActive(ctx context.Context, orgID string) bool {
	until, err := s.AlwaysOnUntil(ctx, orgID)
	return err == nil && until.After(time.Now())
}

// currentPlan lazily expires a paid period. Credits are prepaid and stay in
// the wallet; only the plan's product caps/validity are revoked.
func (s *Store) currentPlan(ctx context.Context, orgID, planCode string) (string, error) {
	if planCode == "free" || planCode == "dsalpha_free" || planCode == "" {
		if planCode == "" {
			planCode = "dsalpha_free"
		}
		return planCode, nil
	}
	var end time.Time
	var status string
	err := s.Pool.QueryRow(ctx, `select period_end,status from billing.subscriptions where org_id=$1`, orgID).Scan(&end, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "free", nil
	}
	if err != nil {
		return "", err
	}
	if status == "active" && end.After(time.Now()) {
		return planCode, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `update public.organizations set plan_code='dsalpha_free',updated_at=now() where id=$1`, orgID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `update billing.subscriptions set status='expired',updated_at=now() where org_id=$1`, orgID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `delete from billing.entitlements where org_id=$1`, orgID); err != nil {
		return "", err
	}
	if err := s.applyEntitlements(ctx, tx, orgID, "dsalpha_free"); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return "free", nil
}

func (s *Store) applyEntitlements(ctx context.Context, tx pgx.Tx, orgID, planCode string) error {
	plan, err := s.PlanTx(ctx, tx, planCode)
	if err != nil {
		return err
	}
	var validUntil *time.Time
	if plan.DurationDays > 0 {
		var end time.Time
		if err := tx.QueryRow(ctx, `select period_end from billing.subscriptions where org_id=$1`, orgID).Scan(&end); err == nil {
			validUntil = &end
		}
	}
	for product, capCredits := range plan.ProductCaps {
		capValue := capCredits
		if _, err := tx.Exec(ctx, `
			insert into billing.entitlements (org_id,product,enabled,monthly_cap_credits,valid_until)
			values ($1,$2,true,$3,$4)
			on conflict (org_id,product) do update set enabled=true, monthly_cap_credits=excluded.monthly_cap_credits, valid_until=excluded.valid_until, updated_at=now()`, orgID, product, capValue, validUntil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) PlanTx(ctx context.Context, tx pgx.Tx, code string) (Plan, error) {
	var p Plan
	var caps, features []byte
	err := tx.QueryRow(ctx, `select code,name,price_cents,duration_days,included_credits,product_caps,features from billing.plans where code=$1 and active`, code).Scan(&p.Code, &p.Name, &p.PriceCents, &p.DurationDays, &p.IncludedCredits, &caps, &features)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	if err != nil {
		return Plan{}, err
	}
	_ = jsonUnmarshal(caps, &p.ProductCaps)
	_ = jsonUnmarshal(features, &p.Features)
	return p, nil
}

func (s *Store) OrgForUser(ctx context.Context, userID string) (Org, error) {
	return s.orgForUser(ctx, userID)
}

func (s *Store) EnsureOrgByUserID(ctx context.Context, userID string) (Org, error) {
	// Product bridges only have the Supabase principal id. Do not read auth.users
	// from the billing role; a personal org is enough to start the relationship.
	return s.EnsureOrg(ctx, userID, userID+"@roost.local")
}
