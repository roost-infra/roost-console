create table billing.relay_preferences (
  org_id uuid not null references public.organizations(id) on delete cascade,
  principal text not null,
  compression_enabled boolean,
  compression_percent smallint check (compression_percent between 0 and 100),
  memory_enabled boolean,
  revision bigint not null default 0,
  user_id uuid generated always as (nullif(principal,'')::uuid) stored,
  primary key (org_id, principal),
  foreign key (org_id, user_id) references public.organization_members on delete cascade
);

create table billing.relay_memories (
  id uuid primary key default gen_random_uuid(),
  org_id uuid not null,
  user_id uuid not null,
  body text not null check (char_length(body) between 1 and 4000),
  source text not null check (source in ('manual', 'model')),
  request_id text,
  version bigint not null default 1,
  updated_at timestamptz not null default clock_timestamp(),
  foreign key (org_id, user_id) references public.organization_members on delete cascade,
  unique (org_id, user_id, request_id)
);
create index relay_memories_owner on billing.relay_memories (org_id, user_id, updated_at desc, id desc);

create table billing.relay_memory_events (
  id bigint generated always as identity primary key,
  org_id uuid not null,
  user_id uuid not null,
  request_id text not null,
  phase text not null,
  status text not null,
  memory_ids text[] not null,
  created_at timestamptz not null default clock_timestamp(),
  foreign key (org_id, user_id) references public.organization_members on delete cascade
);
create index relay_memory_events_owner on billing.relay_memory_events (org_id, user_id, id desc);

alter table billing.relay_preferences enable row level security;
alter table billing.relay_memories enable row level security;
alter table billing.relay_memory_events enable row level security;

revoke all on billing.relay_preferences, billing.relay_memories, billing.relay_memory_events from anon, authenticated;
revoke all on sequence billing.relay_memory_events_id_seq from anon, authenticated;

grant select, insert, update, delete on billing.relay_preferences, billing.relay_memories, billing.relay_memory_events to roost_api;
grant usage, select on sequence billing.relay_memory_events_id_seq to roost_api;
