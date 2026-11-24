-- Billing V2: one price source (price_book + settings), per-settlement price
-- version locking and open debts for under-paid usage.
--
-- Units: 1 credit = ¥0.01; 1000 minor = 1 credit; prices are minor per
-- 1,000,000 tokens. retail is what the user pays; cost is the CNY cost at the
-- time the row was imported (never a fixed-rate CREDIT conversion).
create table if not exists billing.settings (
  key text primary key,
  value jsonb not null,
  updated_at timestamptz not null default now()
);

insert into billing.settings(key,value) values
  ('fx.usd_to_credits','720'),
  ('billing.credit_yuan','0.01'),
  ('billing.rounding','"ceil"'),
  ('billing.min_charge_credits','1'),
  ('compression.user_share_bps','2500'),
  ('compression.min_net_gain_credits','1'),
  ('sandbox.markup_bps','13000'),
  ('sandbox.disk_free_gib','5'),
  ('tools.search.price_minor','7'),
  ('tools.fetch.price_minor','7'),
  ('memory.call.price_credits','1'),
  ('settings.version','"2026-11-25.1"')
on conflict (key) do nothing;

-- Immutable settings snapshots: a lease locks a settings_version and a later
-- settlement replays the exact share/markup/rounding values of that version.
create table if not exists billing.settings_versions (
  version text primary key,
  snapshot jsonb not null,
  created_at timestamptz not null default now()
);

insert into billing.settings_versions(version,snapshot)
select '2026-11-25.1', coalesce(jsonb_object_agg(key, value), '{}'::jsonb)
from billing.settings
on conflict (version) do nothing;

create table if not exists billing.price_book (
  id bigserial primary key,
  version text not null,
  model text not null,
  currency text not null default 'CREDIT',
  component text not null check (component in ('input','output','cache_read','cache_write')),
  tier_max_input bigint not null default 0,           -- 0 = no upper bound
  retail_minor_per_million bigint not null check (retail_minor_per_million >= 0),
  cost_minor_per_million bigint not null check (cost_minor_per_million >= 0),
  cost_currency text not null default 'CNY',
  effective_from timestamptz not null,
  unique (version, model, component, tier_max_input)
);
create index if not exists price_book_lookup on billing.price_book(model, component, effective_from desc);

create table if not exists billing.price_book_peak (
  version text not null,
  model text not null,
  weekdays smallint[] not null,
  start_minute smallint not null,
  end_minute smallint not null,
  timezone text not null,
  multiplier_bps int not null check (multiplier_bps > 0),
  primary key (version, model, weekdays, start_minute, end_minute)
);

create table if not exists billing.debts (
  id uuid primary key default gen_random_uuid(),
  org_id uuid not null,
  product text not null,
  run_ref text not null,
  amount_credits bigint not null check (amount_credits > 0),
  reason text not null,
  status text not null default 'open',
  created_at timestamptz not null default now(),
  resolved_at timestamptz
);
create unique index if not exists debts_once on billing.debts(org_id, product, run_ref, reason);

alter table billing.ledger add column if not exists policy_version text;
alter table billing.ledger add column if not exists settings_version text;
alter table billing.usage_events add column if not exists policy_version text;
alter table billing.usage_events add column if not exists upstream_id text;
alter table billing.usage_leases add column if not exists price_version text;
alter table billing.usage_leases add column if not exists settings_version text;
alter table billing.usage_leases add column if not exists upstream_id text;

alter table billing.usage_compression
  add column if not exists charge_input_tokens bigint,
  add column if not exists upstream_input_tokens bigint,
  add column if not exists user_share_tokens bigint,
  add column if not exists user_share_bps int,
  add column if not exists retail_minor_per_million bigint,
  add column if not exists cost_minor_per_million bigint,
  add column if not exists summary_cost_minor bigint,
  add column if not exists cache_loss_minor bigint default 0,
  add column if not exists no_compression_margin_minor bigint,
  add column if not exists compressed_margin_minor bigint,
  add column if not exists net_gain_minor bigint;

-- Internal-only tables: no anon/authenticated access, same application role as
-- the rest of the billing schema.
alter table billing.settings enable row level security;
alter table billing.settings_versions enable row level security;
alter table billing.price_book enable row level security;
alter table billing.price_book_peak enable row level security;
alter table billing.debts enable row level security;
revoke all on billing.settings from anon, authenticated;
revoke all on billing.settings_versions from anon, authenticated;
revoke all on billing.price_book from anon, authenticated;
revoke all on billing.price_book_peak from anon, authenticated;
revoke all on billing.debts from anon, authenticated;
revoke all on sequence billing.price_book_id_seq from anon, authenticated;

grant select, insert, update, delete on billing.settings to roost_api;
grant select, insert, update, delete on billing.settings_versions to roost_api;
grant select, insert, update, delete on billing.price_book to roost_api;
grant select, insert, update, delete on billing.price_book_peak to roost_api;
grant select, insert, update, delete on billing.debts to roost_api;
grant usage, select on sequence billing.price_book_id_seq to roost_api;
