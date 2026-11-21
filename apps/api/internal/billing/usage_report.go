package billing

import (
	"context"
	"sort"
	"time"
)

// Usage dashboard reporting. The relay settlement writes one usage event per
// request; compression savings live in their own table and are joined by
// settlement id. Ledger charges are the source of truth for spend, while the
// usage rows are the source of truth for token counts.
const (
	UsageDaysDefault = 30
	UsageDaysMax     = 365

	UsageRecentDefaultLimit = 50
	UsageRecentMaxLimit     = 200

	// UsageSessionLimit caps the per-session breakdown in one summary.
	UsageSessionLimit = 20
)

// usageSince keeps summary and daily ranges aligned to UTC calendar days, so
// the frontend can render exactly N zero-filled buckets without an off-by-one
// at the range boundary.
const usageSince = `(date_trunc('day', now() at time zone 'UTC') at time zone 'UTC') - make_interval(days => $2 - 1)`

// UsageTotals is the aggregate of one organization's usage in the range.
type UsageTotals struct {
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CacheReadTokens   int64   `json:"cache_read_tokens"`
	CacheWriteTokens  int64   `json:"cache_write_tokens"`
	TotalTokens       int64   `json:"total_tokens"`
	CreditsMinor      int64   `json:"credits_minor"`
	UpstreamCostMinor int64   `json:"upstream_cost_minor"`
	CacheHitRate      float64 `json:"cache_hit_rate"`
	SavedTokens       int64   `json:"saved_tokens"`
	UserShareTokens   int64   `json:"user_share_tokens"`
	UserShareMinor    int64   `json:"user_share_minor"`
	SummaryCostMinor  int64   `json:"summary_cost_minor"`
}

// UsageSessionRow is one session's token usage in the range.
type UsageSessionRow struct {
	SessionID        string    `json:"session_id"`
	Requests         int64     `json:"requests"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	CacheHitRate     float64   `json:"cache_hit_rate"`
	LastSeen         time.Time `json:"last_seen"`
}

// UsageSummary is the summary endpoint's payload. Sessions is the per-session
// token breakdown (newest-token-first, capped), and SessionHitRate is the warm
// session cache rate: sessions with any cache read, weighted by their tokens.
type UsageSummary struct {
	Days           int               `json:"days"`
	Totals         UsageTotals       `json:"totals"`
	Sessions       []UsageSessionRow `json:"sessions"`
	SessionCount   int64             `json:"session_count"`
	SessionHitRate float64           `json:"session_hit_rate"`
}

// UsageDailyRow is one UTC day's bucket. Only days with at least one source
// row are returned; the frontend fills the empty buckets for its chart.
type UsageDailyRow struct {
	Date             string `json:"date"`
	Requests         int64  `json:"requests"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CreditsMinor     int64  `json:"credits_minor"`
	SavedTokens      int64  `json:"saved_tokens"`
	UserShareTokens  int64  `json:"user_share_tokens"`
	UserShareMinor   int64  `json:"user_share_minor"`
}

// UsageRecentRow is one settled request in the recent activity table. The
// compression fields are zero when the settlement had no savings row.
type UsageRecentRow struct {
	CreatedAt             time.Time `json:"created_at"`
	Model                 string    `json:"model"`
	RequestID             string    `json:"request_id"`
	Requests              int64     `json:"requests"`
	InputTokens           int64     `json:"input_tokens"`
	OutputTokens          int64     `json:"output_tokens"`
	CacheReadTokens       int64     `json:"cache_read_tokens"`
	CacheWriteTokens      int64     `json:"cache_write_tokens"`
	TotalTokens           int64     `json:"total_tokens"`
	CacheHitRate          float64   `json:"cache_hit_rate"`
	CreditsMinor          int64     `json:"credits_minor"`
	SavedTokens           int64     `json:"saved_tokens"`
	UserShareTokens       int64     `json:"user_share_tokens"`
	UserShareMinor        int64     `json:"user_share_minor"`
	CompressionApplied    bool      `json:"compression_applied"`
	OriginalInputTokens   int64     `json:"original_input_tokens"`
	CompressedInputTokens int64     `json:"compressed_input_tokens"`
}

func clampUsageDays(days int) int {
	if days < 1 {
		return 1
	}
	if days > UsageDaysMax {
		return UsageDaysMax
	}
	return days
}

func clampUsageLimit(limit int) int {
	if limit < 1 {
		return UsageRecentDefaultLimit
	}
	if limit > UsageRecentMaxLimit {
		return UsageRecentMaxLimit
	}
	return limit
}

func cacheHitRate(cacheRead, input int64) float64 {
	denom := input + cacheRead
	if denom <= 0 {
		return 0
	}
	return float64(cacheRead) / float64(denom)
}

// UsageSummary aggregates one organization's usage for the trailing N UTC
// calendar days. Every query is scoped by org_id.
func (s *Store) UsageSummary(ctx context.Context, orgID string, days int) (UsageSummary, error) {
	days = clampUsageDays(days)
	out := UsageSummary{Days: days}

	if err := s.Pool.QueryRow(ctx, `
		select coalesce(sum(requests),0)::bigint,
		       coalesce(sum(input_tokens),0)::bigint,
		       coalesce(sum(output_tokens),0)::bigint,
		       coalesce(sum(cache_read_tokens),0)::bigint,
		       coalesce(sum(cache_write_tokens),0)::bigint,
		       coalesce(sum(upstream_cost_minor),0)::bigint
		from billing.usage_events
		where org_id=$1 and created_at >= `+usageSince, orgID, days).
		Scan(&out.Totals.Requests, &out.Totals.InputTokens, &out.Totals.OutputTokens,
			&out.Totals.CacheReadTokens, &out.Totals.CacheWriteTokens,
			&out.Totals.UpstreamCostMinor); err != nil {
		return UsageSummary{}, err
	}

	// Spend is the absolute relay ledger movement in the range, expressed in the
	// relay's minor unit (1000 minor = 1 credit).
	var credits int64
	if err := s.Pool.QueryRow(ctx, `
		select coalesce(sum(abs(delta_credits)),0)::bigint
		from billing.ledger
		where org_id=$1 and product='relay' and created_at >= `+usageSince, orgID, days).
		Scan(&credits); err != nil {
		return UsageSummary{}, err
	}
	out.Totals.CreditsMinor = credits * 1000

	if err := s.Pool.QueryRow(ctx, `
		select coalesce(sum(saved_tokens),0)::bigint,
		       coalesce(sum(user_share_tokens),0)::bigint,
		       coalesce(sum(user_share_minor),0)::bigint,
		       coalesce(sum(summary_cost_minor),0)::bigint
		from billing.usage_compression
		where org_id=$1 and created_at >= `+usageSince, orgID, days).
		Scan(&out.Totals.SavedTokens, &out.Totals.UserShareTokens,
			&out.Totals.UserShareMinor, &out.Totals.SummaryCostMinor); err != nil {
		return UsageSummary{}, err
	}

	out.Totals.TotalTokens = out.Totals.InputTokens + out.Totals.OutputTokens +
		out.Totals.CacheReadTokens + out.Totals.CacheWriteTokens
	out.Totals.CacheHitRate = cacheHitRate(out.Totals.CacheReadTokens, out.Totals.InputTokens)

	// Per-session breakdown. Older rows (before session_id existed) and
	// session-less requests are excluded; the frontend labels the section so an
	// empty list is not mistaken for "no cache".
	sessionRows, err := s.Pool.Query(ctx, `
		select session_id,
		       coalesce(sum(requests),0)::bigint,
		       coalesce(sum(input_tokens),0)::bigint,
		       coalesce(sum(output_tokens),0)::bigint,
		       coalesce(sum(cache_read_tokens),0)::bigint,
		       coalesce(sum(cache_write_tokens),0)::bigint,
		       max(created_at),
		       coalesce(sum(cache_read_tokens) filter (where cache_read_tokens > 0),0)::bigint,
		       coalesce(sum(input_tokens) filter (where cache_read_tokens > 0),0)::bigint
		from billing.usage_events
		where org_id=$1 and created_at >= `+usageSince+`
		  and session_id is not null and btrim(session_id) <> ''
		group by session_id
		order by sum(input_tokens + output_tokens + cache_read_tokens + cache_write_tokens) desc
		limit $3`, orgID, days, UsageSessionLimit)
	if err != nil {
		return UsageSummary{}, err
	}
	defer sessionRows.Close()
	out.Sessions = make([]UsageSessionRow, 0, UsageSessionLimit)
	for sessionRows.Next() {
		var row UsageSessionRow
		var warmCache, warmInput int64
		if err := sessionRows.Scan(&row.SessionID, &row.Requests, &row.InputTokens,
			&row.OutputTokens, &row.CacheReadTokens, &row.CacheWriteTokens, &row.LastSeen,
			&warmCache, &warmInput); err != nil {
			return UsageSummary{}, err
		}
		row.TotalTokens = row.InputTokens + row.OutputTokens + row.CacheReadTokens + row.CacheWriteTokens
		// Per-session rate is warm-only: the first (cold) request would
		// otherwise make every healthy session look like a coin flip.
		row.CacheHitRate = sessionCacheHitRate(warmCache, warmInput)
		out.Sessions = append(out.Sessions, row)
	}
	if err := sessionRows.Err(); err != nil {
		return UsageSummary{}, err
	}

	if err := s.Pool.QueryRow(ctx, `
		select count(distinct session_id)::bigint
		from billing.usage_events
		where org_id=$1 and created_at >= `+usageSince+`
		  and session_id is not null and btrim(session_id) <> ''`, orgID, days).
		Scan(&out.SessionCount); err != nil {
		return UsageSummary{}, err
	}
	// SessionHitRate uses the same warm-session filter as the per-row metric.
	var warmCache, warmInput int64
	if err := s.Pool.QueryRow(ctx, `
		select coalesce(sum(cache_read_tokens),0)::bigint,
		       coalesce(sum(input_tokens),0)::bigint
		from billing.usage_events
		where org_id=$1 and created_at >= `+usageSince+`
		  and session_id is not null and btrim(session_id) <> ''
		  and cache_read_tokens > 0`, orgID, days).Scan(&warmCache, &warmInput); err != nil {
		return UsageSummary{}, err
	}
	out.SessionHitRate = sessionCacheHitRate(warmCache, warmInput)
	return out, nil
}

// sessionCacheHitRate handles both provider conventions: OpenAI-compatible
// prompt_tokens include the cached tokens (cache_read <= input), while native
// Anthropic reports uncached input separately (cache_read may exceed input).
// Preferring cache_read/input for the inclusive shape keeps a warm session at
// its true ~99% instead of halving it with an input+cache denominator.
func sessionCacheHitRate(cacheRead, input int64) float64 {
	if cacheRead <= 0 {
		return 0
	}
	if input >= cacheRead && input > 0 {
		return float64(cacheRead) / float64(input)
	}
	denom := input + cacheRead
	if denom <= 0 {
		return 0
	}
	return float64(cacheRead) / float64(denom)
}

// UsageDaily returns one bucket per UTC day that has data, oldest first.
func (s *Store) UsageDaily(ctx context.Context, orgID string, days int) ([]UsageDailyRow, error) {
	days = clampUsageDays(days)
	buckets := map[string]*UsageDailyRow{}

	rows, err := s.Pool.Query(ctx, `
		select to_char(created_at at time zone 'UTC','YYYY-MM-DD'),
		       coalesce(sum(requests),0)::bigint,
		       coalesce(sum(input_tokens),0)::bigint,
		       coalesce(sum(output_tokens),0)::bigint,
		       coalesce(sum(cache_read_tokens),0)::bigint,
		       coalesce(sum(cache_write_tokens),0)::bigint
		from billing.usage_events
		where org_id=$1 and created_at >= `+usageSince+`
		group by 1 order by 1`, orgID, days)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var date string
		var row UsageDailyRow
		if err := rows.Scan(&date, &row.Requests, &row.InputTokens, &row.OutputTokens,
			&row.CacheReadTokens, &row.CacheWriteTokens); err != nil {
			rows.Close()
			return nil, err
		}
		row.Date = date
		row.TotalTokens = row.InputTokens + row.OutputTokens + row.CacheReadTokens + row.CacheWriteTokens
		buckets[date] = &row
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	creditRows, err := s.Pool.Query(ctx, `
		select to_char(created_at at time zone 'UTC','YYYY-MM-DD'),
		       coalesce(sum(abs(delta_credits)),0)::bigint
		from billing.ledger
		where org_id=$1 and product='relay' and created_at >= `+usageSince+`
		group by 1`, orgID, days)
	if err != nil {
		return nil, err
	}
	for creditRows.Next() {
		var date string
		var credits int64
		if err := creditRows.Scan(&date, &credits); err != nil {
			creditRows.Close()
			return nil, err
		}
		bucket := buckets[date]
		if bucket == nil {
			bucket = &UsageDailyRow{Date: date}
			buckets[date] = bucket
		}
		bucket.CreditsMinor = credits * 1000
	}
	if err := creditRows.Err(); err != nil {
		creditRows.Close()
		return nil, err
	}
	creditRows.Close()

	compressionRows, err := s.Pool.Query(ctx, `
		select to_char(created_at at time zone 'UTC','YYYY-MM-DD'),
		       coalesce(sum(saved_tokens),0)::bigint,
		       coalesce(sum(user_share_tokens),0)::bigint,
		       coalesce(sum(user_share_minor),0)::bigint
		from billing.usage_compression
		where org_id=$1 and created_at >= `+usageSince+`
		group by 1`, orgID, days)
	if err != nil {
		return nil, err
	}
	for compressionRows.Next() {
		var date string
		var saved, shareTokens, shareMinor int64
		if err := compressionRows.Scan(&date, &saved, &shareTokens, &shareMinor); err != nil {
			compressionRows.Close()
			return nil, err
		}
		bucket := buckets[date]
		if bucket == nil {
			bucket = &UsageDailyRow{Date: date}
			buckets[date] = bucket
		}
		bucket.SavedTokens = saved
		bucket.UserShareTokens = shareTokens
		bucket.UserShareMinor = shareMinor
	}
	if err := compressionRows.Err(); err != nil {
		compressionRows.Close()
		return nil, err
	}
	compressionRows.Close()

	out := make([]UsageDailyRow, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, *bucket)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out, nil
}

// UsageRecent returns the newest settled usage rows. Credits are joined per
// lease from the relay ledger and compression savings per settlement.
func (s *Store) UsageRecent(ctx context.Context, orgID string, limit int) ([]UsageRecentRow, error) {
	limit = clampUsageLimit(limit)
	rows, err := s.Pool.Query(ctx, `
		select ue.created_at, ue.model, ue.request_id, ue.requests,
		       ue.input_tokens, ue.output_tokens, ue.cache_read_tokens, ue.cache_write_tokens,
		       coalesce(c.applied,false),
		       coalesce(c.original_input_tokens,0),
		       coalesce(c.compressed_input_tokens,0),
		       coalesce(c.saved_tokens,0),
		       coalesce(c.user_share_tokens,0),
		       coalesce(c.user_share_minor,0),
		       coalesce((select sum(abs(l.delta_credits))
		                 from billing.ledger l
		                 where l.org_id=ue.org_id and l.product='relay'
		                   and l.ref_id=ue.lease_id::text),0)::bigint * 1000
		from billing.usage_events ue
		left join billing.usage_compression c
		  on c.settlement_id=ue.settlement_id and c.org_id=ue.org_id
		where ue.org_id=$1
		order by ue.created_at desc, ue.id desc
		limit $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]UsageRecentRow, 0)
	for rows.Next() {
		var row UsageRecentRow
		if err := rows.Scan(&row.CreatedAt, &row.Model, &row.RequestID, &row.Requests,
			&row.InputTokens, &row.OutputTokens, &row.CacheReadTokens, &row.CacheWriteTokens,
			&row.CompressionApplied, &row.OriginalInputTokens, &row.CompressedInputTokens,
			&row.SavedTokens, &row.UserShareTokens, &row.UserShareMinor, &row.CreditsMinor); err != nil {
			return nil, err
		}
		row.TotalTokens = row.InputTokens + row.OutputTokens + row.CacheReadTokens + row.CacheWriteTokens
		row.CacheHitRate = cacheHitRate(row.CacheReadTokens, row.InputTokens)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
