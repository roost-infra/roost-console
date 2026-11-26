package billing

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

var (
	ErrInvalidGiftCredits      = errors.New("gift credits must be a positive integer")
	ErrMissingIdempotencyKey   = errors.New("idempotency key is required")
	ErrGiftIdempotencyConflict = errors.New("idempotency key belongs to another organization")
)

// AdminOrg is the operations view of one organization.
type AdminOrg struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	PlanCode      string `json:"plan_code"`
	OwnerUserID   string `json:"owner_user_id"`
	OwnerEmail    string `json:"owner_email"`
	BalanceCredit int64  `json:"balance_credits"`
	Reserved      int64  `json:"reserved_credits"`
	CreatedAt     string `json:"created_at"`
}

const adminOrgSelect = `
	select o.id, o.name, o.slug, o.plan_code, o.owner_user_id,
	       coalesce(u.email,''),
	       coalesce(w.balance_credits,0), coalesce(w.reserved_credits,0), o.created_at::text
	from public.organizations o
	left join auth.users u on u.id = o.owner_user_id
	left join billing.wallets w on w.org_id = o.id`

func scanAdminOrg(row pgx.Row) (AdminOrg, error) {
	var a AdminOrg
	err := row.Scan(&a.ID, &a.Name, &a.Slug, &a.PlanCode, &a.OwnerUserID, &a.OwnerEmail, &a.BalanceCredit, &a.Reserved, &a.CreatedAt)
	return a, err
}

// AdminSearchOrgs finds organizations by id, slug or name fragment.
func (s *Store) AdminSearchOrgs(ctx context.Context, query string) ([]AdminOrg, error) {
	pattern := "%" + query + "%"
	rows, err := s.Pool.Query(ctx, adminOrgSelect+`
		where o.id::text = $1 or o.slug ilike $2 or o.name ilike $2 or u.email ilike $2
		order by o.created_at desc limit 50`, query, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AdminOrg, 0)
	for rows.Next() {
		a, err := scanAdminOrg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AdminOrg loads one organization for the operations view.
func (s *Store) AdminOrg(ctx context.Context, orgID string) (AdminOrg, error) {
	a, err := scanAdminOrg(s.Pool.QueryRow(ctx, adminOrgSelect+` where o.id=$1`, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminOrg{}, ErrNotFound
	}
	return a, err
}

// AdminAdjustCredits books an operator adjustment exactly once. The ledger row
// is the audit record; the wallet is never written without one.
func (s *Store) AdminAdjustCredits(ctx context.Context, orgID string, delta int64, reason, idempotencyKey, operator string) (int64, error) {
	if delta == 0 {
		return 0, errors.New("admin: delta must not be zero")
	}
	if idempotencyKey == "" {
		return 0, errors.New("admin: idempotency key is required")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, "wallet:"+orgID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		insert into billing.ledger (org_id,delta_credits,reason,ref_id,idempotency_key)
		values ($1,$2,'admin_adjust',$3,$4)
		on conflict (idempotency_key) do nothing`, orgID, delta, operator, idempotencyKey); err != nil {
		return 0, err
	}
	var balance int64
	if err := tx.QueryRow(ctx, `
		update billing.wallets set balance_credits=greatest(0,balance_credits+$2),version=version+1,updated_at=now()
		where org_id=$1 returning balance_credits`, orgID, delta).Scan(&balance); err != nil {
		return 0, err
	}
	return balance, tx.Commit(ctx)
}

// AdminGiftCredits is the explicit operator gift path: positive credits only,
// reason='admin_gift' in the ledger, the note in ledger.ref_id, and a matching
// billing.admin_audit row with the operator. The wallet is updated only when the
// ledger row was actually inserted, so replaying the same idempotency key (a
// timeout retry) never doubles the gift.
func (s *Store) AdminGiftCredits(ctx context.Context, orgID string, credits int64, note, idempotencyKey, operator string) (int64, error) {
	if credits <= 0 {
		return 0, ErrInvalidGiftCredits
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return 0, ErrMissingIdempotencyKey
	}
	note = strings.TrimSpace(note)
	if note == "" {
		note = "admin gift"
	}
	if operator == "" {
		operator = "admin"
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from public.organizations where id=$1)`, orgID).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, "wallet:"+orgID); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `
		insert into billing.ledger (org_id,delta_credits,reason,ref_id,idempotency_key)
		values ($1,$2,'admin_gift',$3,$4)
		on conflict (idempotency_key) do nothing`, orgID, credits, note, idempotencyKey)
	if err != nil {
		return 0, err
	}

	var balance int64
	if tag.RowsAffected() == 1 {
		if err := tx.QueryRow(ctx, `
			insert into billing.wallets as w (org_id,balance_credits)
			values ($1,$2)
			on conflict (org_id) do update
				set balance_credits = w.balance_credits + excluded.balance_credits,
				    version = w.version + 1,
				    updated_at = now()
			returning balance_credits`, orgID, credits).Scan(&balance); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			insert into billing.admin_audit (actor,action,target,detail)
			values ($1,'admin_gift',$2,jsonb_build_object(
				'credits',$3::bigint,'note',$4::text,'idempotency_key',$5::text,'balance_credits',$6::bigint))`,
			operator, orgID, credits, note, idempotencyKey, balance); err != nil {
			return 0, err
		}
	} else {
		// The key already exists. Verify it belongs to this organization and to
		// the gift path; reusing a key across organizations (or reusing an
		// adjustment key) must fail loudly instead of silently skipping a gift.
		var existingOrgID, existingReason string
		if err := tx.QueryRow(ctx, `
			select org_id::text, reason from billing.ledger where idempotency_key=$1`, idempotencyKey).
			Scan(&existingOrgID, &existingReason); err != nil {
			return 0, err
		}
		if existingOrgID != orgID || existingReason != "admin_gift" {
			return 0, ErrGiftIdempotencyConflict
		}
	}
	if err := tx.QueryRow(ctx, `
		select coalesce((select balance_credits from billing.wallets where org_id=$1),0)`, orgID).Scan(&balance); err != nil {
		return 0, err
	}
	return balance, tx.Commit(ctx)
}

// AdminLedger lists recent ledger rows for one organization. Relay settlements
// also carry their per-tool breakdown so the admin billing view can show the
// "工具调用" line without a second round trip per row.
func (s *Store) AdminLedger(ctx context.Context, orgID string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		select coalesce(product,''), delta_credits, reason, coalesce(ref_id,''), created_at::text
		from billing.ledger where org_id=$1 order by created_at desc limit $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0)
	var relayLeases []string
	var relayIndexes []int
	for rows.Next() {
		var product, reason, ref, created string
		var delta int64
		if err := rows.Scan(&product, &delta, &reason, &ref, &created); err != nil {
			rows.Close()
			return nil, err
		}
		entry := map[string]any{"product": product, "delta_credits": delta, "reason": reason, "ref_id": ref, "created_at": created}
		if reason == "relay_usage" && ref != "" {
			relayLeases = append(relayLeases, ref)
			relayIndexes = append(relayIndexes, len(out))
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(relayLeases) == 0 {
		return out, nil
	}
	byLease, err := s.toolChargesByLeaseIDs(ctx, relayLeases)
	if err != nil {
		return nil, err
	}
	for i, leaseID := range relayLeases {
		charges := byLease[leaseID]
		if len(charges) == 0 {
			continue
		}
		totals := SumToolCharges(charges)
		entry := out[relayIndexes[i]]
		entry["tool_charges"] = charges
		entry["tool_charges_total"] = totals
		entry["tool_charges_amount_minor"] = totals.AmountMinor
		entry["tool_charges_provider_cost_micro"] = totals.ProviderCostMicro
	}
	return out, nil
}

// toolChargesByLeaseIDs groups persisted tool charges for the ledger rows.
func (s *Store) toolChargesByLeaseIDs(ctx context.Context, leaseIDs []string) (map[string][]ToolCharge, error) {
	rows, err := s.Pool.Query(ctx, `
		select lease_id::text,tool,unit,quantity,unit_price_micro,amount_minor,provider_cost_micro,status
		from billing.usage_tool_charges
		where lease_id::text = any($1)
		order by id`, leaseIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]ToolCharge, len(leaseIDs))
	for rows.Next() {
		var leaseID string
		var c ToolCharge
		if err := rows.Scan(&leaseID, &c.Tool, &c.Unit, &c.Quantity, &c.UnitPriceMicro, &c.AmountMinor, &c.ProviderCostMicro, &c.Status); err != nil {
			return nil, err
		}
		out[leaseID] = append(out[leaseID], c)
	}
	return out, rows.Err()
}
