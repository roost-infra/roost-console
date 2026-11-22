-- DSAlpha product plans. The old cross-product starter/pro/max bundles stay
-- active until the application deploy, so billing keeps resolving them; a
-- follow-up data migration moves orgs onto these codes and retires the old
-- rows. Prices are CNY cents; credits are the monthly allowance.
alter table billing.plans
  add column if not exists product text not null default 'dsalpha',
  add column if not exists trial_days integer not null default 0,
  add column if not exists first_month_discount_bps integer not null default 0,
  add column if not exists annual_discount_bps integer not null default 0;

insert into billing.plans
  (code,name,price_cents,duration_days,included_credits,product_caps,features,active,sort_order,
   product,trial_days,first_month_discount_bps,annual_discount_bps)
values
  ('dsalpha_free','DSAlpha Free',0,0,1000,'{"dsalpha":1000}',
   '{"products":["dsalpha"],"workspaces":1,"scheduled_tasks":1,"model_tier":0,"byok":false,"trial_days":7,"trial_credits":2000,"first_month_discount_bps":5000,"annual_discount_bps":1500}',
   true,10,'dsalpha',7,5000,1500),
  ('dsalpha_plus','DSAlpha Plus',14900,30,18000,'{"dsalpha":18000}',
   '{"products":["dsalpha"],"workspaces":10,"scheduled_tasks":5,"model_tier":1,"byok":false,"trial_days":7,"trial_credits":2000,"first_month_discount_bps":5000,"annual_discount_bps":1500}',
   true,11,'dsalpha',7,5000,1500),
  ('dsalpha_pro','DSAlpha Pro',29900,30,38000,'{"dsalpha":38000}',
   '{"products":["dsalpha"],"workspaces":20,"scheduled_tasks":20,"model_tier":2,"byok":true,"trial_days":7,"trial_credits":2000,"first_month_discount_bps":5000,"annual_discount_bps":1500}',
   true,12,'dsalpha',7,5000,1500),
  ('dsalpha_max','DSAlpha Max',74900,30,100000,'{"dsalpha":100000}',
   '{"products":["dsalpha"],"workspaces":0,"scheduled_tasks":0,"model_tier":3,"byok":true,"standard_unlimited":true,"trial_days":7,"trial_credits":2000,"first_month_discount_bps":5000,"annual_discount_bps":1500}',
   true,13,'dsalpha',7,5000,1500)
on conflict (code) do update set
  name=excluded.name, price_cents=excluded.price_cents, duration_days=excluded.duration_days,
  included_credits=excluded.included_credits, product_caps=excluded.product_caps,
  features=excluded.features, active=true, sort_order=excluded.sort_order,
  product=excluded.product, trial_days=excluded.trial_days,
  first_month_discount_bps=excluded.first_month_discount_bps,
  annual_discount_bps=excluded.annual_discount_bps;

update billing.plans set product='legacy' where code in ('free','starter','pro','max','byok-pro');
