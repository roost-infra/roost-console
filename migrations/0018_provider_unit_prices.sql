-- Per-call image and per-second video prices. Text stays in provider_prices /
-- retail_prices; these tables carry a unit (image call or video second) instead
-- of a per-million-token component.
create table if not exists billing.provider_unit_prices (
  id bigserial primary key,
  provider_id text not null references billing.providers(id) on delete cascade,
  version text not null,
  model text not null,
  unit text not null check (unit in ('image','video_second')),
  tier text not null default '',
  usd_micros_per_unit bigint not null check (usd_micros_per_unit >= 0),
  effective_from timestamptz not null default now(),
  unique (provider_id, version, model, unit, tier)
);

create table if not exists billing.retail_unit_prices (
  id bigserial primary key,
  version text not null,
  model text not null,
  unit text not null check (unit in ('image','video_second')),
  tier text not null default '',
  usd_micros_per_unit bigint not null check (usd_micros_per_unit >= 0),
  effective_from timestamptz not null default now(),
  unique (version, model, unit, tier)
);

alter table billing.provider_unit_prices enable row level security;
alter table billing.retail_unit_prices enable row level security;
revoke all on billing.provider_unit_prices from anon, authenticated;
revoke all on billing.retail_unit_prices from anon, authenticated;
revoke all on sequence billing.provider_unit_prices_id_seq from anon, authenticated;
revoke all on sequence billing.retail_unit_prices_id_seq from anon, authenticated;
grant select, insert, update, delete on billing.provider_unit_prices to roost_api;
grant select, insert, update, delete on billing.retail_unit_prices to roost_api;
grant usage, select on sequence billing.provider_unit_prices_id_seq to roost_api;
grant usage, select on sequence billing.retail_unit_prices_id_seq to roost_api;
