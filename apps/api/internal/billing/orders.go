package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type CreditPack struct {
	SKU          string
	AmountCents  int
	Credits      int64
	Title        string
	DurationDays int
	PlanCode     string
	Trial        bool
	Annual       bool
}

// AlwaysOnSKU buys the always-on sandbox add-on. It carries no credits: its
// effect is the billing.addons expiry that /api/auth/validate reports as the
// workspace:always_on scope.
const AlwaysOnSKU = "addon_always_on_30d"

var CreditPacks = map[string]CreditPack{
	"credit_50":  {SKU: "credit_50", AmountCents: 5000, Credits: 5000, Title: "Roost 5,000 credits"},
	"credit_100": {SKU: "credit_100", AmountCents: 10000, Credits: 10000, Title: "Roost 10,000 credits"},
	"credit_500": {SKU: "credit_500", AmountCents: 50000, Credits: 50000, Title: "Roost 50,000 credits"},
	// Token packs are the same credit balance, sold under a token-first name.
	"token_50":  {SKU: "token_50", AmountCents: 5000, Credits: 5000, Title: "Roost 5,000 token credits"},
	"token_100": {SKU: "token_100", AmountCents: 10000, Credits: 10000, Title: "Roost 10,000 token credits"},
	"token_500": {SKU: "token_500", AmountCents: 50000, Credits: 50000, Title: "Roost 50,000 token credits"},
	AlwaysOnSKU: {SKU: AlwaysOnSKU, AmountCents: 4900, Credits: 0, Title: "Roost Always-on · 30 days"},
}

// ErrUSDTAmountTaken means another open order already quotes this exact USDT
// amount; the caller should quote a different suffix and retry.
var ErrUSDTAmountTaken = errors.New("usdt amount already in use")

// ErrTrialUsed is returned when an organization already consumed its trial.
var ErrTrialUsed = errors.New("trial already used")

// planSKU is the parsed shape of plan_<code>_30d / plan_<code>_365d /
// trial_<code>_7d. Code may itself contain underscores, so the duration is the
// final underscore-separated token.
type planSKU struct {
	Code   string
	Days   int
	Trial  bool
	Annual bool
}

func parsePlanSKU(sku string) (planSKU, bool) {
	prefix, trial := "plan_", false
	if strings.HasPrefix(sku, "trial_") {
		prefix, trial = "trial_", true
	}
	if !strings.HasPrefix(sku, prefix) {
		return planSKU{}, false
	}
	rest := strings.TrimPrefix(sku, prefix)
	i := strings.LastIndex(rest, "_")
	if i <= 0 {
		return planSKU{}, false
	}
	code, suffix := rest[:i], rest[i+1:]
	var days int
	switch suffix {
	case "30d":
		days = 30
	case "365d":
		days = 365
	case "7d":
		days = 7
	default:
		return planSKU{}, false
	}
	return planSKU{Code: code, Days: days, Trial: trial, Annual: days >= 365}, true
}

func trialCredits(plan Plan) int64 {
	if v, ok := plan.Features["trial_credits"].(float64); ok && v > 0 {
		return int64(v)
	}
	return 0
}

func randomTradeNo() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

// Quote returns the server-side price and credits for a SKU. Client input never
// supplies either value. It is the org-less form kept for tests; HTTP and order
// creation use QuoteForOrg so first-month/trial discounts can be evaluated.
func (s *Store) Quote(ctx context.Context, sku string) (CreditPack, string, error) {
	return s.QuoteForOrg(ctx, "", sku)
}

// QuoteForOrg prices a SKU for one organization. Plan SKUs:
//
//	plan_<code>_30d   monthly; first paid plan gets the first-month discount
//	plan_<code>_365d  annual; 12 months at the plan's annual discount
//	trial_<code>_7d   free trial once per org, with trial_credits
func (s *Store) QuoteForOrg(ctx context.Context, orgID, sku string) (CreditPack, string, error) {
	if pack, ok := CreditPacks[sku]; ok {
		return pack, "credits", nil
	}
	parsed, ok := parsePlanSKU(sku)
	if !ok {
		return CreditPack{}, "", ErrNotFound
	}
	plan, err := s.Plan(ctx, parsed.Code)
	if err != nil {
		return CreditPack{}, "", err
	}
	days := parsed.Days
	if days == 0 {
		days = plan.DurationDays
	}
	amount := plan.PriceCents
	credits := plan.IncludedCredits
	if parsed.Trial {
		if orgID != "" {
			var used bool
			if err := s.Pool.QueryRow(ctx, `
				select exists(select 1 from billing.orders
					where org_id=$1 and sku like 'trial_%'
					  and status not in ('expired','failed'))`, orgID).Scan(&used); err != nil {
				return CreditPack{}, "", err
			}
			if used {
				return CreditPack{}, "", ErrTrialUsed
			}
		}
		amount = 0
		credits = trialCredits(plan)
	} else if parsed.Annual {
		bps := plan.AnnualDiscountBps
		amount = int((int64(plan.PriceCents)*12*(10000-int64(bps)) + 5000) / 10000)
		credits = plan.IncludedCredits * 12
	} else if orgID != "" {
		var paidBefore bool
		if err := s.Pool.QueryRow(ctx, `
			select exists(select 1 from billing.orders
				where org_id=$1 and status='fulfilled'
				  and sku like 'plan_%' and amount_cents > 0)`, orgID).Scan(&paidBefore); err != nil {
			return CreditPack{}, "", err
		}
		if !paidBefore && plan.FirstMonthDiscountBps > 0 {
			bps := plan.FirstMonthDiscountBps
			amount = int((int64(plan.PriceCents)*(10000-int64(bps)) + 5000) / 10000)
		}
	}
	title := "Roost " + plan.Name
	if parsed.Trial {
		title += " · 7 天试用"
	} else if parsed.Annual {
		title += " · 年付"
	} else {
		title += " · 30 天"
	}
	return CreditPack{SKU: sku, AmountCents: amount, Credits: credits, Title: title,
		DurationDays: days, PlanCode: parsed.Code, Trial: parsed.Trial, Annual: parsed.Annual}, "plan:" + parsed.Code, nil
}

// Custom top-up: the user chooses the amount, and credits are 1:1 with cents
// (100 credits = ¥1). The price is always derived server-side from the amount,
// so a client cannot buy a different number of credits than it pays for.
const (
	CustomTopUpSKU      = "credit_custom"
	CustomTopUpMinCents = 1000   // ¥10
	CustomTopUpMaxCents = 500000 // ¥5000
)

var ErrInvalidTopUpAmount = errors.New("invalid top-up amount")

// CustomTopUpQuote validates a custom amount and returns the pack it buys.
func CustomTopUpQuote(amountCents int64) (CreditPack, error) {
	if amountCents < CustomTopUpMinCents || amountCents > CustomTopUpMaxCents {
		return CreditPack{}, ErrInvalidTopUpAmount
	}
	return CreditPack{
		SKU:         CustomTopUpSKU,
		AmountCents: int(amountCents),
		Credits:     amountCents,
		Title:       "Roost 自定义充值",
	}, nil
}

// CreateCustomOrder persists a custom top-up before the provider checkout is
// created, matching CreateOrder's lifecycle (created -> pending -> fulfilled).
func (s *Store) CreateCustomOrder(ctx context.Context, orgID string, amountCents int64) (Order, error) {
	pack, err := CustomTopUpQuote(amountCents)
	if err != nil {
		return Order{}, err
	}
	var o Order
	err = s.Pool.QueryRow(ctx, `
		insert into billing.orders (org_id,out_trade_no,sku,amount_cents,credits,status)
		values ($1,$2,$3,$4,$5,'created')
		returning id,org_id,out_trade_no,sku,amount_cents,credits,status,created_at`,
		orgID, randomTradeNo(), pack.SKU, pack.AmountCents, pack.Credits,
	).Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.CreatedAt)
	return o, err
}

func (s *Store) CreateOrder(ctx context.Context, orgID, sku string) (Order, error) {
	pack, kind, err := s.QuoteForOrg(ctx, orgID, sku)
	if err != nil {
		return Order{}, err
	}
	if sku == AlwaysOnSKU {
		// The add-on is a Pro/Max optional, never a Starter one. Read the plan
		// through the same lazy-expiry path Billing uses so an elapsed paid
		// period cannot buy it.
		var planCode string
		if err := s.Pool.QueryRow(ctx, `select plan_code from public.organizations where id=$1`, orgID).Scan(&planCode); err != nil {
			return Order{}, err
		}
		effective, err := s.currentPlan(ctx, orgID, planCode)
		if err != nil {
			return Order{}, err
		}
		if effective != "pro" && effective != "max" && effective != "dsalpha_pro" && effective != "dsalpha_max" {
			return Order{}, ErrAddonNotEligible
		}
	}
	var o Order
	err = s.Pool.QueryRow(ctx, `
		insert into billing.orders (org_id,out_trade_no,sku,amount_cents,credits,status)
		values ($1,$2,$3,$4,$5,'created')
		returning id,org_id,out_trade_no,sku,amount_cents,credits,status,created_at`,
		orgID, randomTradeNo(), sku, pack.AmountCents, pack.Credits,
	).Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.CreatedAt)
	_ = kind
	if err != nil {
		return Order{}, err
	}
	if pack.Trial {
		if err := s.activateTrial(ctx, o, pack); err != nil {
			return Order{}, err
		}
		o.Status = "fulfilled"
	}
	return o, nil
}

// activateTrial fulfils a zero-amount trial order without the payment provider.
// Trial credits are a bounded feature (trial_credits), never the plan's full
// monthly allowance.
func (s *Store) activateTrial(ctx context.Context, o Order, pack CreditPack) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `select status from billing.orders where id=$1 for update`, o.ID).Scan(&o.Status); err != nil {
		return err
	}
	if o.Status == "fulfilled" {
		return nil
	}
	if pack.Credits > 0 {
		if _, err := tx.Exec(ctx, `
			insert into billing.ledger (org_id,product,delta_credits,reason,ref_id,idempotency_key)
			values ($1,'dsalpha',$2,'trial_grant',$3,$4)
			on conflict (idempotency_key) do nothing`, o.OrgID, pack.Credits, o.ID, "trial:"+o.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update billing.wallets set balance_credits=balance_credits+$2,version=version+1,updated_at=now() where org_id=$1`, o.OrgID, pack.Credits); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		insert into billing.subscriptions (org_id,plan_code,status,period_start,period_end,source_order_id)
		values ($1,$2,'active',now(),now()+make_interval(days=>$3),$4)
		on conflict (org_id) do update set
			plan_code=excluded.plan_code,status='active',
			period_start=now(),period_end=excluded.period_end,
			source_order_id=excluded.source_order_id,updated_at=now()`,
		o.OrgID, pack.PlanCode, pack.DurationDays, o.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update public.organizations set plan_code=$2,updated_at=now() where id=$1`, o.OrgID, pack.PlanCode); err != nil {
		return err
	}
	if err := s.applyEntitlements(ctx, tx, o.OrgID, pack.PlanCode); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update billing.orders set status='fulfilled',fulfilled_at=now(),updated_at=now() where id=$1`, o.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) AttachPayment(ctx context.Context, orderID, providerOrderID, qrURL, payURL string) error {
	_, err := s.Pool.Exec(ctx, `update billing.orders set status='pending',provider_order_id=$2,qr_url=$3,pay_url=$4,updated_at=now() where id=$1 and status in ('created','pending')`, orderID, providerOrderID, qrURL, payURL)
	return err
}

func (s *Store) GetOrder(ctx context.Context, orgID, orderID string) (Order, error) {
	var o Order
	var raw []byte
	err := s.Pool.QueryRow(ctx, `
		select id,org_id,out_trade_no,sku,amount_cents,credits,status,coalesce(qr_url,''),coalesce(pay_url,''),provider,raw,paid_at,created_at
		from billing.orders where id=$1 and org_id=$2`, orderID, orgID).
		Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.QRURL, &o.PayURL, &o.Provider, &raw, &o.PaidAt, &o.CreatedAt)
	o.USDT = usdtFromRaw(raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

func (s *Store) OrderByTradeNo(ctx context.Context, tradeNo string) (Order, error) {
	var o Order
	err := s.Pool.QueryRow(ctx, `
		select id,org_id,out_trade_no,sku,amount_cents,credits,status,coalesce(qr_url,''),coalesce(pay_url,''),paid_at,created_at
		from billing.orders where out_trade_no=$1`, tradeNo).
		Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.QRURL, &o.PayURL, &o.PaidAt, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

// Fulfill marks an order paid and applies its effect exactly once. amountCents
// is the amount the provider says was paid; callers must verify it first.
func (s *Store) Fulfill(ctx context.Context, orderID, providerTradeNo string, amountCents int, raw []byte) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var o Order
	err = tx.QueryRow(ctx, `select id,org_id,sku,amount_cents,credits,status from billing.orders where id=$1 for update`, orderID).
		Scan(&o.ID, &o.OrgID, &o.SKU, &o.AmountCents, &o.Credits, &o.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if amountCents != o.AmountCents {
		return fmt.Errorf("payment amount mismatch: got %d want %d", amountCents, o.AmountCents)
	}
	if o.Status == "fulfilled" {
		return nil
	}
	if o.Status == "refunded" {
		return fmt.Errorf("order refunded")
	}
	rawJSON := string(raw)
	if o.SKU == AlwaysOnSKU {
		// Stack on the current expiry so early renewals never lose days; the
		// expiry lives on this order's raw payload.
		var base time.Time
		if err := tx.QueryRow(ctx, `
			select greatest(coalesce(max((raw->>'addon_until')::timestamptz), now()), now())
			from billing.orders
			where org_id=$1 and sku=$2 and status in ('paid','fulfilled') and raw ? 'addon_until'`,
			o.OrgID, AlwaysOnSKU).Scan(&base); err != nil {
			return err
		}
		until := base.Add(30 * 24 * time.Hour)
		payload := map[string]any{}
		_ = json.Unmarshal(raw, &payload)
		payload["addon_until"] = until.UTC().Format(time.RFC3339)
		merged, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		rawJSON = string(merged)
	}
	if _, err := tx.Exec(ctx, `
		update billing.orders set status='paid',provider_trade_no=$2,paid_at=now(),raw=$3,updated_at=now()
		where id=$1`, o.ID, providerTradeNo, rawJSON); err != nil {
		return err
	}
	if o.Credits > 0 {
		if _, err := tx.Exec(ctx, `
			insert into billing.ledger (org_id,delta_credits,reason,ref_id,idempotency_key)
			values ($1,$2,'payment',$3,$4)
			on conflict (idempotency_key) do nothing`, o.OrgID, o.Credits, o.ID, "order:"+o.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			update billing.wallets set balance_credits=balance_credits+$2,version=version+1,updated_at=now()
			where org_id=$1`, o.OrgID, o.Credits); err != nil {
			return err
		}
	}
	if parsed, ok := parsePlanSKU(o.SKU); ok && !parsed.Trial {
		code := parsed.Code
		duration := parsed.Days
		if duration <= 0 {
			duration = 30
		}
		if _, err := tx.Exec(ctx, `
			insert into billing.subscriptions (org_id,plan_code,status,period_start,period_end,source_order_id)
			values ($1,$2,'active',now(),now()+make_interval(days=>$3),$4)
			on conflict (org_id) do update set
				plan_code=excluded.plan_code,
				status='active',
				period_start=now(),
				period_end=case
					when billing.subscriptions.plan_code=excluded.plan_code
					     and billing.subscriptions.period_end > now()
					then billing.subscriptions.period_end + make_interval(days=>$3)
					else now() + make_interval(days=>$3)
				end,
				source_order_id=excluded.source_order_id,
				updated_at=now()`, o.OrgID, code, duration, o.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update public.organizations set plan_code=$2,updated_at=now() where id=$1`, o.OrgID, code); err != nil {
			return err
		}
		if err := s.applyEntitlements(ctx, tx, o.OrgID, code); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `update billing.orders set status='fulfilled',fulfilled_at=now(),updated_at=now() where id=$1`, o.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ListOrders(ctx context.Context, orgID string, limit int) ([]Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := s.Pool.Query(ctx, `
		select id,org_id,out_trade_no,sku,amount_cents,credits,status,coalesce(qr_url,''),coalesce(pay_url,''),paid_at,created_at
		from billing.orders where org_id=$1 order by created_at desc limit $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Order, 0)
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.QRURL, &o.PayURL, &o.PaidAt, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func nowPtr() *time.Time { t := time.Now(); return &t }

// MarkOrderFailed closes an order whose provider checkout could not be created.
// It only touches a still-'created' row: a race with a notify must never flip a
// paid order to failed.
func (s *Store) MarkOrderFailed(ctx context.Context, orderID, reason string) error {
	_, err := s.Pool.Exec(ctx, `
		update billing.orders
		set status='failed', raw = raw || jsonb_build_object('failure', $2::text), updated_at=now()
		where id=$1 and status='created'`, orderID, reason)
	return err
}

// ExpireStaleOrders closes abandoned checkouts: an order that stayed
// created/pending past the TTL can never be paid, so it should stop reading as
// "in progress". A later provider notify still fulfills it (money first).
func (s *Store) ExpireStaleOrders(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		olderThan = 2 * time.Hour
	}
	tag, err := s.Pool.Exec(ctx, `
		update billing.orders
		set status='expired', updated_at=now()
		where status in ('created','pending')
		  and created_at < now() - make_interval(secs => $1)`, olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListReconcileOrders returns pending orders old enough that a notify should
// already have arrived, newest window first. The reconciler queries the
// provider for each one.
func (s *Store) ListReconcileOrders(ctx context.Context, minAge time.Duration, limit int) ([]Order, error) {
	if minAge <= 0 {
		minAge = time.Minute
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `
		select id,org_id,out_trade_no,sku,amount_cents,credits,status,coalesce(qr_url,''),coalesce(pay_url,''),paid_at,created_at
		from billing.orders
		where status='pending'
		  and created_at < now() - make_interval(secs => $1)
		  and created_at > now() - interval '24 hours'
		  and (raw->>'reconcile_at' is null
		       or (raw->>'reconcile_at')::timestamptz < now() - interval '10 minutes')
		order by created_at
		limit $2`, minAge.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Order, 0, limit)
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.QRURL, &o.PayURL, &o.PaidAt, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// TouchOrderReconcile records when the reconciler last queried the provider so
// an unpaid order is not re-queried every tick.
func (s *Store) TouchOrderReconcile(ctx context.Context, orderID string) error {
	_, err := s.Pool.Exec(ctx, `
		update billing.orders
		set raw = raw || jsonb_build_object('reconcile_at', now()), updated_at=now()
		where id=$1 and status='pending'`, orderID)
	return err
}

// CloseOrder marks one order with a terminal non-paid status ('expired' for a
// provider-cancelled checkout). Paid orders are never touched.
func (s *Store) CloseOrder(ctx context.Context, orderID, status string) error {
	switch status {
	case "expired", "failed":
	default:
		return fmt.Errorf("close order: unsupported status %q", status)
	}
	_, err := s.Pool.Exec(ctx, `
		update billing.orders set status=$2, updated_at=now()
		where id=$1 and status in ('created','pending')`, orderID, status)
	return err
}

// GetOrderByID loads an order without an org scope; admin-only callers use it.
func (s *Store) GetOrderByID(ctx context.Context, orderID string) (Order, error) {
	var o Order
	err := s.Pool.QueryRow(ctx, `
		select id,org_id,out_trade_no,sku,amount_cents,credits,status,coalesce(qr_url,''),coalesce(pay_url,''),paid_at,created_at
		from billing.orders where id=$1`, orderID).
		Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.QRURL, &o.PayURL, &o.PaidAt, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

// CreateUSDTOrder persists an order that will be paid in USDT to the operator's
// deposit address. The quote lives in raw so reconciliation and the console
// read the same numbers; the unique amount is what makes a shared address
// reconcilable, so the caller passes an amount no other open order holds.
func (s *Store) CreateUSDTOrder(ctx context.Context, orgID, sku string, amountCents, credits int64, provider, chain, address, amount string, amountMicro int64, rate float64, expiresAt time.Time) (Order, error) {
	if amountCents <= 0 || credits < 0 {
		return Order{}, ErrAddonNotEligible
	}
	raw, err := json.Marshal(map[string]any{
		"usdt_chain":        chain,
		"usdt_address":      address,
		"usdt_amount":       amount,
		"usdt_amount_micro": amountMicro,
		"usdt_rate":         rate,
		"usdt_expires_at":   expiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return Order{}, err
	}
	var o Order
	err = s.Pool.QueryRow(ctx, `
		insert into billing.orders (org_id,out_trade_no,sku,amount_cents,credits,status,provider,raw)
		values ($1,$2,$3,$4,$5,'pending',$6,$7)
		returning id,org_id,out_trade_no,sku,amount_cents,credits,status,provider,created_at`,
		orgID, randomTradeNo(), sku, amountCents, credits, provider, raw,
	).Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.Provider, &o.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Another order took this amount between the check and the insert.
			return Order{}, ErrUSDTAmountTaken
		}
		return Order{}, err
	}
	o.USDT = &USDTInstruction{
		Chain:       chain,
		Address:     address,
		Amount:      amount,
		AmountMicro: amountMicro,
		Rate:        rate,
		ExpiresAt:   expiresAt.UTC(),
	}
	return o, nil
}

// USDTInstruction is the payment detail shown for a direct-USDT order.
type USDTInstruction struct {
	Chain       string    `json:"chain"`
	Address     string    `json:"address"`
	Amount      string    `json:"amount"`
	AmountMicro int64     `json:"-"`
	Rate        float64   `json:"rate"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// openUSDTOrder loads a pending USDT order together with its stored quote.
func (s *Store) openUSDTOrder(ctx context.Context, orderID string) (Order, error) {
	var o Order
	var raw []byte
	err := s.Pool.QueryRow(ctx, `
		select id,org_id,out_trade_no,sku,amount_cents,credits,status,provider,raw,created_at
		from billing.orders where id=$1`, orderID).
		Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.Provider, &raw, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	o.USDT = usdtFromRaw(raw)
	return o, nil
}

func usdtFromRaw(raw []byte) *USDTInstruction {
	if len(raw) == 0 {
		return nil
	}
	var payload struct {
		Chain       string  `json:"usdt_chain"`
		Address     string  `json:"usdt_address"`
		Amount      string  `json:"usdt_amount"`
		AmountMicro int64   `json:"usdt_amount_micro"`
		Rate        float64 `json:"usdt_rate"`
		ExpiresAt   string  `json:"usdt_expires_at"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Address == "" {
		return nil
	}
	expires, _ := time.Parse(time.RFC3339, payload.ExpiresAt)
	return &USDTInstruction{
		Chain:       payload.Chain,
		Address:     payload.Address,
		Amount:      payload.Amount,
		AmountMicro: payload.AmountMicro,
		Rate:        payload.Rate,
		ExpiresAt:   expires,
	}
}

// PendingUSDTOrders lists unpaid USDT orders that reconciliation may settle.
func (s *Store) PendingUSDTOrders(ctx context.Context, provider string, limit int) ([]Order, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `
		select id,org_id,out_trade_no,sku,amount_cents,credits,status,provider,raw,created_at
		from billing.orders
		where status='pending' and provider=$1 and created_at > now() - interval '24 hours'
		order by created_at
		limit $2`, provider, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Order, 0, limit)
	for rows.Next() {
		var o Order
		var raw []byte
		if err := rows.Scan(&o.ID, &o.OrgID, &o.OutTradeNo, &o.SKU, &o.AmountCents, &o.Credits, &o.Status, &o.Provider, &raw, &o.CreatedAt); err != nil {
			return nil, err
		}
		o.USDT = usdtFromRaw(raw)
		out = append(out, o)
	}
	return out, rows.Err()
}

// USDTAmountInUse reports whether an unpaid USDT order already quotes this
// exact amount on this chain; quotes must be unique to be matchable.
func (s *Store) USDTAmountInUse(ctx context.Context, provider, chain, amount string) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx, `
		select exists(
			select 1 from billing.orders
			where provider=$1 and status in ('created','pending')
			  and raw->>'usdt_chain'=$2 and raw->>'usdt_amount'=$3)`,
		provider, chain, amount).Scan(&exists)
	return exists, err
}

// ExpireStaleUSDTOrders closes USDT orders whose quote has expired.
func (s *Store) ExpireStaleUSDTOrders(ctx context.Context, provider string, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		olderThan = 30 * time.Minute
	}
	tag, err := s.Pool.Exec(ctx, `
		update billing.orders
		set status='expired', updated_at=now()
		where status in ('created','pending') and provider=$1
		  and created_at < now() - make_interval(secs => $2)`, provider, olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
