package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Relay settle v2: the relay reports usage and the console prices it from the
// price version locked on the reservation. The v1 shapes (amount passed by the
// relay, usage stats, legacy compression) stay available on the old path.
type RelayReserveInput struct {
	OrgID        string
	RequestID    string
	Amount       int64 // hold in credits
	Currency     string
	TTL          time.Duration
	PriceVersion string
}

type RelaySettleInput struct {
	ReservationID  string
	SettlementID   string
	IdempotencyKey string
	Model          string
	UpstreamID     string
	StartedAt      time.Time
	PriceVersion   string
	Usage          *TokenUsage
	Basis          *CompressionBasis
	ToolCharges    []ToolCharge
	// AmountMinor is the transitional relay-computed amount. When present the
	// console recomputes and rejects a mismatch with ErrAmountMismatch without
	// charging.
	AmountMinor *int64
	// Stats is the transitional usage-stats object (requests/model/input_tokens
	// ...). It is persisted for reporting; pricing never reads it.
	Stats *UsageEvent
}

// resolveReserveVersions picks the price and settings versions a reservation
// locks: the requested version when the relay pins one (and it exists), else
// the version currently effective. One query keeps this off the first-token
// latency path.
func resolveReserveVersions(ctx context.Context, q settingsQuerier, requested string) (string, string, error) {
	requested = strings.TrimSpace(requested)
	var (
		current         string
		settingsVersion string
		exists          bool
	)
	err := q.QueryRow(ctx, `
		select
		  coalesce((select version from billing.price_book
		            where effective_from <= now()
		            order by effective_from desc, version desc limit 1), ''),
		  coalesce((select trim(both '"' from value::text) from billing.settings
		            where key='settings.version'), ''),
		  case when $1 = '' then true
		       else exists(select 1 from billing.price_book where version=$1) end`,
		requested).Scan(&current, &settingsVersion, &exists)
	if err != nil {
		return "", "", err
	}
	if requested != "" {
		if !exists {
			return "", "", ErrPriceVersionMismatch
		}
		return requested, settingsVersion, nil
	}
	return current, settingsVersion, nil
}

// RelayReserveWithDetails is the V2 reservation path. The legacy RelayReserve
// keeps its four-argument shape and delegates here with no pinned version.
func (s *Store) RelayReserveWithDetails(ctx context.Context, in RelayReserveInput) (RelayReservation, error) {
	if in.Amount < 0 {
		return RelayReservation{}, errors.New("relay: reserve amount must be >= 0")
	}
	if in.Currency == "" {
		in.Currency = RelayCurrency
	}
	ttl := in.TTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	priceVersion, settingsVersion, err := resolveReserveVersions(ctx, s.Pool, in.PriceVersion)
	if err != nil {
		return RelayReservation{}, err
	}
	var out RelayReservation
	var updated int64
	err = s.Pool.QueryRow(ctx, `
		with upd as (
			update billing.wallets
			set reserved_credits=reserved_credits+$3, version=version+1, updated_at=now()
			where org_id=$1 and balance_credits-reserved_credits >= $3
			returning balance_credits
		), ins as (
			insert into billing.usage_leases
				(org_id,product,run_ref,reserved_credits,status,expires_at,price_version,settings_version)
			select $1,'relay',$2,$3,'active',now()+make_interval(secs=>$4),
			       nullif($5,''), nullif($6,'')
			where exists (select 1 from upd)
			returning id,reserved_credits,expires_at,
			          coalesce(price_version,'') as price_version,
			          coalesce(settings_version,'') as settings_version
		)
		select id,reserved_credits,expires_at,price_version,settings_version,(select count(*) from upd) from ins`,
		in.OrgID, in.RequestID, in.Amount, int(ttl.Seconds()), priceVersion, settingsVersion).
		Scan(&out.ID, &out.Amount, &out.ExpiresAt, &out.PriceVersion, &out.SettingsVersion, &updated)
	if err == nil {
		return out, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		var existing RelayReservation
		var status string
		if qerr := s.Pool.QueryRow(ctx, `
			select id,reserved_credits,status,expires_at,coalesce(price_version,''),coalesce(settings_version,'')
			from billing.usage_leases
			where org_id=$1 and product='relay' and run_ref=$2`, in.OrgID, in.RequestID).
			Scan(&existing.ID, &existing.Amount, &status, &existing.ExpiresAt, &existing.PriceVersion, &existing.SettingsVersion); qerr == nil {
			if existing.Amount != in.Amount || status != "active" {
				return RelayReservation{}, ErrRelayConflict
			}
			return existing, nil
		}
	}
	if errors.Is(err, pgx.ErrNoRows) || updated == 0 {
		return RelayReservation{}, ErrInsufficientCredits
	}
	return RelayReservation{}, err
}

// RelaySettleV2 books a usage-based settlement. It recomputes the amount from
// the price version pinned on the lease, compares the transitional amount when
// one was sent, allows an expired lease to settle, and splits an unaffordable
// charge into a partial debit plus a debt.
func (s *Store) RelaySettleV2(ctx context.Context, in RelaySettleInput) (RelayOperation, error) {
	if strings.TrimSpace(in.Model) == "" {
		return RelayOperation{}, fmt.Errorf("%w: model is required", ErrInvalidPricing)
	}
	usage := TokenUsage{}
	if in.Usage != nil {
		usage = *in.Usage
	}
	if err := usage.Validate(); err != nil {
		return RelayOperation{}, err
	}
	charges, err := NormalizeToolCharges(in.ToolCharges)
	if err != nil {
		return RelayOperation{}, err
	}
	basis, err := NormalizeCompressionBasis(in.Basis)
	if err != nil {
		return RelayOperation{}, err
	}
	stats, err := NormalizeUsage(in.Stats)
	if err != nil {
		return RelayOperation{}, err
	}
	if in.IdempotencyKey == "" {
		return RelayOperation{}, fmt.Errorf("%w: idempotency_key is required", ErrInvalidPricing)
	}
	if in.SettlementID == "" {
		in.SettlementID = in.IdempotencyKey
	}
	startedAt := in.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return RelayOperation{}, err
	}
	defer tx.Rollback(ctx)

	if op, found, err := relaySettleV2Replay(ctx, tx, in.IdempotencyKey); err != nil {
		return RelayOperation{}, err
	} else if found {
		return op, tx.Commit(ctx)
	}

	var (
		orgID, requestID, status                string
		leasePriceVersion, leaseSettingsVersion string
		reserved                                int64
	)
	if err := tx.QueryRow(ctx, `
		select org_id::text,run_ref,reserved_credits,status,
		       coalesce(price_version,''),coalesce(settings_version,'')
		from billing.usage_leases
		where id=$1 and product='relay' for update`, in.ReservationID).
		Scan(&orgID, &requestID, &reserved, &status, &leasePriceVersion, &leaseSettingsVersion); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RelayOperation{}, ErrRelayNotFound
		}
		return RelayOperation{}, err
	}
	switch status {
	case "released":
		return RelayOperation{}, ErrRelayReleased
	case "settled":
		return RelayOperation{}, ErrRelayConflict
	}
	// ExpireLeases already released an expired lease's hold; subtracting it
	// again would corrupt the wallet's reserved total.
	if status == "expired" {
		reserved = 0
	}
	requestPriceVersion := strings.TrimSpace(in.PriceVersion)
	if leasePriceVersion != "" && requestPriceVersion != "" && requestPriceVersion != leasePriceVersion {
		return RelayOperation{}, ErrPriceVersionMismatch
	}
	priceVersion := leasePriceVersion
	if priceVersion == "" {
		priceVersion = requestPriceVersion
	}
	if priceVersion == "" {
		priceVersion, err = currentPriceVersion(ctx, tx, startedAt)
		if err != nil {
			return RelayOperation{}, err
		}
	}
	book, err := loadPriceBook(ctx, tx, time.Time{}, priceVersion)
	if err != nil {
		return RelayOperation{}, err
	}
	settings, settingsVersion, err := loadSettlementSettings(ctx, tx, leaseSettingsVersion)
	if err != nil {
		return RelayOperation{}, err
	}
	charge, err := book.PriceCharge(in.Model, usage, startedAt, basis, settings, charges)
	if err != nil {
		return RelayOperation{}, err
	}
	if in.AmountMinor != nil && *in.AmountMinor != charge.AmountMinor {
		return RelayOperation{}, fmt.Errorf("%w: relay=%d console=%d", ErrAmountMismatch, *in.AmountMinor, charge.AmountMinor)
	}

	var balance, walletReserved int64
	if err := tx.QueryRow(ctx, `
		select balance_credits,reserved_credits from billing.wallets where org_id=$1 for update`, orgID).
		Scan(&balance, &walletReserved); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RelayOperation{}, ErrNotFound
		}
		return RelayOperation{}, err
	}
	// The most this settlement may debit without stealing other leases' holds:
	// this lease's own reservation plus the wallet's free balance.
	available := balance - walletReserved
	maxCharge := reserved + available
	due := charge.AmountCredits
	charged := due
	if charged > maxCharge {
		charged = maxCharge
	}
	if charged < 0 {
		charged = 0
	}
	var debt *Debt
	if due > charged {
		debt, err = insertDebt(ctx, tx, orgID, RelayProduct, requestID, due-charged, DebtReasonInsufficientBalance)
		if err != nil {
			return RelayOperation{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		update billing.wallets
		set balance_credits=greatest(0,balance_credits-$2),
		    reserved_credits=greatest(0,reserved_credits-$3),
		    version=version+1, updated_at=now()
		where org_id=$1`, orgID, charged, reserved); err != nil {
		return RelayOperation{}, err
	}
	if charged > 0 {
		tag, err := tx.Exec(ctx, `
			insert into billing.ledger
				(org_id,product,delta_credits,reason,ref_id,idempotency_key,policy_version,settings_version)
			values ($1,'relay',$2,'relay_usage',$3,$4,$5,$6)
			on conflict (idempotency_key) do nothing`,
			orgID, -charged, in.ReservationID, in.IdempotencyKey, priceVersion, settingsVersion)
		if err != nil {
			return RelayOperation{}, err
		}
		if tag.RowsAffected() == 0 {
			// A concurrent settle won after our replay read; roll back and
			// return the winner's booking.
			_ = tx.Rollback(ctx)
			return s.RelayOperation(ctx, in.IdempotencyKey)
		}
	}
	pricedCharges := PriceToolCharges(settings, charges)
	if err := insertToolCharges(ctx, tx, orgID, requestID, in.SettlementID, in.ReservationID, pricedCharges); err != nil {
		return RelayOperation{}, err
	}
	if compression := charge.Compression; compression != nil {
		if err := insertCompressionV2(ctx, tx, orgID, requestID, in.SettlementID, in.ReservationID, compression); err != nil {
			return RelayOperation{}, err
		}
	}
	event := stats
	if event == nil && !usage.IsZero() {
		event = &UsageEvent{
			Model:            in.Model,
			Requests:         1,
			InputTokens:      usage.Input,
			OutputTokens:     usage.Output,
			CacheReadTokens:  usage.CacheRead,
			CacheWriteTokens: usage.CacheWrite,
		}
	}
	if event != nil {
		if event.Model == "" {
			event.Model = in.Model
		}
		if event.UpstreamCostMinor == nil {
			if cost := book.EstimatedCostMinor(in.Model, usage); cost > 0 {
				event.UpstreamCostMinor = &cost
			}
		}
		if err := insertUsageEventV2(ctx, tx, orgID, requestID, in.SettlementID, in.ReservationID, event, in.UpstreamID, priceVersion); err != nil {
			return RelayOperation{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		update billing.usage_leases set status='settled',settled_credits=$2,upstream_id=nullif($3,'') where id=$1`,
		in.ReservationID, due, in.UpstreamID); err != nil {
		return RelayOperation{}, err
	}
	op := RelayOperation{
		Found:           true,
		Operation:       "settle",
		Status:          "settled",
		ReservationID:   in.ReservationID,
		Amount:          due,
		AmountMinor:     charge.AmountMinor,
		PolicyVersion:   priceVersion,
		SettingsVersion: settingsVersion,
		UpstreamID:      in.UpstreamID,
		ToolCharges:     pricedCharges,
		Compression:     nil,
		Pricing:         &charge,
		Usage:           event,
		Debts:           debtsOrEmpty(debt),
	}
	return op, tx.Commit(ctx)
}

func debtsOrEmpty(debt *Debt) []Debt {
	if debt == nil {
		return []Debt{}
	}
	return []Debt{*debt}
}

// relaySettleV2Replay returns a previously booked settlement for the same
// idempotency key. The ledger row is authoritative for the credits charged;
// the persisted breakdown rows rebuild the reporting detail.
func relaySettleV2Replay(ctx context.Context, tx pgx.Tx, idempotencyKey string) (RelayOperation, bool, error) {
	var (
		policyVersion, settingsVersion string
		refID                          string
		booked                         int64
	)
	err := tx.QueryRow(ctx, `
		select delta_credits, coalesce(ref_id,''), coalesce(policy_version,''), coalesce(settings_version,'')
		from billing.ledger where idempotency_key=$1`, idempotencyKey).
		Scan(&booked, &refID, &policyVersion, &settingsVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return RelayOperation{}, false, nil
	}
	if err != nil {
		return RelayOperation{}, false, err
	}
	op := RelayOperation{
		Found:           true,
		Operation:       "settle",
		Status:          "settled",
		ReservationID:   refID,
		Amount:          -booked,
		AmountMinor:     -booked * MinorPerCredit,
		PolicyVersion:   policyVersion,
		SettingsVersion: settingsVersion,
		Debts:           []Debt{},
	}
	if refID == "" {
		return op, true, nil
	}
	op.ToolCharges, err = queryToolCharges(ctx, tx, refID)
	if err != nil {
		return RelayOperation{}, false, err
	}
	op.Compression, err = queryCompression(ctx, tx, refID)
	if err != nil {
		return RelayOperation{}, false, err
	}
	op.Usage, err = queryUsageEvent(ctx, tx, refID)
	if err != nil {
		return RelayOperation{}, false, err
	}
	compression, err := queryCompressionCharge(ctx, tx, refID)
	if err != nil {
		return RelayOperation{}, false, err
	}
	if compression != nil {
		op.Pricing = &Charge{Compression: compression}
	}
	var orgID, runRef string
	if err := tx.QueryRow(ctx, `
		select org_id::text,run_ref from billing.usage_leases where id=$1`, refID).Scan(&orgID, &runRef); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return RelayOperation{}, false, err
		}
		return op, true, nil
	}
	debts, err := queryDebts(ctx, tx, orgID, RelayProduct, runRef)
	if err != nil {
		return RelayOperation{}, false, err
	}
	op.Debts = debts
	return op, true, nil
}

func queryDebts(ctx context.Context, q settingsQuerier, orgID, product, runRef string) ([]Debt, error) {
	rows, err := q.Query(ctx, `
		select id::text, amount_credits, reason, status, created_at, resolved_at
		from billing.debts
		where org_id=$1 and product=$2 and run_ref=$3
		order by created_at, id`, orgID, product, runRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Debt, 0)
	for rows.Next() {
		var debt Debt
		if err := rows.Scan(&debt.ID, &debt.AmountCredits, &debt.Reason, &debt.Status, &debt.CreatedAt, &debt.ResolvedAt); err != nil {
			return nil, err
		}
		debt.OrgID = orgID
		debt.Product = product
		debt.RunRef = runRef
		out = append(out, debt)
	}
	return out, rows.Err()
}
