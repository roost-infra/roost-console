-- Direct-USDT orders are reconciled by matching (chain, amount) against the
-- operator's own deposit address, so an amount may only be quoted to one open
-- order at a time. The API checks this before inserting and retries on
-- collision; the index closes the race between the check and the insert.
create unique index if not exists orders_usdt_open_amount
  on billing.orders ((raw->>'usdt_chain'), (raw->>'usdt_amount'))
  where provider = 'okx_usdt' and status in ('created', 'pending');
