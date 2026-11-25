package billing

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/JingxuanC/roost-console/apps/api/internal/billing/provider"
)

// shadowProviderQuote computes the provider/retail split for a settlement
// without changing the amount charged. It is additive: a missing migration,
// missing price row, or malformed plan returns nil so the legacy settlement is
// never blocked by shadow accounting.
func (s *Store) shadowProviderQuote(
	ctx context.Context,
	tx pgx.Tx,
	providerID, planID, model string,
	original, upstream TokenUsage,
) (*provider.Quote, error) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" || strings.TrimSpace(model) == "" {
		return nil, nil
	}
	retail, ok, err := loadShadowBook(ctx, tx, `
		select distinct on (component,tier_max_input,peak)
		       component,tier_max_input,peak,usd_micros_per_million
		from billing.retail_prices
		where model=$1 and effective_from <= now()
		order by component,tier_max_input,peak,effective_from desc`, model)
	if err != nil || !ok {
		return nil, err
	}
	prov, ok, err := loadShadowBook(ctx, tx, `
		select distinct on (component,tier_max_input,peak)
		       component,tier_max_input,peak,usd_micros_per_million
		from billing.provider_prices
		where provider_id=$1 and model=$2 and effective_from <= now()
		order by component,tier_max_input,peak,effective_from desc`, providerID, model)
	if err != nil || !ok {
		return nil, err
	}
	var plan provider.Plan
	planID = strings.TrimSpace(planID)
	var row pgx.Row
	if planID != "" {
		row = tx.QueryRow(ctx, `
			select cost_micros,allowance_micros,basis
			from billing.provider_plans
			where id=$1 and active`, planID)
	} else {
		row = tx.QueryRow(ctx, `
			select cost_micros,allowance_micros,basis
			from billing.provider_plans
			where provider_id=$1 and active
			order by created_at desc limit 1`, providerID)
	}
	if err := row.Scan(&plan.CostMicros, &plan.AllowanceMicros, &plan.Basis); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if plan.Basis == "" {
		plan.Basis = provider.BasisProvider
	}
	// Official-basis plans need a separate official price table; do not
	// silently price them against FYT's list.
	if plan.Basis == provider.BasisOfficial {
		return nil, nil
	}
	calc := provider.Calculator{
		Retail:   provider.Book{model: retail},
		Provider: provider.Book{model: prov},
		Plan:     plan,
	}
	u := provider.Usage{
		OriginalInput:     original.Input,
		Input:             upstream.Input,
		Output:            upstream.Output,
		CacheRead:         upstream.CacheRead,
		CacheWrite:        upstream.CacheWrite,
		RetailTierInput:   original.Input,
		ProviderTierInput: upstream.Input + upstream.CacheRead,
	}
	q, err := calc.Quote(provider.Request{Model: model, Usage: u})
	if err != nil {
		return nil, err
	}
	return &q, nil
}

func loadShadowBook(ctx context.Context, tx pgx.Tx, query string, args ...any) (provider.ModelPrices, bool, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	type tierKey struct {
		maxInput int64
		peak     string
	}
	byTier := map[tierKey]provider.Price{}
	for rows.Next() {
		var component, peak string
		var maxInput, micros int64
		if err := rows.Scan(&component, &maxInput, &peak, &micros); err != nil {
			return nil, false, err
		}
		key := tierKey{maxInput: maxInput, peak: peak}
		p := byTier[key]
		switch component {
		case "input":
			p.Input = micros
		case "output":
			p.Output = micros
		case "cache_read":
			p.CacheRead = micros
		case "cache_write":
			p.CacheWrite = micros
		}
		byTier[key] = p
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(byTier) == 0 {
		return nil, false, nil
	}
	tiers := make(provider.ModelPrices, 0, len(byTier))
	for key, price := range byTier {
		tiers = append(tiers, provider.Tier{MaxInput: key.maxInput, Peak: key.peak, Price: price})
	}
	return tiers, true, nil
}
