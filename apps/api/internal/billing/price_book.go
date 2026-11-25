package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Billing V2 pricing errors. The HTTP layer maps them to the codes in the
// implementation contract (section 8).
var (
	ErrPriceBookNotFound     = errors.New("billing: price book not found")
	ErrUnknownModel          = errors.New("billing: unknown model")
	ErrPriceTierNotFound     = errors.New("billing: no price tier for token count")
	ErrPriceVersionMismatch  = errors.New("billing: price version mismatch")
	ErrAmountMismatch        = errors.New("billing: amount mismatch")
	ErrInvalidPricing        = errors.New("billing: invalid pricing input")
	ErrInvalidSettings       = errors.New("billing: invalid settings")
	ErrSettingsVersionExists = errors.New("billing: settings version already snapshotted")
)

// Price components. The order is the canonical order used by price books,
// hashes and settlement breakdowns.
const (
	ComponentInput      = "input"
	ComponentOutput     = "output"
	ComponentCacheRead  = "cache_read"
	ComponentCacheWrite = "cache_write"
)

var PriceComponents = []string{ComponentInput, ComponentOutput, ComponentCacheRead, ComponentCacheWrite}

// cstZone formats price-book timestamps the way the contract examples do
// (Asia/Shanghai, +08:00). A fixed zone avoids depending on tzdata for output;
// peak windows still load their own IANA location.
var cstZone = time.FixedZone("CST", 8*60*60)

// ComponentPrice is one component price of one tier. Retail is what the user
// pays; Cost is the CNY cost captured when the book version was imported.
type ComponentPrice struct {
	RetailMinorPerMillion int64
	CostMinorPerMillion   int64
	CostCurrency          string
}

// PriceTier is one input-length band. MaxInput == 0 means "no upper bound".
type PriceTier struct {
	MaxInput int64
	Prices   map[string]ComponentPrice
}

// Price returns the component price, zero when the tier omits it.
func (t PriceTier) Price(component string) ComponentPrice {
	p, ok := t.Prices[component]
	if !ok {
		return ComponentPrice{CostCurrency: "CNY"}
	}
	if p.CostCurrency == "" {
		p.CostCurrency = "CNY"
	}
	return p
}

// PeakWindow multiplies every component price while started_at falls inside
// the window, evaluated in the window's own timezone.
type PeakWindow struct {
	Weekdays      []int
	StartMinute   int
	EndMinute     int
	Timezone      string
	MultiplierBPS int64
}

// PriceModel is one model's full retail/cost schedule for a version.
type PriceModel struct {
	Model    string
	Currency string
	Tiers    []PriceTier
	Peak     []PeakWindow
}

// PriceTool is a per-call tool price resolved from settings.
type PriceTool struct {
	Tool       string
	Unit       string
	PriceMinor int64
}

// PriceBook is one immutable version of the price book, ready for pricing and
// for the public/internal HTTP views.
type PriceBook struct {
	Version         string
	EffectiveAt     time.Time
	FX              FXSettings
	Models          []PriceModel
	Tools           []PriceTool
	SettingsVersion string
	Hash            string
}

// normalizeModelName folds the dot/hyphen separator differences between a
// product manifest (`claude-sonnet-5-5`) and the price book
// (`claude-sonnet-5.5`) so a naming mismatch cannot surface as unknown_model.
func normalizeModelName(name string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), ".", "-")
}

// Model returns the schedule for one model name. An exact match wins; a
// dot↔hyphen-equivalent match is the fallback (case-insensitively).
func (b PriceBook) Model(name string) (PriceModel, bool) {
	name = strings.TrimSpace(name)
	for _, m := range b.Models {
		if m.Model == name {
			return m, true
		}
	}
	normalized := normalizeModelName(name)
	for _, m := range b.Models {
		if normalizeModelName(m.Model) == normalized {
			return m, true
		}
	}
	return PriceModel{}, false
}

// TierFor picks the smallest non-zero band whose max_input covers the input
// token count; MaxInput == 0 is the unbounded fallback.
func (m PriceModel) TierFor(inputTokens int64) (PriceTier, bool) {
	var best *PriceTier
	var unbounded *PriceTier
	for i := range m.Tiers {
		tier := &m.Tiers[i]
		if tier.MaxInput == 0 {
			unbounded = tier
			continue
		}
		if tier.MaxInput >= inputTokens && (best == nil || tier.MaxInput < best.MaxInput) {
			best = tier
		}
	}
	if best != nil {
		return *best, true
	}
	if unbounded != nil {
		return *unbounded, true
	}
	return PriceTier{}, false
}

// PeakMultiplierBPS returns the multiplier for started_at, or 10000 (no
// change). Overlapping windows take the largest multiplier so a misconfigured
// overlap cannot silently undercharge.
func (m PriceModel) PeakMultiplierBPS(startedAt time.Time) int64 {
	if startedAt.IsZero() {
		return 10000
	}
	best := int64(10000)
	for _, window := range m.Peak {
		if !window.matches(startedAt) {
			continue
		}
		if window.MultiplierBPS > best {
			best = window.MultiplierBPS
		}
	}
	return best
}

func (w PeakWindow) matches(at time.Time) bool {
	loc, err := time.LoadLocation(w.Timezone)
	if err != nil || loc == nil {
		// A broken window must not overcharge; skip it rather than guess.
		return false
	}
	local := at.In(loc)
	weekday := int(local.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	matched := false
	for _, day := range w.Weekdays {
		if day == weekday {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	minute := local.Hour()*60 + local.Minute()
	if w.StartMinute <= w.EndMinute {
		return minute >= w.StartMinute && minute < w.EndMinute
	}
	// Overnight window, e.g. 22:00 -> 02:00.
	return minute >= w.StartMinute || minute < w.EndMinute
}

// Public price-book view (contract section 2.1). Prices are plain retail
// numbers and no cost column appears anywhere in this projection.
type PriceBookView struct {
	Version         string           `json:"version"`
	Hash            string           `json:"hash"`
	EffectiveAt     string           `json:"effective_at"`
	FX              FXSettings       `json:"fx"`
	Models          []PriceModelView `json:"models"`
	Tools           []PriceToolView  `json:"tools"`
	SettingsVersion string           `json:"settings_version,omitempty"`
}

type PriceModelView struct {
	Model    string           `json:"model"`
	Currency string           `json:"currency"`
	Tiers    []PriceTierView  `json:"tiers"`
	Peak     []PeakWindowView `json:"peak"`
}

type PriceTierView struct {
	MaxInput int64            `json:"max_input"`
	Prices   map[string]int64 `json:"prices"`
}

type PeakWindowView struct {
	Weekdays      []int  `json:"weekdays"`
	StartMinute   int    `json:"start_minute"`
	EndMinute     int    `json:"end_minute"`
	Timezone      string `json:"timezone"`
	MultiplierBPS int64  `json:"multiplier_bps"`
}

type PriceToolView struct {
	Tool       string `json:"tool"`
	Unit       string `json:"unit"`
	PriceMinor int64  `json:"price_minor"`
}

// Internal price-book view (contract section 2.2): every component also carries
// its CNY cost so the relay and the margin dashboard can use one book.
type InternalPriceBookView struct {
	Version         string              `json:"version"`
	Hash            string              `json:"hash"`
	EffectiveAt     string              `json:"effective_at"`
	FX              FXSettings          `json:"fx"`
	Models          []InternalModelView `json:"models"`
	Tools           []PriceToolView     `json:"tools"`
	SettingsVersion string              `json:"settings_version"`
}

type InternalModelView struct {
	Model    string             `json:"model"`
	Currency string             `json:"currency"`
	Tiers    []InternalTierView `json:"tiers"`
	Peak     []PeakWindowView   `json:"peak"`
}

type InternalTierView struct {
	MaxInput int64                            `json:"max_input"`
	Prices   map[string]InternalComponentView `json:"prices"`
}

type InternalComponentView struct {
	RetailMinorPerMillion int64  `json:"retail_minor_per_million"`
	CostMinorPerMillion   int64  `json:"cost_minor_per_million"`
	CostCurrency          string `json:"cost_currency"`
}

func (b PriceBook) effectiveAtString() string {
	if b.EffectiveAt.IsZero() {
		return ""
	}
	return b.EffectiveAt.In(cstZone).Format(time.RFC3339)
}

func (b PriceBook) jsonModels() []PriceModelView {
	out := make([]PriceModelView, 0, len(b.Models))
	for _, model := range b.Models {
		tiers := make([]PriceTierView, 0, len(model.Tiers))
		for _, tier := range model.Tiers {
			prices := make(map[string]int64, len(PriceComponents))
			for _, component := range PriceComponents {
				prices[component] = tier.Price(component).RetailMinorPerMillion
			}
			tiers = append(tiers, PriceTierView{MaxInput: tier.MaxInput, Prices: prices})
		}
		peak := make([]PeakWindowView, 0, len(model.Peak))
		for _, window := range model.Peak {
			peak = append(peak, PeakWindowView{
				Weekdays:      append([]int(nil), window.Weekdays...),
				StartMinute:   window.StartMinute,
				EndMinute:     window.EndMinute,
				Timezone:      window.Timezone,
				MultiplierBPS: window.MultiplierBPS,
			})
		}
		out = append(out, PriceModelView{
			Model:    model.Model,
			Currency: model.Currency,
			Tiers:    tiers,
			Peak:     peak,
		})
	}
	return out
}

func (b PriceBook) jsonTools() []PriceToolView {
	out := make([]PriceToolView, 0, len(b.Tools))
	for _, tool := range b.Tools {
		out = append(out, PriceToolView{Tool: tool.Tool, Unit: tool.Unit, PriceMinor: tool.PriceMinor})
	}
	return out
}

// PublicView is the unauthenticated projection: retail only.
func (b PriceBook) PublicView() PriceBookView {
	return PriceBookView{
		Version:     b.Version,
		Hash:        b.Hash,
		EffectiveAt: b.effectiveAtString(),
		FX:          b.FX,
		Models:      b.jsonModels(),
		Tools:       b.jsonTools(),
	}
}

// InternalView is the service-authenticated projection: retail plus cost.
func (b PriceBook) InternalView() InternalPriceBookView {
	models := make([]InternalModelView, 0, len(b.Models))
	for _, model := range b.Models {
		tiers := make([]InternalTierView, 0, len(model.Tiers))
		for _, tier := range model.Tiers {
			prices := make(map[string]InternalComponentView, len(PriceComponents))
			for _, component := range PriceComponents {
				price := tier.Price(component)
				prices[component] = InternalComponentView{
					RetailMinorPerMillion: price.RetailMinorPerMillion,
					CostMinorPerMillion:   price.CostMinorPerMillion,
					CostCurrency:          price.CostCurrency,
				}
			}
			tiers = append(tiers, InternalTierView{MaxInput: tier.MaxInput, Prices: prices})
		}
		peak := make([]PeakWindowView, 0, len(model.Peak))
		for _, window := range model.Peak {
			peak = append(peak, PeakWindowView{
				Weekdays:      append([]int(nil), window.Weekdays...),
				StartMinute:   window.StartMinute,
				EndMinute:     window.EndMinute,
				Timezone:      window.Timezone,
				MultiplierBPS: window.MultiplierBPS,
			})
		}
		models = append(models, InternalModelView{
			Model:    model.Model,
			Currency: model.Currency,
			Tiers:    tiers,
			Peak:     peak,
		})
	}
	return InternalPriceBookView{
		Version:         b.Version,
		Hash:            b.Hash,
		EffectiveAt:     b.effectiveAtString(),
		FX:              b.FX,
		Models:          models,
		Tools:           b.jsonTools(),
		SettingsVersion: b.SettingsVersion,
	}
}

// Finalize sorts the book into a canonical order and computes the content
// hash. The hash covers the public projection only, so the page hash, the
// public response hash and the internal (cost-bearing) response hash are all
// comparable in CI. The serialization is sorted-key canonical JSON (see
// canonicalJSON), matching roost-website/scripts/generate-prices.mjs.
func (b *PriceBook) Finalize() {
	sort.SliceStable(b.Models, func(i, j int) bool { return b.Models[i].Model < b.Models[j].Model })
	for i := range b.Models {
		model := &b.Models[i]
		if model.Currency == "" {
			model.Currency = "CREDIT"
		}
		sort.SliceStable(model.Tiers, func(a, c int) bool {
			left, right := model.Tiers[a], model.Tiers[c]
			// Unbounded tiers (0) sort last so a bounded band always wins.
			if (left.MaxInput == 0) != (right.MaxInput == 0) {
				return left.MaxInput != 0
			}
			return left.MaxInput < right.MaxInput
		})
		for t := range model.Tiers {
			if model.Tiers[t].Prices == nil {
				model.Tiers[t].Prices = map[string]ComponentPrice{}
			}
			for _, component := range PriceComponents {
				price := model.Tiers[t].Price(component)
				if price.CostCurrency == "" {
					price.CostCurrency = "CNY"
				}
				model.Tiers[t].Prices[component] = price
			}
		}
		sort.SliceStable(model.Peak, func(a, c int) bool {
			left, right := model.Peak[a], model.Peak[c]
			if left.StartMinute != right.StartMinute {
				return left.StartMinute < right.StartMinute
			}
			return left.EndMinute < right.EndMinute
		})
		for p := range model.Peak {
			days := append([]int(nil), model.Peak[p].Weekdays...)
			sort.Ints(days)
			model.Peak[p].Weekdays = days
		}
	}
	sort.SliceStable(b.Tools, func(i, j int) bool { return b.Tools[i].Tool < b.Tools[j].Tool })
	b.Hash = b.computeHash()
}

func (b PriceBook) computeHash() string {
	payload := struct {
		Version     string           `json:"version"`
		EffectiveAt string           `json:"effective_at"`
		FX          FXSettings       `json:"fx"`
		Models      []PriceModelView `json:"models"`
		Tools       []PriceToolView  `json:"tools"`
	}{
		Version:     b.Version,
		EffectiveAt: b.effectiveAtString(),
		FX:          b.FX,
		Models:      b.jsonModels(),
		Tools:       b.jsonTools(),
	}
	// The hash covers the same canonical bytes the website generator hashes
	// (sorted keys at every level), so page hash == API ETag == book hash.
	data, err := canonicalJSON(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PriceBook loads the price book effective at `at` (zero means now), together
// with the current settings version and the tool prices derived from settings.
func (s *Store) PriceBook(ctx context.Context, at time.Time) (PriceBook, error) {
	return loadPriceBook(ctx, s.Pool, at, "")
}

// PriceBookForVersion loads one immutable version. Settlements use this so a
// price change between reserve and settle cannot silently reprice a request.
func (s *Store) PriceBookForVersion(ctx context.Context, version string) (PriceBook, error) {
	if strings.TrimSpace(version) == "" {
		return PriceBook{}, ErrPriceBookNotFound
	}
	return loadPriceBook(ctx, s.Pool, time.Time{}, version)
}

// PriceBookVersion returns the version effective at `at` without loading the
// whole book; reservations use it to lock a price version.
func (s *Store) PriceBookVersion(ctx context.Context, at time.Time) (string, error) {
	return currentPriceVersion(ctx, s.Pool, at)
}

func currentPriceVersion(ctx context.Context, q settingsQuerier, at time.Time) (string, error) {
	if at.IsZero() {
		at = time.Now()
	}
	var version string
	err := q.QueryRow(ctx, `
		select version from billing.price_book
		where effective_from <= $1
		order by effective_from desc, version desc
		limit 1`, at.UTC()).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPriceBookNotFound
	}
	if err != nil {
		return "", err
	}
	return version, nil
}

func loadPriceBook(ctx context.Context, q settingsQuerier, at time.Time, version string) (PriceBook, error) {
	settings, err := loadSettings(ctx, q)
	if err != nil {
		return PriceBook{}, err
	}
	var effectiveFrom time.Time
	if version == "" {
		if at.IsZero() {
			at = time.Now()
		}
		err = q.QueryRow(ctx, `
			select version, effective_from from billing.price_book
			where effective_from <= $1
			order by effective_from desc, version desc
			limit 1`, at.UTC()).Scan(&version, &effectiveFrom)
	} else {
		err = q.QueryRow(ctx, `
			select version, effective_from from billing.price_book
			where version=$1
			order by effective_from
			limit 1`, version).Scan(&version, &effectiveFrom)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return PriceBook{}, ErrPriceBookNotFound
	}
	if err != nil {
		return PriceBook{}, err
	}

	rows, err := q.Query(ctx, `
		select model, currency, component, tier_max_input,
		       retail_minor_per_million, cost_minor_per_million, cost_currency
		from billing.price_book
		where version=$1
		order by model, tier_max_input, component`, version)
	if err != nil {
		return PriceBook{}, err
	}
	defer rows.Close()
	modelIndex := map[string]int{}
	models := make([]PriceModel, 0, 8)
	for rows.Next() {
		var (
			model, currency, component, costCurrency string
			tierMax, retail, cost                    int64
		)
		if err := rows.Scan(&model, &currency, &component, &tierMax, &retail, &cost, &costCurrency); err != nil {
			return PriceBook{}, err
		}
		idx, ok := modelIndex[model]
		if !ok {
			idx = len(models)
			modelIndex[model] = idx
			models = append(models, PriceModel{Model: model, Currency: currency})
		}
		m := &models[idx]
		if m.Currency == "" {
			m.Currency = currency
		}
		var tier *PriceTier
		for i := range m.Tiers {
			if m.Tiers[i].MaxInput == tierMax {
				tier = &m.Tiers[i]
				break
			}
		}
		if tier == nil {
			m.Tiers = append(m.Tiers, PriceTier{MaxInput: tierMax, Prices: map[string]ComponentPrice{}})
			tier = &m.Tiers[len(m.Tiers)-1]
		}
		tier.Prices[component] = ComponentPrice{
			RetailMinorPerMillion: retail,
			CostMinorPerMillion:   cost,
			CostCurrency:          costCurrency,
		}
	}
	if err := rows.Err(); err != nil {
		return PriceBook{}, err
	}
	rows.Close()

	peakRows, err := q.Query(ctx, `
		select model, weekdays::int[], start_minute, end_minute, timezone, multiplier_bps
		from billing.price_book_peak
		where version=$1
		order by model, start_minute, end_minute`, version)
	if err != nil {
		return PriceBook{}, err
	}
	defer peakRows.Close()
	for peakRows.Next() {
		var (
			model, timezone string
			weekdays        []int32
			start, end      int32
			multiplier      int64
		)
		if err := peakRows.Scan(&model, &weekdays, &start, &end, &timezone, &multiplier); err != nil {
			return PriceBook{}, err
		}
		idx, ok := modelIndex[model]
		if !ok {
			idx = len(models)
			modelIndex[model] = idx
			models = append(models, PriceModel{Model: model, Currency: "CREDIT"})
		}
		days := make([]int, 0, len(weekdays))
		for _, day := range weekdays {
			days = append(days, int(day))
		}
		models[idx].Peak = append(models[idx].Peak, PeakWindow{
			Weekdays:      days,
			StartMinute:   int(start),
			EndMinute:     int(end),
			Timezone:      timezone,
			MultiplierBPS: multiplier,
		})
	}
	if err := peakRows.Err(); err != nil {
		return PriceBook{}, err
	}
	peakRows.Close()

	tools := make([]PriceTool, 0, len(settings.Tools))
	for name, price := range settings.Tools {
		tools = append(tools, PriceTool{Tool: name, Unit: "call", PriceMinor: price})
	}
	book := PriceBook{
		Version:     version,
		EffectiveAt: effectiveFrom,
		FX: FXSettings{
			USDToCredits: settings.FXUSDToCredits,
			CreditYuan:   settings.CreditYuan,
		},
		Models:          models,
		Tools:           tools,
		SettingsVersion: settings.Version,
	}
	book.Finalize()
	return book, nil
}
