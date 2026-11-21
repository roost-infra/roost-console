package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Relay usage reporting. The relay sends one optional object per settled
// request with the model and token/cost counters it observed upstream. The
// console stores it as a reporting row next to the ledger entry. A settlement
// with no usage (or an all-zero object) stores nothing, which keeps older relay
// clients and empty runs out of the table.
const (
	UsageEventMaxModelLen   = 200
	UsageEventMaxSessionLen = 200
)

// ErrInvalidUsage is returned for a malformed usage object. The HTTP layer maps
// it to 400 before a settlement is booked.
var ErrInvalidUsage = errors.New("invalid usage")

// UsageEvent is the relay's usage report for one settled request. All numeric
// fields are non-negative. UpstreamCostMinor is optional: nil stays null in
// Postgres so "not reported" is distinguishable from a real zero.
type UsageEvent struct {
	Model string `json:"model"`
	// SessionID is the client's explicit session header. Optional: empty means
	// the request was not session-scoped and per-session reporting skips it.
	SessionID         string `json:"session_id,omitempty"`
	Requests          int64  `json:"requests"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CacheReadTokens   int64  `json:"cache_read_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	UpstreamCostMinor *int64 `json:"upstream_cost_minor"`
}

// IsZero reports whether the object carries no usage at all. Such an object
// (absent, null, or all-zero counters) is not persisted.
func (u UsageEvent) IsZero() bool {
	return u.Requests == 0 &&
		u.InputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 &&
		u.CacheWriteTokens == 0 &&
		(u.UpstreamCostMinor == nil || *u.UpstreamCostMinor == 0)
}

// NormalizeUsage validates the optional usage object. It returns nil for an
// absent object or one whose counters are all zero, meaning "do not store a
// row". The model is trimmed and capped at UsageEventMaxModelLen characters. A
// usage object with tokens but no explicit request count defaults to one,
// matching the table's default and the relay's one-object-per-request shape.
func NormalizeUsage(u *UsageEvent) (*UsageEvent, error) {
	if u == nil {
		return nil, nil
	}
	fields := []struct {
		name  string
		value int64
	}{
		{"requests", u.Requests},
		{"input_tokens", u.InputTokens},
		{"output_tokens", u.OutputTokens},
		{"cache_read_tokens", u.CacheReadTokens},
		{"cache_write_tokens", u.CacheWriteTokens},
	}
	for _, f := range fields {
		if f.value < 0 {
			return nil, fmt.Errorf("%w: %s must be >= 0", ErrInvalidUsage, f.name)
		}
	}
	if u.UpstreamCostMinor != nil && *u.UpstreamCostMinor < 0 {
		return nil, fmt.Errorf("%w: upstream_cost_minor must be >= 0", ErrInvalidUsage)
	}
	out := *u
	out.Model = strings.TrimSpace(u.Model)
	if utf8.RuneCountInString(out.Model) > UsageEventMaxModelLen {
		return nil, fmt.Errorf("%w: model must be at most %d characters", ErrInvalidUsage, UsageEventMaxModelLen)
	}
	out.SessionID = strings.TrimSpace(u.SessionID)
	if utf8.RuneCountInString(out.SessionID) > UsageEventMaxSessionLen {
		return nil, fmt.Errorf("%w: session_id must be at most %d characters", ErrInvalidUsage, UsageEventMaxSessionLen)
	}
	if out.IsZero() {
		return nil, nil
	}
	if out.Requests == 0 {
		out.Requests = 1
	}
	return &out, nil
}

// UsageEventRow is one persisted usage report.
type UsageEventRow struct {
	ID                int64     `json:"id"`
	OrgID             string    `json:"org_id"`
	RequestID         string    `json:"request_id"`
	SettlementID      string    `json:"settlement_id"`
	LeaseID           string    `json:"lease_id"`
	Model             string    `json:"model"`
	Requests          int64     `json:"requests"`
	InputTokens       int64     `json:"input_tokens"`
	OutputTokens      int64     `json:"output_tokens"`
	CacheReadTokens   int64     `json:"cache_read_tokens"`
	CacheWriteTokens  int64     `json:"cache_write_tokens"`
	UpstreamCostMinor *int64    `json:"upstream_cost_minor"`
	CreatedAt         time.Time `json:"created_at"`
}

// insertUsageEvent writes the usage row inside the settlement transaction. It
// is keyed by settlement_id, so a retry that reaches the insert again is a
// no-op instead of a duplicate.
func nullIfEmpty(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}

func insertUsageEvent(ctx context.Context, tx pgx.Tx, orgID, requestID, settlementID, leaseID string, u *UsageEvent) error {
	if u == nil {
		return nil
	}
	_, err := tx.Exec(ctx, `
		insert into billing.usage_events
			(org_id,request_id,settlement_id,lease_id,model,session_id,requests,input_tokens,
			 output_tokens,cache_read_tokens,cache_write_tokens,upstream_cost_minor)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		on conflict (settlement_id) do nothing`,
		orgID, requestID, settlementID, leaseID, u.Model, nullIfEmpty(u.SessionID), u.Requests, u.InputTokens,
		u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.UpstreamCostMinor)
	return err
}

type usageEventQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// queryUsageEvent loads the usage row of a settled lease. A lease settles at
// most once, so this is also the idempotent replay read path.
func queryUsageEvent(ctx context.Context, q usageEventQuerier, leaseID string) (*UsageEvent, error) {
	var u UsageEvent
	err := q.QueryRow(ctx, `
		select model,coalesce(session_id,''),requests,input_tokens,output_tokens,cache_read_tokens,
		       cache_write_tokens,upstream_cost_minor
		from billing.usage_events
		where lease_id=$1
		order by id limit 1`, leaseID).
		Scan(&u.Model, &u.SessionID, &u.Requests, &u.InputTokens, &u.OutputTokens,
			&u.CacheReadTokens, &u.CacheWriteTokens, &u.UpstreamCostMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}
