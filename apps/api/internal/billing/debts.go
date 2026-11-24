package billing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Open debts for usage the wallet could not cover. A settlement that exceeds
// the lease hold plus the free balance debits what is available and records the
// remainder here, keyed by (org, product, run_ref, reason) so a retry cannot
// double-charge a run.
const (
	DebtReasonInsufficientBalance = "insufficient_balance"
	DebtReasonExpiredLease        = "expired_lease"
	DebtReasonReleaseFailed       = "release_failed"

	DebtStatusOpen = "open"
)

// Debt is one open (or resolved) billing debt.
type Debt struct {
	ID            string     `json:"id,omitempty"`
	OrgID         string     `json:"org_id,omitempty"`
	Product       string     `json:"product,omitempty"`
	RunRef        string     `json:"run_ref,omitempty"`
	AmountCredits int64      `json:"amount_credits"`
	Reason        string     `json:"reason"`
	Status        string     `json:"status,omitempty"`
	CreatedAt     time.Time  `json:"created_at,omitempty"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
}

// insertDebt records the uncovered part of a settlement inside the settlement
// transaction. An existing (org, product, run_ref, reason) row wins so a replay
// returns the original debt instead of stacking a second one.
func insertDebt(ctx context.Context, tx pgx.Tx, orgID, product, runRef string, credits int64, reason string) (*Debt, error) {
	if credits <= 0 {
		return nil, nil
	}
	var debt Debt
	err := tx.QueryRow(ctx, `
		insert into billing.debts (org_id, product, run_ref, amount_credits, reason)
		values ($1,$2,$3,$4,$5)
		on conflict (org_id, product, run_ref, reason) do nothing
		returning id::text, amount_credits, status, created_at`,
		orgID, product, runRef, credits, reason).
		Scan(&debt.ID, &debt.AmountCredits, &debt.Status, &debt.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			select id::text, amount_credits, status, created_at from billing.debts
			where org_id=$1 and product=$2 and run_ref=$3 and reason=$4`,
			orgID, product, runRef, reason).
			Scan(&debt.ID, &debt.AmountCredits, &debt.Status, &debt.CreatedAt)
	}
	if err != nil {
		return nil, err
	}
	debt.OrgID = orgID
	debt.Product = product
	debt.RunRef = runRef
	debt.Reason = reason
	return &debt, nil
}

// Debts lists an organization's debts, newest first. The admin/reconciliation
// surfaces read this; settlements only write it.
func (s *Store) Debts(ctx context.Context, orgID string) ([]Debt, error) {
	rows, err := s.Pool.Query(ctx, `
		select id::text, org_id::text, product, run_ref, amount_credits, reason, status, created_at, resolved_at
		from billing.debts
		where org_id=$1
		order by created_at desc, id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Debt, 0)
	for rows.Next() {
		var debt Debt
		if err := rows.Scan(&debt.ID, &debt.OrgID, &debt.Product, &debt.RunRef, &debt.AmountCredits,
			&debt.Reason, &debt.Status, &debt.CreatedAt, &debt.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, debt)
	}
	return out, rows.Err()
}
