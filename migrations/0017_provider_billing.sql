-- Provider billing layer: a transit provider is a supplier (FYT, another
-- relay, or a direct vendor) plus one or more purchased plans. Provider prices
-- are the cost basis. Retail prices are what our product charges users and are
-- deliberately provider-independent, so routing a request to a different
-- upstream never changes the user's bill.
--
-- Units: micro-USD per 1,000,000 tokens. 1 credit = 1000 minor = ¥0.01;
-- 1 USD = 680 credits at the platform conversion.

create table if not exists billing.providers (
  id text primary key,
  kind text not null,                 -- transit / direct / vendor
  display_name text not null,
  capabilities jsonb not null default '{}'::jsonb,
  enabled boolean not null default true,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table if not exists billing.provider_plans (
  id text primary key,
  provider_id text not null references billing.providers(id) on delete cascade,
  plan_code text not null,
  cost_micros bigint not null check (cost_micros >= 0),
  allowance_micros bigint not null check (allowance_micros >= 0),
  basis text not null default 'provider' check (basis in ('provider','official')),
  currency text not null default 'USD',
  limits jsonb not null default '{}'::jsonb,
  key_refs jsonb not null default '[]'::jsonb,
  active boolean not null default true,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (provider_id, plan_code)
);

create table if not exists billing.provider_prices (
  id bigserial primary key,
  provider_id text not null references billing.providers(id) on delete cascade,
  version text not null,
  model text not null,
  component text not null check (component in ('input','output','cache_read','cache_write')),
  tier_max_input bigint not null default 0,      -- 0 = no upper bound
  peak text not null default '',                 -- '' / 非高峰 / 高峰
  usd_micros_per_million bigint not null check (usd_micros_per_million >= 0),
  effective_from timestamptz not null default now(),
  unique (provider_id, version, model, component, tier_max_input, peak)
);
create index if not exists provider_prices_lookup
  on billing.provider_prices(provider_id, model, component, effective_from desc);

create table if not exists billing.retail_prices (
  id bigserial primary key,
  version text not null,
  model text not null,
  component text not null check (component in ('input','output','cache_read','cache_write')),
  tier_max_input bigint not null default 0,
  peak text not null default '',
  usd_micros_per_million bigint not null check (usd_micros_per_million >= 0),
  effective_from timestamptz not null default now(),
  unique (version, model, component, tier_max_input, peak)
);
create index if not exists retail_prices_lookup
  on billing.retail_prices(model, component, effective_from desc);

-- Per-request provider facts. The user-facing charge stays in ledger; this
-- table is the cost/margin audit trail.
create table if not exists billing.provider_usage (
  id bigserial primary key,
  provider_id text not null references billing.providers(id) on delete cascade,
  plan_id text references billing.provider_plans(id) on delete set null,
  request_id text not null,
  quota bigint,
  cost_micros bigint not null default 0,
  basis text not null default 'provider',
  occurred_at timestamptz not null default now(),
  unique (provider_id, request_id)
);

-- Provider capacity is reserved separately from the user's wallet reservation.
create table if not exists billing.provider_quota_leases (
  id uuid primary key default gen_random_uuid(),
  provider_id text not null references billing.providers(id) on delete cascade,
  plan_id text not null references billing.provider_plans(id) on delete cascade,
  request_id text not null,
  reserved_micros bigint not null check (reserved_micros >= 0),
  status text not null default 'active' check (status in ('active','settled','released','expired')),
  expires_at timestamptz not null,
  created_at timestamptz not null default now(),
  unique (provider_id, request_id)
);
create index if not exists provider_quota_leases_active
  on billing.provider_quota_leases(plan_id, status, expires_at);

alter table billing.usage_events add column if not exists provider_id text;
alter table billing.usage_events add column if not exists provider_plan_id text;
alter table billing.usage_events add column if not exists provider_cost_micros bigint;
alter table billing.usage_events add column if not exists retail_minor bigint;
alter table billing.usage_events add column if not exists gross_minor bigint;
alter table billing.usage_events add column if not exists provider_cost_basis text;

alter table billing.providers enable row level security;
alter table billing.provider_plans enable row level security;
alter table billing.provider_prices enable row level security;
alter table billing.retail_prices enable row level security;
alter table billing.provider_usage enable row level security;
alter table billing.provider_quota_leases enable row level security;

revoke all on billing.providers from anon, authenticated;
revoke all on billing.provider_plans from anon, authenticated;
revoke all on billing.provider_prices from anon, authenticated;
revoke all on billing.retail_prices from anon, authenticated;
revoke all on billing.provider_usage from anon, authenticated;
revoke all on billing.provider_quota_leases from anon, authenticated;
revoke all on sequence billing.provider_prices_id_seq from anon, authenticated;
revoke all on sequence billing.retail_prices_id_seq from anon, authenticated;
revoke all on sequence billing.provider_usage_id_seq from anon, authenticated;

grant select, insert, update, delete on billing.providers to roost_api;
grant select, insert, update, delete on billing.provider_plans to roost_api;
grant select, insert, update, delete on billing.provider_prices to roost_api;
grant select, insert, update, delete on billing.retail_prices to roost_api;
grant select, insert, update, delete on billing.provider_usage to roost_api;
grant select, insert, update, delete on billing.provider_quota_leases to roost_api;
grant usage, select on sequence billing.provider_prices_id_seq to roost_api;
grant usage, select on sequence billing.retail_prices_id_seq to roost_api;
grant usage, select on sequence billing.provider_usage_id_seq to roost_api;

insert into billing.providers(id, kind, display_name, capabilities)
values ('fyt-codeplan', 'transit', 'FYT CodePlan', '{"protocols":["openai","anthropic"],"input_includes_cache":true}'::jsonb)
on conflict (id) do nothing;

insert into billing.provider_plans(id, provider_id, plan_code, cost_micros, allowance_micros, basis, limits)
values (
  'fyt-codeplan-starter', 'fyt-codeplan', 'starter',
  169000000, 300000000, 'provider',
  '{"daily_limit_micros":null,"five_hour_limit_micros":null,"weekly_limit_micros":null}'::jsonb
)
on conflict (id) do nothing;
