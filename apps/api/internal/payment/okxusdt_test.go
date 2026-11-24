package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testUSDT() *OKXUSDT {
	return &OKXUSDT{
		Addresses: map[string]string{
			ChainTRC20:   "TXtestAddress",
			ChainERC20:   "0xTestAddress",
			ChainPolygon: "",
		},
		Rate:     7.2,
		OrderTTL: 30 * time.Minute,
	}
}

func TestFormatMicroRoundsToFourDecimals(t *testing.T) {
	cases := []struct {
		micro int64
		want  string
	}{
		{13_888_900, "13.8889"},
		{13_000_000, "13.0000"},
		{1, "0.0000"},
		{50, "0.0001"},
		{9_999_999, "10.0000"},
		{100_000_000, "100.0000"},
	}
	for _, c := range cases {
		if got := FormatMicro(c.micro); got != c.want {
			t.Errorf("FormatMicro(%d) = %q, want %q", c.micro, got, c.want)
		}
	}
}

func TestParseAmountMatchesFormat(t *testing.T) {
	for _, micro := range []int64{1_000_000, 13_888_900, 500_000, 2_500} {
		got, err := ParseAmount(FormatMicro(micro))
		if err != nil {
			t.Fatalf("ParseAmount(%q): %v", FormatMicro(micro), err)
		}
		// FormatMicro keeps four decimals, so the parsed value is the rounded
		// form of the input, not necessarily the input itself.
		if diff := got - micro; diff > 100 || diff < -100 {
			t.Errorf("round trip %d -> %q -> %d drifts too far", micro, FormatMicro(micro), got)
		}
	}
	if _, err := ParseAmount("not-a-number"); err == nil {
		t.Error("ParseAmount accepted garbage")
	}
	if _, err := ParseAmount(""); err == nil {
		t.Error("ParseAmount accepted empty input")
	}
}

func TestQuoteUsesRateAndStaysUnique(t *testing.T) {
	c := testUSDT()
	// ¥100 = 10000 cents; 100/7.2 = 13.888… USDT.
	first, err := c.Quote(ChainTRC20, 10_000, 1)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if first.Amount != "13.8801" {
		t.Fatalf("amount = %q, want 13.8801", first.Amount)
	}
	second, err := c.Quote(ChainTRC20, 10_000, 2)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if second.Amount == first.Amount {
		t.Fatalf("two suffixes produced the same amount %q", first.Amount)
	}
	if second.AmountMicro-first.AmountMicro <= 0 {
		t.Fatalf("suffix did not raise the amount: %d then %d", first.AmountMicro, second.AmountMicro)
	}
	// The quote must be sendable: a wallet cannot send more precision than the
	// four decimals we display.
	if first.AmountMicro%100 != 0 {
		t.Fatalf("quote %d micro is finer than four decimals", first.AmountMicro)
	}
	if first.Address != "TXtestAddress" {
		t.Fatalf("address = %q", first.Address)
	}
	if first.ExpiresAt.Before(time.Now()) {
		t.Fatal("quote already expired")
	}
}

func TestQuoteRejectsUnknownChain(t *testing.T) {
	c := testUSDT()
	if _, err := c.Quote(ChainPolygon, 10_000, 1); err == nil {
		t.Fatal("quoted a chain without an address")
	}
	if _, err := c.Quote("DOGE", 10_000, 1); err == nil {
		t.Fatal("quoted an unknown chain")
	}
	if _, err := c.Quote(ChainTRC20, 0, 1); err == nil {
		t.Fatal("quoted a zero amount")
	}
}

func TestChainsFollowDisplayOrder(t *testing.T) {
	c := testUSDT()
	got := c.Chains()
	if len(got) != 2 || got[0] != ChainTRC20 || got[1] != ChainERC20 {
		t.Fatalf("chains = %v", got)
	}
	if !c.Configured() {
		t.Fatal("channel with addresses and a rate should be configured")
	}
	if (&OKXUSDT{Addresses: map[string]string{ChainTRC20: "x"}}).Configured() {
		t.Fatal("channel without a rate should not be configured")
	}
}

func TestMatchDeposits(t *testing.T) {
	c := testUSDT()
	now := time.Now()
	order := OpenUSDTOrder{
		OrderID:     "order-1",
		Chain:       ChainTRC20,
		AmountMicro: 13_880_100,
		CreatedAt:   now.Add(-10 * time.Minute),
		ExpiresAt:   now.Add(20 * time.Minute),
	}

	ok := []Deposit{{
		DepositID:   "dep-1",
		Chain:       ChainTRC20,
		AmountMicro: 13_880_100,
		At:          now.Add(-5 * time.Minute),
	}}
	if got := c.MatchDeposits([]OpenUSDTOrder{order}, ok); len(got) != 1 || got[0].OrderID != "order-1" {
		t.Fatalf("exact deposit did not match: %+v", got)
	}

	wrongChain := []Deposit{{DepositID: "dep-2", Chain: ChainERC20, AmountMicro: 13_880_100, At: now}}
	if got := c.MatchDeposits([]OpenUSDTOrder{order}, wrongChain); len(got) != 0 {
		t.Fatalf("matched a deposit on another chain: %+v", got)
	}

	wrongAmount := []Deposit{{DepositID: "dep-3", Chain: ChainTRC20, AmountMicro: 13_880_000, At: now}}
	if got := c.MatchDeposits([]OpenUSDTOrder{order}, wrongAmount); len(got) != 0 {
		t.Fatalf("matched a deposit with the wrong amount: %+v", got)
	}

	tooEarly := []Deposit{{DepositID: "dep-4", Chain: ChainTRC20, AmountMicro: 13_880_100, At: now.Add(-30 * time.Minute)}}
	if got := c.MatchDeposits([]OpenUSDTOrder{order}, tooEarly); len(got) != 0 {
		t.Fatalf("matched a deposit from before the order existed: %+v", got)
	}

	tooLate := []Deposit{{DepositID: "dep-5", Chain: ChainTRC20, AmountMicro: 13_880_100, At: now.Add(48 * time.Hour)}}
	if got := c.MatchDeposits([]OpenUSDTOrder{order}, tooLate); len(got) != 0 {
		t.Fatalf("matched a deposit after the grace window: %+v", got)
	}
}

func TestMatchDepositsSettlesEachSideOnce(t *testing.T) {
	c := testUSDT()
	now := time.Now()
	orders := []OpenUSDTOrder{
		{OrderID: "a", Chain: ChainTRC20, AmountMicro: 1_000_000, CreatedAt: now.Add(-20 * time.Minute), ExpiresAt: now.Add(10 * time.Minute)},
		{OrderID: "b", Chain: ChainTRC20, AmountMicro: 1_000_000, CreatedAt: now.Add(-10 * time.Minute), ExpiresAt: now.Add(20 * time.Minute)},
	}
	// One deposit for two identical orders: only the older order may claim it.
	deposits := []Deposit{{DepositID: "d1", Chain: ChainTRC20, AmountMicro: 1_000_000, At: now.Add(-5 * time.Minute)}}
	got := c.MatchDeposits(orders, deposits)
	if len(got) != 1 || got[0].OrderID != "a" {
		t.Fatalf("expected the oldest order to take the deposit, got %+v", got)
	}

	// The same deposit must not settle a second order on the next tick.
	again := c.MatchDeposits(orders[1:], deposits)
	if len(again) != 1 {
		t.Fatalf("a later order should still match its own deposit, got %+v", again)
	}
}

func TestOKXSignMatchesKnownVector(t *testing.T) {
	got := OKXSign("secret", "2020-12-08T09:08:57.715Z", http.MethodGet, "/api/v5/asset/deposit-history?ccy=USDT", "")
	want := "bPJXMGBJrV2f/dwfPA/x6coPl6evx1ftIrGVzOJFTAU="
	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestOKXClientDepositsParsesAndFilters(t *testing.T) {
	var gotPath, gotKey, gotSign, gotPass string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotKey = r.Header.Get("OK-ACCESS-KEY")
		gotSign = r.Header.Get("OK-ACCESS-SIGN")
		gotPass = r.Header.Get("OK-ACCESS-PASSPHRASE")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0",
			"data": []map[string]any{
				{"depId": "1", "chain": "USDT-TRC20", "amt": "13.8801", "ts": "1764000000000", "txId": "0xabc", "state": "2"},
				{"depId": "2", "chain": "USDT-ERC20", "amt": "5", "ts": "1764000000000", "txId": "0xdef", "state": "2"},
				// Not credited yet: must be ignored.
				{"depId": "3", "chain": "USDT-TRC20", "amt": "1", "ts": "1764000000000", "txId": "0xghi", "state": "1"},
				// A chain we do not accept.
				{"depId": "4", "chain": "USDT-BTC", "amt": "1", "ts": "1764000000000", "txId": "0xjkl", "state": "2"},
			},
		})
	}))
	defer server.Close()

	client := &OKXClient{
		BaseURL:    server.URL,
		Key:        "key",
		Secret:     "secret",
		Passphrase: "pass",
		ChainCodes: map[string]string{
			ChainTRC20:   "TRC20",
			ChainERC20:   "ERC20",
			ChainPolygon: "Polygon",
		},
		Now: func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) },
	}
	if !client.Configured() {
		t.Fatal("client with credentials and https/loopback base should be configured")
	}
	deposits, err := client.Deposits(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("deposits: %v", err)
	}
	if len(deposits) != 2 {
		t.Fatalf("expected 2 usable deposits, got %+v", deposits)
	}
	if deposits[0].Chain != ChainTRC20 || deposits[0].AmountMicro != 13_880_100 {
		t.Fatalf("first deposit = %+v", deposits[0])
	}
	if deposits[1].Chain != ChainERC20 || deposits[1].AmountMicro != 5_000_000 {
		t.Fatalf("second deposit = %+v", deposits[1])
	}
	if gotKey != "key" || gotPass != "pass" || gotSign == "" {
		t.Fatalf("headers missing: key=%q pass=%q sign=%q", gotKey, gotPass, gotSign)
	}
	if gotPath == "" || gotPath[:len("/api/v5/asset/deposit-history")] != "/api/v5/asset/deposit-history" {
		t.Fatalf("unexpected request path %q", gotPath)
	}
}

func TestOKXClientRejectsBadBase(t *testing.T) {
	for _, base := range []string{"http://example.com", "ftp://x", "", "not a url"} {
		client := &OKXClient{BaseURL: base, Key: "k", Secret: "s", Passphrase: "p"}
		if client.Configured() {
			t.Errorf("base %q should not be accepted", base)
		}
	}
}
