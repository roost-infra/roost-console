-- Per-session cache reporting. The relay sends the client's explicit session
-- header (X-Roost-Session by default) with each settlement; older rows and
-- session-less requests stay null and are excluded from session breakdowns.
alter table billing.usage_events
  add column if not exists session_id text
  check (session_id is null or length(btrim(session_id)) <= 200);

create index if not exists usage_events_org_session_created
  on billing.usage_events(org_id, session_id, created_at desc)
  where session_id is not null;
