package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

type Entitlement struct {
	OrgID      string `json:"org_id"`
	Product    string `json:"product"`
	Enabled    bool   `json:"enabled"`
	PlanCode   string `json:"plan_code"`
	MonthlyCap *int64 `json:"monthly_cap_credits"`
	Available  int64  `json:"available_credits"`
	Balance    int64  `json:"balance_credits"`
	Reserved   int64  `json:"reserved_credits"`
}

func (s *Store) Resolve(ctx context.Context, orgID, product string) (Entitlement, error) {
	var storedPlan string
	if err := s.Pool.QueryRow(ctx, `select plan_code from public.organizations where id=$1`, orgID).Scan(&storedPlan); err != nil {
		return Entitlement{}, err
	}
	// Lazy-expire the paid period before reading caps; prepaid credits stay.
	if _, err := s.currentPlan(ctx, orgID, storedPlan); err != nil {
		return Entitlement{}, err
	}
	var e Entitlement
	err := s.Pool.QueryRow(ctx, `
		select o.id,o.plan_code,e.enabled,e.monthly_cap_credits,
		       w.balance_credits,w.reserved_credits
		from public.organizations o
		join billing.wallets w on w.org_id=o.id
		join billing.entitlements e on e.org_id=o.id and e.product=$2
		where o.id=$1 and o.status='active'
		  and (e.valid_until is null or e.valid_until > now())`,
		orgID, product).Scan(&e.OrgID, &e.PlanCode, &e.Enabled, &e.MonthlyCap, &e.Balance, &e.Reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entitlement{}, ErrProductNotEnabled
	}
	if err != nil {
		return Entitlement{}, err
	}
	e.Product = product
	e.Available = e.Balance - e.Reserved
	return e, nil
}

// MonthlyUsage is the product's metered spend this calendar month, net of
// refunds. Grants and admin adjustments are not usage and are excluded, the
// same definition Reserve uses for the product cap.
func (s *Store) MonthlyUsage(ctx context.Context, orgID, product string) (int64, error) {
	var used int64
	err := s.Pool.QueryRow(ctx, `
		select coalesce(-sum(delta_credits),0) from billing.ledger
		where org_id=$1 and product=$2
		  and reason in ('usage','relay_usage','sandbox_usage','sandbox_refund')
		  and created_at >= date_trunc('month', now())`, orgID, product).Scan(&used)
	return used, err
}

func (s *Store) Reserve(ctx context.Context, orgID, product, runRef string, estimated int64, ttl time.Duration) (Lease, error) {
	if estimated < 0 {
		return Lease{}, errors.New("estimated credits must be >= 0")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, "wallet:"+orgID); err != nil {
		return Lease{}, err
	}
	var planCode string
	var enabled bool
	var capCredits *int64
	if err := tx.QueryRow(ctx, `
		select o.plan_code,e.enabled,e.monthly_cap_credits
		from public.organizations o
		join billing.entitlements e on e.org_id=o.id and e.product=$2
		where o.id=$1 and o.status='active' and (e.valid_until is null or e.valid_until > now())`,
		orgID, product).Scan(&planCode, &enabled, &capCredits); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Lease{}, ErrProductNotEnabled
		}
		return Lease{}, err
	}
	if !enabled {
		return Lease{}, ErrProductNotEnabled
	}
	if capCredits != nil {
		var used int64
		// Net the month's metered spend against its refunds. Summing only
		// negative rows left sandbox refunds out, so a refunded interval still
		// counted against the product cap. Grants/admin adjustments stay out:
		// they are not usage, and must not buy cap headroom.
		if err := tx.QueryRow(ctx, `
			select coalesce(-sum(delta_credits),0) from billing.ledger
			where org_id=$1 and product=$2
			  and reason in ('usage','relay_usage','sandbox_usage','sandbox_refund')
			  and created_at >= date_trunc('month', now())`, orgID, product).Scan(&used); err != nil {
			return Lease{}, err
		}
		if used+estimated > *capCredits {
			return Lease{}, ErrProductCap
		}
	}
	var balance, reserved int64
	if err := tx.QueryRow(ctx, `select balance_credits,reserved_credits from billing.wallets where org_id=$1 for update`, orgID).Scan(&balance, &reserved); err != nil {
		return Lease{}, err
	}
	// A retry of the same run must not reserve twice; it may only extend the
	// existing reservation's ceiling and its lifetime. Refreshing the expiry is
	// what keeps a long turn's lease alive: without it a turn outliving the TTL
	// lets ExpireLeases free the reservation, and the later settle finds no
	// active lease and loses the charge.
	var existing Lease
	err = tx.QueryRow(ctx, `select id,org_id,product,run_ref,reserved_credits,status,expires_at from billing.usage_leases where org_id=$1 and product=$2 and run_ref=$3 and status='active' for update`, orgID, product, runRef).Scan(&existing.ID, &existing.OrgID, &existing.Product, &existing.RunRef, &existing.ReservedCredits, &existing.Status, &existing.ExpiresAt)
	if err == nil {
		if estimated <= existing.ReservedCredits {
			if _, err := tx.Exec(ctx, `update billing.usage_leases set expires_at=now()+make_interval(secs=>$2) where id=$1`, existing.ID, int(ttl.Seconds())); err != nil {
				return Lease{}, err
			}
			existing.ExpiresAt = time.Now().Add(ttl)
			return existing, tx.Commit(ctx)
		}
		delta := estimated - existing.ReservedCredits
		if balance-reserved < delta {
			return Lease{}, ErrInsufficientCredits
		}
		if _, err := tx.Exec(ctx, `update billing.usage_leases set reserved_credits=$2,expires_at=now()+make_interval(secs=>$3) where id=$1`, existing.ID, estimated, int(ttl.Seconds())); err != nil {
			return Lease{}, err
		}
		if _, err := tx.Exec(ctx, `update billing.wallets set reserved_credits=reserved_credits+$2,version=version+1,updated_at=now() where org_id=$1`, orgID, delta); err != nil {
			return Lease{}, err
		}
		existing.ReservedCredits = estimated
		existing.ExpiresAt = time.Now().Add(ttl)
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, err
	}
	if balance-reserved < estimated {
		return Lease{}, ErrInsufficientCredits
	}
	var l Lease
	err = tx.QueryRow(ctx, `
		insert into billing.usage_leases (org_id,product,run_ref,reserved_credits,expires_at)
		values ($1,$2,$3,$4,now()+make_interval(secs=>$5))
		on conflict (org_id,product,run_ref) do update set
			reserved_credits=excluded.reserved_credits,expires_at=excluded.expires_at
		where billing.usage_leases.status='active'
		returning id,org_id,product,run_ref,reserved_credits,status,expires_at`,
		orgID, product, runRef, estimated, int(ttl.Seconds())).Scan(&l.ID, &l.OrgID, &l.Product, &l.RunRef, &l.ReservedCredits, &l.Status, &l.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The run already settled or was released. A retry is not an error:
			// answer with the row that exists so a client's retry loop stops
			// instead of hammering a 500 every few seconds.
			var settled Lease
			if qerr := tx.QueryRow(ctx, `
				select id,org_id,product,run_ref,reserved_credits,status,expires_at
				from billing.usage_leases
				where org_id=$1 and product=$2 and run_ref=$3`,
				orgID, product, runRef).Scan(&settled.ID, &settled.OrgID, &settled.Product, &settled.RunRef, &settled.ReservedCredits, &settled.Status, &settled.ExpiresAt); qerr != nil {
				return Lease{}, qerr
			}
			return settled, tx.Commit(ctx)
		}
		return Lease{}, err
	}
	if _, err := tx.Exec(ctx, `update billing.wallets set reserved_credits=reserved_credits+$2,version=version+1,updated_at=now() where org_id=$1`, orgID, estimated); err != nil {
		return Lease{}, err
	}
	return l, tx.Commit(ctx)
}

func (s *Store) Settle(ctx context.Context, leaseID string, actual int64) (Lease, error) {
	if actual < 0 {
		return Lease{}, errors.New("actual credits must be >= 0")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback(ctx)
	var l Lease
	if err := tx.QueryRow(ctx, `select id,org_id,product,run_ref,reserved_credits,status,expires_at from billing.usage_leases where id=$1 for update`, leaseID).Scan(&l.ID, &l.OrgID, &l.Product, &l.RunRef, &l.ReservedCredits, &l.Status, &l.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Lease{}, ErrNotFound
		}
		return Lease{}, err
	}
	if l.Status != "active" {
		return l, nil
	}
	var balance, reserved int64
	if err := tx.QueryRow(ctx, `select balance_credits,reserved_credits from billing.wallets where org_id=$1 for update`, l.OrgID).Scan(&balance, &reserved); err != nil {
		return Lease{}, err
	}
	extra := actual - l.ReservedCredits
	if extra > 0 {
		if balance-reserved < extra {
			return Lease{}, ErrInsufficientCredits
		}
	}
	if _, err := tx.Exec(ctx, `update billing.wallets set reserved_credits=greatest(0,reserved_credits-$2),balance_credits=greatest(0,balance_credits-$3),version=version+1,updated_at=now() where org_id=$1`, l.OrgID, l.ReservedCredits, actual); err != nil {
		return Lease{}, err
	}
	if actual > 0 {
		if _, err := tx.Exec(ctx, `insert into billing.ledger (org_id,product,delta_credits,reason,ref_id,idempotency_key) values ($1,$2,$3,'usage',$4,$5) on conflict (idempotency_key) do nothing`, l.OrgID, l.Product, -actual, leaseID, "lease:"+leaseID); err != nil {
			return Lease{}, err
		}
	}
	if _, err := tx.Exec(ctx, `update billing.usage_leases set status='settled',settled_credits=$2 where id=$1`, leaseID, actual); err != nil {
		return Lease{}, err
	}
	l.Status = "settled"
	l.ReservedCredits = 0
	return l, tx.Commit(ctx)
}

func (s *Store) Release(ctx context.Context, leaseID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var orgID string
	var reserved int64
	var status string
	if err := tx.QueryRow(ctx, `select org_id,reserved_credits,status from billing.usage_leases where id=$1 for update`, leaseID).Scan(&orgID, &reserved, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if status != "active" {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `update billing.wallets set reserved_credits=greatest(0,reserved_credits-$2),version=version+1,updated_at=now() where org_id=$1`, orgID, reserved); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update billing.usage_leases set status='released' where id=$1`, leaseID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ExpireLeases releases active leases whose deadline passed. It runs on a
// timer so a crashed product cannot leave a user's credits frozen forever.
func (s *Store) ExpireLeases(ctx context.Context) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		select id, org_id, reserved_credits from billing.usage_leases
		where status='active' and expires_at < now()
		order by expires_at limit 100
		for update skip locked`)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id       string
		orgID    string
		reserved int64
	}
	var list []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.orgID, &e.reserved); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, e := range list {
		if _, err := tx.Exec(ctx, `update billing.wallets set reserved_credits=greatest(0,reserved_credits-$2),version=version+1,updated_at=now() where org_id=$1`, e.orgID, e.reserved); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `update billing.usage_leases set status='expired' where id=$1`, e.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(list), nil
}

// SettleByRun settles the run's lease with the legacy whole-credit figure. It
// delegates to SettleByRunV2, which also allows an expired lease to settle and
// books an unaffordable remainder as a debt instead of silently clamping.
func (s *Store) SettleByRun(ctx context.Context, orgID, product, runRef string, actual int64) (bool, error) {
	result, err := s.SettleByRunV2(ctx, SettleRunInput{
		OrgID: orgID, Product: product, RunRef: runRef, ActualCredits: &actual,
	})
	if err != nil {
		return false, err
	}
	return result.Settled, nil
}

// ReleaseByRun releases an active lease without charging it.
func (s *Store) ReleaseByRun(ctx context.Context, orgID, product, runRef string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id string
	var reserved int64
	if err := tx.QueryRow(ctx, `select id,reserved_credits from billing.usage_leases where org_id=$1 and product=$2 and run_ref=$3 and status='active' for update`, orgID, product, runRef).Scan(&id, &reserved); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := tx.Exec(ctx, `update billing.wallets set reserved_credits=greatest(0,reserved_credits-$2),version=version+1,updated_at=now() where org_id=$1`, orgID, reserved); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `update billing.usage_leases set status='released' where id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Charge directly debits usage without a reservation. It is used by a product
// that meters after the fact (market lease periods), where a per-period lease
// would be more machinery than value. ref is the idempotency key.
func (s *Store) Charge(ctx context.Context, orgID, product, ref string, credits int64) (bool, error) {
	if credits < 0 {
		return false, errors.New("credits must be >= 0")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, "wallet:"+orgID); err != nil {
		return false, err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `select balance_credits from billing.wallets where org_id=$1 for update`, orgID).Scan(&balance); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	var exists int
	if err := tx.QueryRow(ctx, `select count(*) from billing.ledger where idempotency_key=$1`, "charge:"+ref).Scan(&exists); err != nil {
		return false, err
	}
	if exists > 0 {
		return true, tx.Commit(ctx)
	}
	if balance < credits {
		return false, ErrInsufficientCredits
	}
	if credits > 0 {
		if _, err := tx.Exec(ctx, `update billing.wallets set balance_credits=balance_credits-$2,version=version+1,updated_at=now() where org_id=$1`, orgID, credits); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `insert into billing.ledger(org_id,product,delta_credits,reason,ref_id,idempotency_key) values ($1,$2,$3,'usage',$4,$5)`, orgID, product, -credits, ref, "charge:"+ref); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// Draw consumes part of an active reservation without closing it. Market
// leases reserve one hour of capacity at open and draw each settled window;
// the remaining reservation is released when the lease closes.
func (s *Store) DrawByRun(ctx context.Context, orgID, product, runRef, ref string, credits int64) (bool, error) {
	if credits < 0 {
		return false, errors.New("credits must be >= 0")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, "wallet:"+orgID); err != nil {
		return false, err
	}
	var leaseID string
	var reserved int64
	if err := tx.QueryRow(ctx, `select id,reserved_credits from billing.usage_leases where org_id=$1 and product=$2 and run_ref=$3 and status='active' for update`, orgID, product, runRef).Scan(&leaseID, &reserved); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `select balance_credits from billing.wallets where org_id=$1 for update`, orgID).Scan(&balance); err != nil {
		return false, err
	}
	if credits > reserved || credits > balance {
		return false, ErrInsufficientCredits
	}
	if _, err := tx.Exec(ctx, `update billing.wallets set balance_credits=balance_credits-$2,reserved_credits=reserved_credits-$3,version=version+1,updated_at=now() where org_id=$1`, orgID, credits, credits); err != nil {
		return false, err
	}
	if credits > 0 {
		if _, err := tx.Exec(ctx, `insert into billing.ledger(org_id,product,delta_credits,reason,ref_id,idempotency_key) values ($1,$2,$3,'usage',$4,$5) on conflict (idempotency_key) do nothing`, orgID, product, -credits, ref, "draw:"+ref); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `update billing.usage_leases set reserved_credits=reserved_credits-$2 where id=$1`, leaseID, credits); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// SettleRunInput is a product-side turn settlement (dslpha's release). Exactly
// one amount source is used, in priority order: per-model usage priced from the
// price book, `spent_usd × fx`, then the legacy whole-credit figure.
type SettleRunInput struct {
	OrgID         string
	Product       string
	RunRef        string
	ActualCredits *int64
	ModelUsage    []ModelUsage
	SpentUSD      *float64
	Byok          bool
	// SurchargeMinor is an optional product-reported infrastructure/tool fee
	// in minor units. It is added after the token amount is priced and
	// persisted as a tool charge (tool="surcharge", unit="credit") for audit.
	SurchargeMinor int64
}

// SettleRunResult reports what a turn settlement booked.
type SettleRunResult struct {
	Settled         bool
	ChargedCredits  int64
	AmountMinor     int64
	PriceVersion    string
	SettingsVersion string
	Fallback        bool
	Debts           []Debt
}

func creditsFromSpentUSD(spent float64, settings Settings) int64 {
	if spent <= 0 || math.IsNaN(spent) || math.IsInf(spent, 0) {
		return 0
	}
	credits := int64(math.Ceil(spent * float64(settings.FXUSDToCredits)))
	if credits < 1 {
		credits = 1
	}
	return credits
}

// SettleByRunV2 settles the run's lease with V2 semantics: model_usage is
// priced from the price book, spent_usd is the USD fallback, byok records usage
// without charging, an expired lease still settles, and a charge the wallet
// cannot cover becomes a partial debit plus a debt.
func (s *Store) SettleByRunV2(ctx context.Context, in SettleRunInput) (SettleRunResult, error) {
	if in.OrgID == "" || in.Product == "" || in.RunRef == "" {
		return SettleRunResult{}, fmt.Errorf("%w: org_id, product and run_ref are required", ErrInvalidPricing)
	}
	if in.ActualCredits != nil && *in.ActualCredits < 0 {
		return SettleRunResult{}, fmt.Errorf("%w: actual_credits must be >= 0", ErrInvalidPricing)
	}
	if in.SpentUSD != nil && (*in.SpentUSD < 0 || math.IsNaN(*in.SpentUSD) || math.IsInf(*in.SpentUSD, 0)) {
		return SettleRunResult{}, fmt.Errorf("%w: spent_usd must be >= 0", ErrInvalidPricing)
	}
	if in.SurchargeMinor < 0 {
		return SettleRunResult{}, fmt.Errorf("%w: surcharge_minor must be >= 0", ErrInvalidPricing)
	}
	usages, err := NormalizeModelUsage(in.ModelUsage)
	if err != nil {
		return SettleRunResult{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return SettleRunResult{}, err
	}
	defer tx.Rollback(ctx)
	var (
		leaseID, status                         string
		reserved                                int64
		leasePriceVersion, leaseSettingsVersion string
		leaseCreatedAt                          time.Time
	)
	if err := tx.QueryRow(ctx, `
		select id,status,reserved_credits,coalesce(price_version,''),coalesce(settings_version,''),created_at
		from billing.usage_leases
		where org_id=$1 and product=$2 and run_ref=$3 and status in ('active','expired')
		for update`, in.OrgID, in.Product, in.RunRef).
		Scan(&leaseID, &status, &reserved, &leasePriceVersion, &leaseSettingsVersion, &leaseCreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SettleRunResult{Settled: false}, nil
		}
		return SettleRunResult{}, err
	}
	// An expired lease's hold was already released by ExpireLeases.
	if status == "expired" {
		reserved = 0
	}
	settings, settingsVersion, err := loadSettlementSettings(ctx, tx, leaseSettingsVersion)
	if err != nil {
		return SettleRunResult{}, err
	}

	// dslpha's release carries no started_at. Price the turn at reservation
	// time (the closest known start) so a turn that began inside a peak window
	// is not silently repriced at the moment the release arrives.
	pricedAt := leaseCreatedAt
	if pricedAt.IsZero() {
		pricedAt = time.Now()
	}
	priceVersion := leasePriceVersion
	var (
		book       PriceBook
		bookLoaded bool
		bookErr    error
	)
	if len(usages) > 0 {
		if priceVersion == "" {
			priceVersion, bookErr = currentPriceVersion(ctx, tx, pricedAt)
		}
		if bookErr == nil {
			book, bookErr = loadPriceBook(ctx, tx, time.Time{}, priceVersion)
			bookLoaded = bookErr == nil
		}
	}

	var amountMinor, credits int64
	fallback := false
	fallbackToUSD := func() {
		fallback = true
		if in.SpentUSD == nil {
			return
		}
		credits = creditsFromSpentUSD(*in.SpentUSD, settings)
		amountMinor = credits * MinorPerCredit
	}
	unknownModel := ""
	switch {
	case in.Byok:
		// BYOK records model usage without charging it; an explicit infra/tool
		// surcharge is still billed because it is not model spend.
	case len(usages) > 0 && bookLoaded:
		var total int64
		for _, usage := range usages {
			charge, priceErr := book.PriceCharge(usage.Model, TokenUsage{
				Input: usage.Input, Output: usage.Output,
				CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite,
			}, pricedAt, nil, settings, nil)
			if priceErr != nil {
				if errors.Is(priceErr, ErrUnknownModel) || errors.Is(priceErr, ErrPriceTierNotFound) {
					unknownModel = usage.Model
					break
				}
				return SettleRunResult{}, priceErr
			}
			total += charge.AmountMinor
			priceVersion = charge.PriceVersion
		}
		if unknownModel == "" {
			amountMinor = total
			credits = CreditsFromMinor(total)
			break
		}
		if in.SpentUSD != nil && *in.SpentUSD > 0 {
			fallbackToUSD()
			break
		}
		return SettleRunResult{}, fmt.Errorf("%w: %s", ErrUnknownModel, unknownModel)
	case len(usages) > 0:
		// No usable price book. Fall back to the USD figure when the product
		// supplied one, otherwise the model cannot be priced.
		if in.SpentUSD != nil && *in.SpentUSD > 0 {
			fallbackToUSD()
			break
		}
		return SettleRunResult{}, fmt.Errorf("%w: %s", ErrUnknownModel, usages[0].Model)
	case in.SpentUSD != nil && *in.SpentUSD > 0:
		fallbackToUSD()
	case in.ActualCredits != nil:
		credits = *in.ActualCredits
		amountMinor = credits * MinorPerCredit
	}
	// The surcharge is added to the priced minor amount before rounding to
	// credits, so charged_credits includes it.
	if in.SurchargeMinor > 0 {
		amountMinor += in.SurchargeMinor
		credits = CreditsFromMinor(amountMinor)
	}

	var balance, walletReserved int64
	if err := tx.QueryRow(ctx, `
		select balance_credits,reserved_credits from billing.wallets where org_id=$1 for update`, in.OrgID).
		Scan(&balance, &walletReserved); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SettleRunResult{}, ErrNotFound
		}
		return SettleRunResult{}, err
	}
	available := balance - walletReserved
	maxCharge := reserved + available
	charged := credits
	if charged > maxCharge {
		charged = maxCharge
	}
	if charged < 0 {
		charged = 0
	}
	var debt *Debt
	if credits > charged {
		debt, err = insertDebt(ctx, tx, in.OrgID, in.Product, in.RunRef, credits-charged, DebtReasonInsufficientBalance)
		if err != nil {
			return SettleRunResult{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		update billing.wallets
		set balance_credits=greatest(0,balance_credits-$2),
		    reserved_credits=greatest(0,reserved_credits-$3),
		    version=version+1, updated_at=now()
		where org_id=$1`, in.OrgID, charged, reserved); err != nil {
		return SettleRunResult{}, err
	}
	if charged > 0 {
		if _, err := tx.Exec(ctx, `
			insert into billing.ledger
				(org_id,product,delta_credits,reason,ref_id,idempotency_key,policy_version,settings_version)
			values ($1,$2,$3,'usage',$4,$5,nullif($6,''),$7)
			on conflict (idempotency_key) do nothing`,
			in.OrgID, in.Product, -charged, leaseID, "lease:"+leaseID, priceVersion, settingsVersion); err != nil {
			return SettleRunResult{}, err
		}
	}
	if in.SurchargeMinor > 0 {
		surcharge := []ToolCharge{{
			Tool:           "surcharge",
			Unit:           "credit",
			Quantity:       1,
			UnitPriceMicro: in.SurchargeMinor * 1000,
			AmountMinor:    in.SurchargeMinor,
			Status:         ToolChargeStatusSucceeded,
		}}
		if err := insertToolCharges(ctx, tx, in.OrgID, in.RunRef, leaseID+":surcharge", leaseID, surcharge); err != nil {
			return SettleRunResult{}, err
		}
	}
	for i, usage := range usages {
		event := &UsageEvent{
			Model:            usage.Model,
			Requests:         usage.Calls,
			InputTokens:      usage.Input,
			OutputTokens:     usage.Output,
			CacheReadTokens:  usage.CacheRead,
			CacheWriteTokens: usage.CacheWrite,
		}
		if bookLoaded {
			if cost := book.EstimatedCostMinor(usage.Model, TokenUsage{
				Input: usage.Input, Output: usage.Output,
				CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite,
			}); cost > 0 {
				event.UpstreamCostMinor = &cost
			}
		}
		settlementID := leaseID + ":model:" + usage.Model
		if i > 0 {
			settlementID = fmt.Sprintf("%s:model:%d:%s", leaseID, i, usage.Model)
		}
		if err := insertUsageEventV2(ctx, tx, in.OrgID, in.RunRef, settlementID, leaseID, event, "", "", "", priceVersion, nil, nil, nil, ""); err != nil {
			return SettleRunResult{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		update billing.usage_leases
		set status='settled',settled_credits=$2,price_version=coalesce(price_version,nullif($3,''))
		where id=$1`, leaseID, credits, priceVersion); err != nil {
		return SettleRunResult{}, err
	}
	return SettleRunResult{
		Settled:         true,
		ChargedCredits:  charged,
		AmountMinor:     amountMinor,
		PriceVersion:    priceVersion,
		SettingsVersion: settingsVersion,
		Fallback:        fallback,
		Debts:           debtsOrEmpty(debt),
	}, tx.Commit(ctx)
}
