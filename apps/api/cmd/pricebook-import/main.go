// pricebook-import loads the provider/retail pricebook JSON into the provider
// billing tables. It is intentionally a separate command: importing prices is
// an operator action, not a server startup side effect.
//
// Usage:
//
//	DATABASE_URL=... go run ./apps/api/cmd/pricebook-import \
//	  -file apps/api/internal/billing/provider/testdata/pricebook.json \
//	  -provider fyt-codeplan -plan-code starter
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JingxuanC/roost-console/apps/api/internal/billing/provider"
	"github.com/JingxuanC/roost-console/apps/api/internal/db"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var (
		file       = flag.String("file", "apps/api/internal/billing/provider/testdata/pricebook.json", "pricebook JSON path")
		providerID = flag.String("provider", "fyt-codeplan", "provider id")
		planCode   = flag.String("plan-code", "starter", "provider plan code")
		dryRun     = flag.Bool("dry-run", false, "validate/read prices without writing")
	)
	flag.Parse()

	f, err := os.Open(*file)
	if err != nil {
		log.Error("open pricebook", "err", err)
		os.Exit(2)
	}
	defer f.Close()
	doc, err := provider.LoadPricebook(f)
	if err != nil {
		log.Error("load pricebook", "err", err)
		os.Exit(2)
	}
	log.Info("pricebook parsed", "version", doc.Version, "models", len(doc.Models), "provider", *providerID)
	if *dryRun {
		for _, m := range doc.Models {
			log.Info("would import", "model", m.Model, "tier", m.Tier, "max_input", m.MaxInput)
		}
		return
	}

	url := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if url == "" {
		log.Error("DATABASE_URL is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, url)
	if err != nil {
		log.Error("database connection failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Error("begin", "err", err)
		os.Exit(1)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		insert into billing.providers(id,kind,display_name,capabilities)
		values ($1,'transit',$1,'{}'::jsonb)
		on conflict (id) do nothing`, *providerID); err != nil {
		log.Error("ensure provider", "err", err)
		os.Exit(1)
	}
	if doc.Plan.ID != "" {
		if _, err := tx.Exec(ctx, `
			insert into billing.provider_plans
				(id,provider_id,plan_code,cost_micros,allowance_micros,basis,active)
			values ($1,$2,$3,$4,$5,$6,true)
			on conflict (id) do update set
				provider_id=excluded.provider_id,
				plan_code=excluded.plan_code,
				cost_micros=excluded.cost_micros,
				allowance_micros=excluded.allowance_micros,
				basis=excluded.basis,
				active=true,
				updated_at=now()`,
			doc.Plan.ID, *providerID, *planCode, doc.Plan.CostMicros, doc.Plan.AllowanceMicros, string(doc.Plan.Basis)); err != nil {
			log.Error("upsert plan", "err", err)
			os.Exit(1)
		}
	}
	for _, m := range doc.Models {
		for _, component := range []string{"input", "output", "cache_read", "cache_write"} {
			retailPrice := componentPrice(m.Retail, component)
			if _, err := tx.Exec(ctx, `
				insert into billing.retail_prices
					(version,model,component,tier_max_input,peak,usd_micros_per_million,effective_from)
				values ($1,$2,$3,$4,$5,$6,now())
				on conflict (version,model,component,tier_max_input,peak) do update set
					usd_micros_per_million=excluded.usd_micros_per_million,
					effective_from=excluded.effective_from`,
				doc.Version, m.Model, component, m.MaxInput, m.Peak, retailPrice); err != nil {
				log.Error("upsert retail", "model", m.Model, "component", component, "err", err)
				os.Exit(1)
			}
			providerPrice := componentPrice(m.Provider, component)
			if _, err := tx.Exec(ctx, `
				insert into billing.provider_prices
					(provider_id,version,model,component,tier_max_input,peak,usd_micros_per_million,effective_from)
				values ($1,$2,$3,$4,$5,$6,$7,now())
				on conflict (provider_id,version,model,component,tier_max_input,peak) do update set
					usd_micros_per_million=excluded.usd_micros_per_million,
					effective_from=excluded.effective_from`,
				*providerID, doc.Version, m.Model, component, m.MaxInput, m.Peak, providerPrice); err != nil {
				log.Error("upsert provider", "model", m.Model, "component", component, "err", err)
				os.Exit(1)
			}
		}
	}

	for _, u := range doc.Images {
		if err := upsertUnit(ctx, tx, doc.Version, *providerID, u, "image", ""); err != nil {
			log.Error("upsert image", "model", u.Model, "err", err)
			os.Exit(1)
		}
	}
	for _, u := range doc.Videos {
		if err := upsertUnit(ctx, tx, doc.Version, *providerID, u, "video_second", u.Tier); err != nil {
			log.Error("upsert video", "model", u.Model, "tier", u.Tier, "err", err)
			os.Exit(1)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		log.Error("commit", "err", err)
		os.Exit(1)
	}
	log.Info("pricebook imported", "version", doc.Version, "models", len(doc.Models), "provider", *providerID)
}

func upsertUnit(ctx context.Context, tx pgx.Tx, version, providerID string, u provider.UnitPrice, unit, tier string) error {
	if _, err := tx.Exec(ctx, `
		insert into billing.retail_unit_prices
			(version,model,unit,tier,usd_micros_per_unit,effective_from)
		values ($1,$2,$3,$4,$5,now())
		on conflict (version,model,unit,tier) do update set
			usd_micros_per_unit=excluded.usd_micros_per_unit,
			effective_from=excluded.effective_from`,
		version, u.Model, unit, tier, u.RetailMicros); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		insert into billing.provider_unit_prices
			(provider_id,version,model,unit,tier,usd_micros_per_unit,effective_from)
		values ($1,$2,$3,$4,$5,$6,now())
		on conflict (provider_id,version,model,unit,tier) do update set
			usd_micros_per_unit=excluded.usd_micros_per_unit,
			effective_from=excluded.effective_from`,
		providerID, version, u.Model, unit, tier, u.ProviderMicros)
	return err
}

func componentPrice(p provider.Price, component string) int64 {
	switch component {
	case "input":
		return p.Input
	case "output":
		return p.Output
	case "cache_read":
		return p.CacheRead
	case "cache_write":
		return p.CacheWrite
	default:
		panic(fmt.Sprintf("unknown component %q", component))
	}
}
