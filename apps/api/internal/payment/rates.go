package payment

// Live USDT/CNY rate used to quote direct-USDT checkouts.
//
// Every order shows an exact USDT amount, so the rate it was computed with is
// money: a stale or absurd quote charges the wrong amount. The feed therefore
// (a) refreshes on a timer, (b) keeps the last good value when a refresh fails,
// (c) rejects prices outside a sanity band, and (d) reports how old the value
// is so callers can treat a very old rate as stale.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateStore is one observed USDT/CNY price.
type RateStore struct {
	Rate float64
	At   time.Time
}

// RateFeed caches the CNY-per-USDT rate between refreshes.
type RateFeed struct {
	// Fetch returns a fresh rate. Required.
	Fetch func(ctx context.Context) (float64, error)
	// Min and Max bound acceptable values; a fetch outside the band is
	// discarded and the previous value kept.
	Min, Max float64
	// TTL marks a value as stale for readers that care; Current still returns
	// it so a brief outage does not blank the checkout.
	TTL time.Duration
	// Seed is used before the first successful fetch.
	Seed float64
	// Log is optional.
	Log *slog.Logger

	mu  sync.RWMutex
	cur RateStore
}

// Current returns the cached rate and when it was observed.
func (f *RateFeed) Current() (float64, time.Time, bool) {
	if f == nil {
		return 0, time.Time{}, false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.cur.Rate <= 0 {
		// The seed keeps checkout priced before the first successful fetch.
		if f.Seed > 0 {
			return f.Seed, time.Time{}, true
		}
		return 0, time.Time{}, false
	}
	return f.cur.Rate, f.cur.At, true
}

// Stale reports whether the cached value is older than TTL.
func (f *RateFeed) Stale() bool {
	if f == nil || f.TTL <= 0 {
		return false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.cur.Rate <= 0 || f.cur.At.IsZero() {
		// Nothing fetched yet: the seed cannot be judged by age.
		return false
	}
	return time.Since(f.cur.At) > f.TTL
}

// set stores a value, applying the sanity band.
func (f *RateFeed) set(rate float64, at time.Time) error {
	if rate <= 0 {
		return errors.New("rate must be positive")
	}
	if f.Min > 0 && rate < f.Min {
		return fmt.Errorf("rate %.4f is below the floor %.4f", rate, f.Min)
	}
	if f.Max > 0 && rate > f.Max {
		return fmt.Errorf("rate %.4f is above the ceiling %.4f", rate, f.Max)
	}
	f.mu.Lock()
	f.cur = RateStore{Rate: rate, At: at}
	f.mu.Unlock()
	return nil
}

// Refresh pulls one rate and stores it. On failure the previous value is kept.
func (f *RateFeed) Refresh(ctx context.Context) error {
	if f == nil || f.Fetch == nil {
		return errors.New("rate feed has no fetcher")
	}
	rate, err := f.Fetch(ctx)
	if err != nil {
		f.logf("usdt rate refresh failed", "err", err)
		return err
	}
	if err := f.set(rate, time.Now()); err != nil {
		f.logf("usdt rate rejected", "err", err)
		return err
	}
	return nil
}

// Run refreshes immediately and then on every interval until ctx ends. The
// first refresh happens before the first sleep, so a fresh process quotes with
// a live rate as soon as the source answers.
func (f *RateFeed) Run(ctx context.Context, interval time.Duration) {
	if f == nil || f.Fetch == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	_ = f.Refresh(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = f.Refresh(ctx)
		}
	}
}

func (f *RateFeed) logf(msg string, args ...any) {
	if f != nil && f.Log != nil {
		f.Log.Warn(msg, args...)
	}
}

// RateFeedForConfig builds the feed used by the direct-USDT channel. It returns
// nil when automatic pricing is off, which leaves the static rate in charge.
func RateFeedForConfig(auto bool, seed, min, max float64, ttl time.Duration, apiBase string) *RateFeed {
	if !auto {
		return nil
	}
	return &RateFeed{
		Fetch: func(ctx context.Context) (float64, error) {
			return FetchOKXUSDTCNY(ctx, nil, apiBase)
		},
		Min:  min,
		Max:  max,
		TTL:  ttl,
		Seed: seed,
	}
}

// OKXTickerURL is the public spot ticker for USDT priced in CNY.
const okxTickerPath = "/api/v5/market/ticker"

// FetchOKXUSDTCNY reads the USDT/CNY last price from OKX's public market API.
// No credentials are involved: this endpoint is unauthenticated.
func FetchOKXUSDTCNY(ctx context.Context, httpClient *http.Client, baseURL string) (float64, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://www.okx.com"
	}
	if httpClient == nil {
		httpClient = okxHTTP
	}
	query := url.Values{}
	query.Set("instId", "USDT-CNY")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+okxTickerPath+"?"+query.Encode(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("okx ticker: http %d: %s", resp.StatusCode, truncate(string(body), 160))
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID string `json:"instId"`
			Last   string `json:"last"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("okx ticker: bad json: %w", err)
	}
	if payload.Code != "0" {
		return 0, fmt.Errorf("okx ticker: %s %s", payload.Code, truncate(payload.Msg, 160))
	}
	if len(payload.Data) == 0 {
		return 0, errors.New("okx ticker: no data")
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(payload.Data[0].Last), 64)
	if err != nil || rate <= 0 {
		return 0, fmt.Errorf("okx ticker: bad price %q", payload.Data[0].Last)
	}
	return rate, nil
}
