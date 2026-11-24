package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Relay input-compression reporting. The relay sends one optional object per
// settled request; the console stores it as a reporting row next to the ledger
// entry. A settlement with no compression (or an all-zero object) stores
// nothing, which keeps pre-compression clients and no-op runs out of the table.
const (
	CompressionDefaultLimit = 50
	CompressionMaxLimit     = 200
)

// ErrInvalidCompression is returned for a malformed compression object. The
// HTTP layer maps it to 400 before a settlement is booked.
var ErrInvalidCompression = errors.New("invalid compression")

// Compression is the relay's input-compression savings report. All token and
// minor-unit fields are non-negative; minor units are the relay's 1/1000 credit
// unit (the same unit as amount_minor elsewhere in the settlement contract).
type Compression struct {
	Applied               bool  `json:"applied"`
	OriginalInputTokens   int64 `json:"original_input_tokens"`
	CompressedInputTokens int64 `json:"compressed_input_tokens"`
	SavedTokens           int64 `json:"saved_tokens"`
	UserShareTokens       int64 `json:"user_share_tokens"`
	UserShareMinor        int64 `json:"user_share_minor"`
	SummaryCostMinor      int64 `json:"summary_cost_minor"`
}

// IsZero reports whether the object carries no savings at all. Such an object
// (including applied=false with zero counters) is not persisted.
func (c Compression) IsZero() bool {
	return c.OriginalInputTokens == 0 &&
		c.CompressedInputTokens == 0 &&
		c.SavedTokens == 0 &&
		c.UserShareTokens == 0 &&
		c.UserShareMinor == 0 &&
		c.SummaryCostMinor == 0
}

// NormalizeCompression validates the optional compression object. It returns
// nil for an absent object or one whose counters are all zero, meaning "do not
// store a row". The applied flag is decoded by encoding/json, so a non-boolean
// value is rejected as a malformed request before this point.
func NormalizeCompression(c *Compression) (*Compression, error) {
	if c == nil {
		return nil, nil
	}
	fields := []struct {
		name  string
		value int64
	}{
		{"original_input_tokens", c.OriginalInputTokens},
		{"compressed_input_tokens", c.CompressedInputTokens},
		{"saved_tokens", c.SavedTokens},
		{"user_share_tokens", c.UserShareTokens},
		{"user_share_minor", c.UserShareMinor},
		{"summary_cost_minor", c.SummaryCostMinor},
	}
	for _, f := range fields {
		if f.value < 0 {
			return nil, fmt.Errorf("%w: %s must be >= 0", ErrInvalidCompression, f.name)
		}
	}
	if c.IsZero() {
		return nil, nil
	}
	out := *c
	return &out, nil
}

// CompressionRow is one persisted settlement's savings report.
type CompressionRow struct {
	ID                    int64     `json:"id"`
	OrgID                 string    `json:"org_id"`
	RequestID             string    `json:"request_id"`
	SettlementID          string    `json:"settlement_id"`
	LeaseID               string    `json:"lease_id"`
	Applied               bool      `json:"applied"`
	OriginalInputTokens   int64     `json:"original_input_tokens"`
	CompressedInputTokens int64     `json:"compressed_input_tokens"`
	SavedTokens           int64     `json:"saved_tokens"`
	UserShareTokens       int64     `json:"user_share_tokens"`
	UserShareMinor        int64     `json:"user_share_minor"`
	SummaryCostMinor      int64     `json:"summary_cost_minor"`
	CreatedAt             time.Time `json:"created_at"`
}

// CompressionTotals aggregates every compression row of one organization. It
// intentionally covers all rows, not just the limited page of recent rows.
type CompressionTotals struct {
	Count            int64 `json:"count"`
	SavedTokens      int64 `json:"saved_tokens"`
	UserShareTokens  int64 `json:"user_share_tokens"`
	UserShareMinor   int64 `json:"user_share_minor"`
	SummaryCostMinor int64 `json:"summary_cost_minor"`
}

// CompressionUsage is the billing page's compression report: newest rows plus
// the organization's all-time totals.
type CompressionUsage struct {
	Rows   []CompressionRow  `json:"rows"`
	Totals CompressionTotals `json:"totals"`
	Limit  int               `json:"limit"`
}

type compressionQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CompressionUsage reads one organization's compression rows, newest first.
// The query is always scoped by org_id, so another organization's rows can
// never leak into the response.
func (s *Store) CompressionUsage(ctx context.Context, orgID string, limit int) (CompressionUsage, error) {
	if limit <= 0 {
		limit = CompressionDefaultLimit
	}
	if limit > CompressionMaxLimit {
		limit = CompressionMaxLimit
	}
	rows, err := s.Pool.Query(ctx, `
		select id,org_id,request_id,settlement_id,lease_id,applied,
		       original_input_tokens,compressed_input_tokens,saved_tokens,
		       user_share_tokens,user_share_minor,summary_cost_minor,created_at
		from billing.usage_compression
		where org_id=$1
		order by created_at desc, id desc
		limit $2`, orgID, limit)
	if err != nil {
		return CompressionUsage{}, err
	}
	out := CompressionUsage{Rows: make([]CompressionRow, 0), Limit: limit}
	for rows.Next() {
		var row CompressionRow
		if err := rows.Scan(&row.ID, &row.OrgID, &row.RequestID, &row.SettlementID, &row.LeaseID,
			&row.Applied, &row.OriginalInputTokens, &row.CompressedInputTokens, &row.SavedTokens,
			&row.UserShareTokens, &row.UserShareMinor, &row.SummaryCostMinor, &row.CreatedAt); err != nil {
			rows.Close()
			return CompressionUsage{}, err
		}
		out.Rows = append(out.Rows, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CompressionUsage{}, err
	}
	rows.Close()

	if err := s.Pool.QueryRow(ctx, `
		select count(*),coalesce(sum(saved_tokens),0),coalesce(sum(user_share_tokens),0),
		       coalesce(sum(user_share_minor),0),coalesce(sum(summary_cost_minor),0)
		from billing.usage_compression
		where org_id=$1`, orgID).
		Scan(&out.Totals.Count, &out.Totals.SavedTokens, &out.Totals.UserShareTokens,
			&out.Totals.UserShareMinor, &out.Totals.SummaryCostMinor); err != nil {
		return CompressionUsage{}, err
	}
	return out, nil
}

// queryCompression loads the compression row of a settled lease. A lease
// settles at most once, so this is also the idempotent replay read path.
func queryCompression(ctx context.Context, q compressionQuerier, leaseID string) (*Compression, error) {
	var c Compression
	err := q.QueryRow(ctx, `
		select applied,original_input_tokens,compressed_input_tokens,saved_tokens,
		       user_share_tokens,user_share_minor,summary_cost_minor
		from billing.usage_compression
		where lease_id=$1
		order by id limit 1`, leaseID).
		Scan(&c.Applied, &c.OriginalInputTokens, &c.CompressedInputTokens, &c.SavedTokens,
			&c.UserShareTokens, &c.UserShareMinor, &c.SummaryCostMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// insertCompression writes the savings row inside the settlement transaction.
// It is keyed by settlement_id, so a retry that reaches the insert again is a
// no-op instead of a duplicate.
func insertCompression(ctx context.Context, tx pgx.Tx, orgID, requestID, settlementID, leaseID string, c *Compression) error {
	if c == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `
		insert into billing.usage_compression
			(org_id,request_id,settlement_id,lease_id,applied,original_input_tokens,
			 compressed_input_tokens,saved_tokens,user_share_tokens,user_share_minor,
			 summary_cost_minor)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		on conflict (settlement_id) do nothing`,
		orgID, requestID, settlementID, leaseID, c.Applied, c.OriginalInputTokens,
		c.CompressedInputTokens, c.SavedTokens, c.UserShareTokens, c.UserShareMinor,
		c.SummaryCostMinor)
	return err
}

// insertCompressionV2 persists the counterfactual compression report in the
// columns added by 0015: what was charged, what the user's share was, the
// retail/cost used and the net gain versus not compressing at all.
func insertCompressionV2(ctx context.Context, tx pgx.Tx, orgID, requestID, settlementID, leaseID string, c *CompressionCharge) error {
	if c == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `
		insert into billing.usage_compression
			(org_id,request_id,settlement_id,lease_id,applied,original_input_tokens,
			 compressed_input_tokens,saved_tokens,user_share_tokens,user_share_minor,
			 summary_cost_minor,charge_input_tokens,upstream_input_tokens,user_share_bps,
			 retail_minor_per_million,cost_minor_per_million,cache_loss_minor,
			 no_compression_margin_minor,compressed_margin_minor,net_gain_minor)
		values ($1,$2,$3,$4,true,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		on conflict (settlement_id) do nothing`,
		orgID, requestID, settlementID, leaseID, c.BeforeInputTokens, c.AfterInputTokens,
		c.SavedTokens, c.UserShareTokens, c.UserShareMinor, c.SummaryCostMinor,
		c.ChargeInputTokens, c.AfterInputTokens, c.UserShareBPS,
		c.RetailInputMinorPerMillion, c.CostInputMinorPerMillion, c.CacheLossMinor,
		c.NoCompressionMarginMinor, c.CompressedMarginMinor, c.NetGainMinor)
	return err
}

// queryCompressionCharge reloads the V2 counterfactual report of a settled
// lease for the idempotent replay path.
func queryCompressionCharge(ctx context.Context, q compressionQuerier, leaseID string) (*CompressionCharge, error) {
	var (
		c                                                         CompressionCharge
		chargeInput, upstreamInput, shareBPS                      *int64
		retail, cost, summary, cacheLoss, noComp, compMargin, net *int64
		saved, userShare, userShareMinor                          int64
	)
	err := q.QueryRow(ctx, `
		select coalesce(original_input_tokens,0),coalesce(compressed_input_tokens,0),
		       coalesce(saved_tokens,0),coalesce(user_share_tokens,0),coalesce(user_share_minor,0),
		       charge_input_tokens,upstream_input_tokens,user_share_bps,
		       retail_minor_per_million,cost_minor_per_million,coalesce(summary_cost_minor,0),
		       cache_loss_minor,no_compression_margin_minor,compressed_margin_minor,net_gain_minor
		from billing.usage_compression
		where lease_id=$1
		order by id limit 1`, leaseID).
		Scan(&c.BeforeInputTokens, &c.AfterInputTokens, &saved, &userShare, &userShareMinor,
			&chargeInput, &upstreamInput, &shareBPS, &retail, &cost, &summary,
			&cacheLoss, &noComp, &compMargin, &net)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Applied = true
	c.SavedTokens = saved
	c.UserShareTokens = userShare
	c.UserShareMinor = userShareMinor
	if chargeInput != nil {
		c.ChargeInputTokens = *chargeInput
	}
	if upstreamInput != nil {
		c.AfterInputTokens = *upstreamInput
	}
	if shareBPS != nil {
		c.UserShareBPS = *shareBPS
	}
	if retail != nil {
		c.RetailInputMinorPerMillion = *retail
	}
	if cost != nil {
		c.CostInputMinorPerMillion = *cost
	}
	if summary != nil {
		c.SummaryCostMinor = *summary
	}
	if cacheLoss != nil {
		c.CacheLossMinor = *cacheLoss
	}
	if noComp != nil {
		c.NoCompressionMarginMinor = *noComp
	}
	if compMargin != nil {
		c.CompressedMarginMinor = *compMargin
	}
	if net != nil {
		c.NetGainMinor = *net
	}
	return &c, nil
}
