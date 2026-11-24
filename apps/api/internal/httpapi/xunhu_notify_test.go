package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/payment"
)

const xunhuSecret = "xunhu-merchant-secret"

func xunhuServer(cfgAppID string, store *fakePaidOrders) *Server {
	cfg := testConfig()
	cfg.XunhuAppID = cfgAppID
	cfg.XunhuAppSecret = xunhuSecret
	if cfgAppID == "" {
		cfg.XunhuAppSecret = ""
	}
	cfg.RatePaymentNotify = 1000
	s := &Server{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if store != nil {
		s.paidOrders = store
	}
	return s
}

func xunhuNotifyRequest(t *testing.T, secret string, values url.Values) *http.Request {
	t.Helper()
	form := url.Values{}
	for k, v := range values {
		form.Set(k, v[0])
	}
	form.Set("hash", payment.Sign(form, secret))
	req := httptest.NewRequest(http.MethodPost, "/v1/payments/xunhupay/notify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func serveXunhu(t *testing.T, h http.Handler, secret string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, xunhuNotifyRequest(t, secret, values))
	return rec
}

func xunhuPaidForm(outTradeNo string) url.Values {
	return url.Values{
		"trade_order_id": {outTradeNo},
		"total_fee":      {"100"},
		"status":         {"OD"},
		"transaction_id": {"4200001234"},
	}
}

func TestXunhuNotifyFulfilsItsOwnOrder(t *testing.T) {
	store := &fakePaidOrders{order: billing.Order{ID: "order-1", OutTradeNo: "TRADE1", AmountCents: 10000,
		Credits: 10000, Status: "pending", Provider: payment.ProviderXunhuPay}}
	rec := serveXunhu(t, xunhuServer("appid", store).Handler(), xunhuSecret, xunhuPaidForm("TRADE1"))
	if rec.Code != http.StatusOK || rec.Body.String() != "success" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(store.calls) != 1 || store.calls[0].tradeNo != "4200001234" {
		t.Fatalf("calls=%+v", store.calls)
	}
}

// The regression this guards: before EPUSDT existed, an unconfigured XunhuPay
// had no pending orders to attack. Now an attacker can open a crypto checkout
// (which leaves a pending row) and try to settle it here with a hash computed
// over the empty secret.
func TestXunhuNotifyIsClosedWhenMerchantIsNotConfigured(t *testing.T) {
	store := &fakePaidOrders{order: billing.Order{ID: "order-1", OutTradeNo: "TRADE1", AmountCents: 10000,
		Credits: 10000, Status: "pending", Provider: payment.ProviderEPUSDT}}
	rec := serveXunhu(t, xunhuServer("", store).Handler(), "", xunhuPaidForm("TRADE1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(store.calls) != 0 {
		t.Fatalf("an unconfigured gateway fulfilled a crypto order: %+v", store.calls)
	}
}

func TestXunhuNotifyRejectsAnotherProvidersOrder(t *testing.T) {
	// A correctly signed XunhuPay callback proves nothing about an order that
	// checks out through the crypto gateway.
	store := &fakePaidOrders{order: billing.Order{ID: "order-1", OutTradeNo: "TRADE1", AmountCents: 10000,
		Credits: 10000, Status: "pending", Provider: payment.ProviderEPUSDT, ProviderOrderID: "GW1"}}
	rec := serveXunhu(t, xunhuServer("appid", store).Handler(), xunhuSecret, xunhuPaidForm("TRADE1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(store.calls) != 0 {
		t.Fatalf("calls=%+v", store.calls)
	}
}

func TestXunhuVerifyRejectsEmptySecret(t *testing.T) {
	form := xunhuPaidForm("TRADE1")
	if payment.Verify(form, "") {
		t.Fatal("a hash over the empty secret must never verify")
	}
}
