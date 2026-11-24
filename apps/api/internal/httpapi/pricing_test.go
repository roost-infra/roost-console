package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/auth"
	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/config"
)

type fakePriceBookStore struct {
	book  billing.PriceBook
	err   error
	calls int
	ats   []time.Time
}

func (f *fakePriceBookStore) PriceBook(_ context.Context, at time.Time) (billing.PriceBook, error) {
	f.calls++
	f.ats = append(f.ats, at)
	if f.err != nil {
		return billing.PriceBook{}, f.err
	}
	return f.book, nil
}

func testPricingBook() billing.PriceBook {
	book := billing.PriceBook{
		Version:         "2026-11-25.1",
		SettingsVersion: "2026-11-25.1",
		EffectiveAt:     time.Date(2026, 11, 25, 0, 0, 0, 0, time.FixedZone("CST", 8*3600)),
		FX:              billing.FXSettings{USDToCredits: 720, CreditYuan: 0.01},
		Models: []billing.PriceModel{{
			Model:    "claude-sonnet-5.5",
			Currency: "CREDIT",
			Tiers: []billing.PriceTier{{
				MaxInput: 0,
				Prices: map[string]billing.ComponentPrice{
					billing.ComponentInput:      {RetailMinorPerMillion: 1122500, CostMinorPerMillion: 544000, CostCurrency: "CNY"},
					billing.ComponentOutput:     {RetailMinorPerMillion: 5610000, CostMinorPerMillion: 2720000, CostCurrency: "CNY"},
					billing.ComponentCacheRead:  {RetailMinorPerMillion: 57500, CostMinorPerMillion: 27200, CostCurrency: "CNY"},
					billing.ComponentCacheWrite: {RetailMinorPerMillion: 0, CostMinorPerMillion: 680000, CostCurrency: "CNY"},
				},
			}},
			Peak: []billing.PeakWindow{{
				Weekdays: []int{1, 2, 3, 4, 5}, StartMinute: 540, EndMinute: 1080,
				Timezone: "Asia/Shanghai", MultiplierBPS: 12000,
			}},
		}},
		Tools: []billing.PriceTool{{Tool: "search", Unit: "call", PriceMinor: 7}},
	}
	book.Finalize()
	return book
}

func pricingTestServer(store priceBookStore) *Server {
	return &Server{
		cfg: config.Config{ServiceSecrets: map[string][]string{
			"relay":  {"relay-secret"},
			"dslpha": {"dslpha-secret"},
		}},
		pricing: store,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func callPricing(t *testing.T, s *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://example"+path, nil)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodePricingBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v body=%s", err, rec.Body.String())
	}
	return body
}

func TestPublicPricingBook(t *testing.T) {
	store := &fakePriceBookStore{book: testPricingBook()}
	s := pricingTestServer(store)
	rec := callPricing(t, s, "/v1/pricing/book", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodePricingBody(t, rec)
	if body["version"] != "2026-11-25.1" || body["hash"] != store.book.Hash {
		t.Fatalf("version/hash=%v/%v", body["version"], body["hash"])
	}
	if body["effective_at"] != "2026-11-25T00:00:00+08:00" {
		t.Fatalf("effective_at=%v", body["effective_at"])
	}
	fx, _ := body["fx"].(map[string]any)
	if fx["usd_to_credits"] != float64(720) || fx["credit_yuan"] != 0.01 {
		t.Fatalf("fx=%v", fx)
	}
	models, _ := body["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models=%v", body["models"])
	}
	model, _ := models[0].(map[string]any)
	tiers, _ := model["tiers"].([]any)
	tier, _ := tiers[0].(map[string]any)
	prices, _ := tier["prices"].(map[string]any)
	if prices["input"] != float64(1122500) || prices["output"] != float64(5610000) {
		t.Fatalf("prices=%v", prices)
	}
	peak, _ := model["peak"].([]any)
	if len(peak) != 1 {
		t.Fatalf("peak=%v", model["peak"])
	}
	peakWindow, _ := peak[0].(map[string]any)
	if peakWindow["multiplier_bps"] != float64(12000) || peakWindow["timezone"] != "Asia/Shanghai" {
		t.Fatalf("peak window=%v", peakWindow)
	}
	tools, _ := body["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["tool"] != "search" || tool["price_minor"] != float64(7) {
		t.Fatalf("tools=%v", body["tools"])
	}
	// The public projection must not expose the CNY cost column.
	if _, ok := prices["cost_minor_per_million"]; ok {
		t.Fatalf("public prices leak cost: %v", prices)
	}
	if string(rec.Body.Bytes()) == "" {
		t.Fatal("empty body")
	}
	if body := rec.Body.String(); strings.Contains(body, "cost") {
		t.Fatalf("public body leaks cost: %s", body)
	}
	if got, want := rec.Header().Get("ETag"), `"`+store.book.Hash+`"`; got != want {
		t.Fatalf("etag=%q want %q", got, want)
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("cache-control=%q", rec.Header().Get("Cache-Control"))
	}
	if store.calls != 1 {
		t.Fatalf("store calls=%d", store.calls)
	}

	// A second call within the 60-second window is served from the in-process
	// cache without hitting the store.
	again := callPricing(t, s, "/v1/pricing/book", nil)
	if again.Code != http.StatusOK {
		t.Fatalf("second status=%d", again.Code)
	}
	if store.calls != 1 {
		t.Fatalf("cache miss: store calls=%d", store.calls)
	}
}

func TestPublicPricingBookNotModified(t *testing.T) {
	store := &fakePriceBookStore{book: testPricingBook()}
	s := pricingTestServer(store)
	first := callPricing(t, s, "/v1/pricing/book", nil)
	etag := first.Header().Get("ETag")
	second := callPricing(t, s, "/v1/pricing/book", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("status=%d want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 body=%q", second.Body.String())
	}
}

func TestPublicPricingBookExplicitAtBypassesCache(t *testing.T) {
	store := &fakePriceBookStore{book: testPricingBook()}
	s := pricingTestServer(store)
	at := "2026-11-25T00:00:00Z"
	rec := callPricing(t, s, "/v1/pricing/book?at="+at, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.calls != 1 || store.ats[0].IsZero() {
		t.Fatalf("explicit at did not reach the store: calls=%d ats=%v", store.calls, store.ats)
	}
	callPricing(t, s, "/v1/pricing/book?at="+at, nil)
	if store.calls != 2 {
		t.Fatalf("explicit at was cached: calls=%d", store.calls)
	}
}

func TestPublicPricingBookBadInput(t *testing.T) {
	store := &fakePriceBookStore{book: testPricingBook()}
	s := pricingTestServer(store)
	cases := map[string]struct {
		path string
		code string
	}{
		"invalid at": {"/v1/pricing/book?at=not-a-time", "invalid_at"},
		"currency":   {"/v1/pricing/book?currency=USD", "currency_mismatch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := callPricing(t, s, tc.path, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := decodePricingBody(t, rec)
			errBody, _ := body["error"].(map[string]any)
			if errBody["code"] != tc.code {
				t.Fatalf("error=%v", body)
			}
		})
	}
}

func TestPublicPricingBookMissing(t *testing.T) {
	store := &fakePriceBookStore{err: billing.ErrPriceBookNotFound}
	rec := callPricing(t, pricingTestServer(store), "/v1/pricing/book", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func signedPricingRequest(service, uri string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example"+uri, nil)
	ts, nonce, sig := auth.SignService(service, http.MethodGet, uri, nil, service+"-secret", time.Now())
	req.Header.Set("X-Roost-Service", service)
	req.Header.Set("X-Roost-Timestamp", ts)
	req.Header.Set("X-Roost-Nonce", nonce)
	req.Header.Set("X-Roost-Signature", sig)
	return req
}

func TestInternalPricingBookIncludesCost(t *testing.T) {
	store := &fakePriceBookStore{book: testPricingBook()}
	s := pricingTestServer(store)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedPricingRequest("relay", "/internal/v1/pricing/book"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodePricingBody(t, rec)
	if body["settings_version"] != "2026-11-25.1" {
		t.Fatalf("settings_version=%v", body["settings_version"])
	}
	models, _ := body["models"].([]any)
	model, _ := models[0].(map[string]any)
	tiers, _ := model["tiers"].([]any)
	tier, _ := tiers[0].(map[string]any)
	prices, _ := tier["prices"].(map[string]any)
	input, _ := prices["input"].(map[string]any)
	if input["retail_minor_per_million"] != float64(1122500) ||
		input["cost_minor_per_million"] != float64(544000) ||
		input["cost_currency"] != "CNY" {
		t.Fatalf("internal input price=%v", input)
	}
	// Internal and public responses must expose the same content hash.
	if body["hash"] != store.book.Hash {
		t.Fatalf("internal hash=%v want %v", body["hash"], store.book.Hash)
	}
}

func TestInternalPricingBookRequiresServiceAuth(t *testing.T) {
	store := &fakePriceBookStore{book: testPricingBook()}
	s := pricingTestServer(store)
	rec := callPricing(t, s, "/internal/v1/pricing/book", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if store.calls != 0 {
		t.Fatalf("unauthenticated request reached the store: %d", store.calls)
	}
}

func TestInternalPricingBookScope(t *testing.T) {
	for _, service := range []string{"relay", "dslpha", "market", "root"} {
		if !serviceAllows(service, http.MethodGet, "/internal/v1/pricing/book") {
			t.Errorf("service %q is not allowed to read the internal price book", service)
		}
	}
	for _, service := range []string{"dslpha", "market", "root"} {
		if !serviceAllows(service, http.MethodGet, "/internal/v1/billing/settings") {
			t.Errorf("service %q is not allowed to read billing settings", service)
		}
	}
	if serviceAllows("causal", http.MethodGet, "/internal/v1/pricing/book") {
		t.Error("causal service should not read the price book")
	}
}

type fakeSettingsProvider struct {
	values map[string]any
	err    error
}

func (f *fakeSettingsProvider) SettingsFlat(context.Context) (map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.values, nil
}

func TestInternalBillingSettings(t *testing.T) {
	store := &fakeSettingsProvider{values: map[string]any{
		"settings_version":           "2026-11-25.1",
		"fx.usd_to_credits":          int64(720),
		"sandbox.markup_bps":         int64(13000),
		"compression.user_share_bps": int64(2500),
	}}
	s := pricingTestServer(&fakePriceBookStore{book: testPricingBook()})
	s.settings = store
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedPricingRequest("dslpha", "/internal/v1/billing/settings"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodePricingBody(t, rec)
	if body["settings_version"] != "2026-11-25.1" ||
		body["fx.usd_to_credits"] != float64(720) ||
		body["sandbox.markup_bps"] != float64(13000) ||
		body["compression.user_share_bps"] != float64(2500) {
		t.Fatalf("body=%v", body)
	}
}

func TestInternalBillingSettingsAuthAndScope(t *testing.T) {
	store := &fakeSettingsProvider{values: map[string]any{"settings_version": "v"}}
	s := pricingTestServer(&fakePriceBookStore{book: testPricingBook()})
	s.settings = store
	rec := callPricing(t, s, "/internal/v1/billing/settings", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, service := range []string{"dslpha", "market"} {
		if !serviceAllows(service, http.MethodGet, "/internal/v1/billing/settings") {
			t.Errorf("service %q cannot read billing settings", service)
		}
	}
	if serviceAllows("causal", http.MethodGet, "/internal/v1/billing/settings") {
		t.Error("causal service should not read billing settings")
	}
}
