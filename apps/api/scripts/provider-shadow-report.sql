-- Provider billing shadow report.
--
-- Run after migration 0017 and after the pricebook import:
--   psql "$DATABASE_URL" -f apps/api/scripts/provider-shadow-report.sql
--
-- Units:
--   retail_minor / gross_minor / upstream_cost_minor: 1000 minor = 1 credit
--   provider_cost_micros: micro-USD; credits = micros / 1,000,000 * 680
--   provider_cost_basis: provider | official
with recent as (
  select
    date_trunc('day', created_at)::date as day,
    coalesce(provider_id, 'legacy') as provider_id,
    model,
    requests,
    input_tokens,
    output_tokens,
    cache_read_tokens,
    cache_write_tokens,
    upstream_cost_minor,
    retail_minor,
    provider_cost_micros,
    gross_minor
  from billing.usage_events
  where created_at >= now() - interval '7 days'
)
select
  day,
  provider_id,
  model,
  sum(requests)::bigint as requests,
  count(*) filter (where retail_minor is null)::bigint as missing_shadow,
  round(sum(coalesce(retail_minor,0)) / 1000.0, 2) as retail_credits,
  round(sum(coalesce(provider_cost_micros,0)) / 1000000.0 * 680.0, 2) as allocated_cost_credits,
  round(sum(coalesce(gross_minor,0)) / 1000.0, 2) as gross_credits,
  case
    when sum(coalesce(retail_minor,0)) > 0
    then round(sum(coalesce(gross_minor,0))::numeric * 100 / sum(coalesce(retail_minor,0)), 1)
    else null
  end as margin_pct,
  round(avg(cache_read_tokens::numeric / nullif(input_tokens + cache_read_tokens + cache_write_tokens, 0)) * 100, 1) as cache_hit_pct
from recent
group by day, provider_id, model
order by day desc, retail_credits desc nulls last;

-- One-line health: are shadow rows appearing at all?
select
  count(*)::bigint as total_events,
  count(*) filter (where retail_minor is not null)::bigint as shadow_events,
  count(*) filter (where provider_id is not null)::bigint as provider_events,
  round(count(*) filter (where retail_minor is not null)::numeric * 100 / nullif(count(*),0), 1) as shadow_coverage_pct
from billing.usage_events
where created_at >= now() - interval '7 days';
