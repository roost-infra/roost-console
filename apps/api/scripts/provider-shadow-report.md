# Provider shadow report

After migration 0017 and pricebook import:

```bash
psql "$DATABASE_URL" -f apps/api/scripts/provider-shadow-report.sql
```

Read:

- `retail_credits` — what the new retail table would charge
- `allocated_cost_credits` — FYT provider cost after 169/300 allocation
- `gross_credits` / `margin_pct` — shadow gross margin
- `missing_shadow` — events without new prices/provider
- `shadow_coverage_pct` — should approach 100% before cutting over

No production charge changes happen in shadow mode.
