package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateFeedRejectsOutOfBandPrices(t *testing.T) {
	feed := &RateFeed{Fetch: func(context.Context) (float64, error) { return 99, nil }, Min: 5, Max: 12}
	if err := feed.Refresh(context.Background()); err == nil {
		t.Fatal("a price far above the ceiling must be rejected")
	}
	if _, _, ok := feed.Current(); ok {
		t.Fatal("a rejected price must not be stored")
	}
	feed = &RateFeed{Fetch: func(context.Context) (float64, error) { return 7.21, nil }, Min: 5, Max: 12}
	if err := feed.Refresh(context.Background()); err != nil {
		t.Fatalf("in-band price rejected: %v", err)
	}
	rate, at, ok := feed.Current()
	if !ok || rate != 7.21 || at.IsZero() {
		t.Fatalf("current = %v %v %v", rate, at, ok)
	}
}

func TestRateFeedKeepsLastGoodValue(t *testing.T) {
	calls := 0
	feed := &RateFeed{Fetch: func(context.Context) (float64, error) {
		calls++
		if calls == 1 {
			return 7.3, nil
		}
		return 0, errors.New("market unavailable")
	}}
	if err := feed.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := feed.Refresh(context.Background()); err == nil {
		t.Fatal("expected the second refresh to fail")
	}
	rate, _, ok := feed.Current()
	if !ok || rate != 7.3 {
		t.Fatalf("last good value was lost: %v %v", rate, ok)
	}
}

func TestRateFeedSeedAndStaleness(t *testing.T) {
	feed := &RateFeed{
		Fetch: func(context.Context) (float64, error) { return 0, errors.New("down") },
		Seed:  7.15,
		TTL:   time.Hour,
	}
	_ = feed.Refresh(context.Background())
	rate, at, ok := feed.Current()
	if !ok || rate != 7.15 {
		t.Fatalf("seed should keep the channel priced: %v %v", rate, ok)
	}
	if !at.IsZero() {
		t.Fatalf("seed must not claim a fetch time, got %v", at)
	}
	if feed.Stale() {
		t.Fatal("a value with no timestamp cannot be judged stale")
	}
	_ = feed.set(7.2, time.Now().Add(-2*time.Hour))
	if !feed.Stale() {
		t.Fatal("two-hour-old value should be stale under a one-hour TTL")
	}
}

func TestChannelQuotesWithLiveRate(t *testing.T) {
	feed := &RateFeed{Fetch: func(context.Context) (float64, error) { return 8, nil }}
	_ = feed.Refresh(context.Background())
	channel := &OKXUSDT{
		Addresses: map[string]string{ChainTRC20: "Taddr"},
		Rate:      7.0, // static fallback must lose to the live feed
		Rates:     feed,
	}
	quote, err := channel.Quote(ChainTRC20, 10_000, 1) // ¥100
	if err != nil {
		t.Fatal(err)
	}
	if quote.Amount != "12.5001" {
		t.Fatalf("amount = %q, want 12.5001 (100/8 + suffix)", quote.Amount)
	}
	if quote.Rate != 8 {
		t.Fatalf("quote rate = %v, want the live 8", quote.Rate)
	}
}

func TestChannelFallsBackToStaticRate(t *testing.T) {
	feed := &RateFeed{Fetch: func(context.Context) (float64, error) { return 0, errors.New("down") }}
	_ = feed.Refresh(context.Background())
	channel := &OKXUSDT{
		Addresses: map[string]string{ChainTRC20: "Taddr"},
		Rate:      7.2,
		Rates:     feed,
	}
	quote, err := channel.Quote(ChainTRC20, 10_000, 1)
	if err != nil {
		t.Fatalf("static fallback should still quote: %v", err)
	}
	if quote.Rate != 7.2 {
		t.Fatalf("rate = %v, want the static 7.2", quote.Rate)
	}
}

func TestChannelUnconfiguredWithoutAnyRate(t *testing.T) {
	channel := &OKXUSDT{Addresses: map[string]string{ChainTRC20: "Taddr"}}
	if channel.Configured() {
		t.Fatal("a channel with no rate source must not be configured")
	}
}

func TestFetchOKXUSDTCNY(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v5/market/ticker" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("instId"); got != "USDT-CNY" {
			t.Errorf("instId = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0",
			"data": []map[string]string{{"instId": "USDT-CNY", "last": "7.2143"}},
		})
	}))
	defer server.Close()
	rate, err := FetchOKXUSDTCNY(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if rate != 7.2143 {
		t.Fatalf("rate = %v", rate)
	}
}

func TestFetchOKXUSDTCNYErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "51001", "msg": "Instrument ID does not exist"})
	}))
	defer server.Close()
	if _, err := FetchOKXUSDTCNY(context.Background(), server.Client(), server.URL); err == nil {
		t.Fatal("a non-zero API code must be an error")
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "data": []any{}})
	}))
	defer empty.Close()
	if _, err := FetchOKXUSDTCNY(context.Background(), empty.Client(), empty.URL); err == nil {
		t.Fatal("an empty ticker must be an error")
	}
}
