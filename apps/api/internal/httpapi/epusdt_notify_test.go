package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JingxuanC/roost-console/apps/api/internal/auth"
	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/config"
	"github.com/JingxuanC/roost-console/apps/api/internal/payment"
)

const epusdtSecret = "epusdt-merchant-secret"

// fakePaidOrders mirrors the two store rules the callback depends on: an order
// is found by its merchant trade number, and Fulfill refuses a wrong amount.
type fakePaidOrders struct {
	order billing.Order
	calls []fulfillCall
}

func (f *fakePaidOrders) OrderByTradeNo(_ context.Context, tradeNo string) (billing.Order, error) {
	if tradeNo != f.order.OutTradeNo {
		return billing.Order{}, billing.ErrNotFound
	}
	return f.order, nil
}

func (f *fakePaidOrders) Fulfill(_ context.Context, orderID, tradeNo string, amountCents int, raw []byte) error {
	if amountCents != f.order.AmountCents {
		return fmt.Errorf("payment amount mismatch: got %d want %d", amountCents, f.order.AmountCents)
	}
	f.calls = append(f.calls, fulfillCall{orderID: orderID, tradeNo: tradeNo, amount: amountCents, raw: string(raw)})
	return nil
}

func epusdtConfig() config.Config {
	cfg := testConfig()
	cfg.EPUSDTAPIBase = "http://127.0.0.1:8000"
	cfg.EPUSDTPID = "1000"
	cfg.EPUSDTSecretKey = epusdtSecret
	cfg.EPUSDTNotifyURL = "https://www.roostbots.com/console-api/v1/payments/epusdt/notify"
	cfg.RatePaymentNotify = 1000
	return cfg
}

func epusdtServer(cfg config.Config, store *fakePaidOrders) *Server {
	s := &Server{
		cfg: cfg,
		epusdt: &payment.EPUSDT{
			APIBase: cfg.EPUSDTAPIBase, PID: cfg.EPUSDTPID, SecretKey: cfg.EPUSDTSecretKey, NotifyURL: cfg.EPUSDTNotifyURL,
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if store != nil {
		s.paidOrders = store
	}
	return s
}

func epusdtOrder() billing.Order {
	return billing.Order{ID: "order-1", OrgID: "org-1", OutTradeNo: "TRADE1", SKU: "credit_100",
		AmountCents: 10000, Credits: 10000, Status: "pending", Provider: payment.ProviderEPUSDT, ProviderOrderID: "GW1"}
}

func epusdtCallback(t *testing.T, secret string, mutate func(map[string]any)) string {
	t.Helper()
	params := map[string]any{
		"pid": "1000", "trade_id": "GW1", "order_id": "TRADE1",
		"amount": 100, "actual_amount": 14.71, "receive_address": "TXYZ",
		"token": "USDT", "network": "tron", "block_transaction_id": "0xhash", "status": 2,
	}
	if mutate != nil {
		mutate(params)
	}
	sig, err := payment.EPUSDTSign(params, secret)
	if err != nil {
		t.Fatal(err)
	}
	params["signature"] = sig
	body, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func postEPUSDTNotify(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/payments/epusdt/notify", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEPUSDTNotifyFulfillsPaidOrder(t *testing.T) {
	store := &fakePaidOrders{order: epusdtOrder()}
	body := epusdtCallback(t, epusdtSecret, nil)
	rec := postEPUSDTNotify(epusdtServer(epusdtConfig(), store).Handler(), body)
	// The gateway only stops retrying when it reads "ok" or "success".
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(store.calls) != 1 {
		t.Fatalf("calls=%v", store.calls)
	}
	call := store.calls[0]
	// Keyed by the gateway trade id (the same value reconciliation uses); the
	// chain hash is kept in the stored callback body.
	if call.orderID != "order-1" || call.tradeNo != "epusdt_GW1" || call.amount != 10000 || call.raw != body {
		t.Fatalf("call=%+v", call)
	}
	if !strings.Contains(call.raw, `"block_transaction_id":"0xhash"`) {
		t.Fatalf("raw=%s", call.raw)
	}
}

func TestEPUSDTNotifyAcceptsOrphanedCheckout(t *testing.T) {
	// The gateway created a checkout but the trade id never reached the row
	// (create timed out, attach failed). The payment is still real.
	order := epusdtOrder()
	order.ProviderOrderID = ""
	store := &fakePaidOrders{order: order}
	body := epusdtCallback(t, epusdtSecret, nil)
	rec := postEPUSDTNotify(epusdtServer(epusdtConfig(), store).Handler(), body)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(store.calls) != 1 || store.calls[0].tradeNo != "epusdt_GW1" {
		t.Fatalf("calls=%+v", store.calls)
	}
}

func TestEPUSDTNotifyRejectsWithoutFulfilling(t *testing.T) {
	xunhuOrder := epusdtOrder()
	xunhuOrder.Provider = payment.ProviderXunhuPay
	cases := map[string]struct {
		order billing.Order
		body  string
		want  int
	}{
		"wrong secret":         {epusdtOrder(), epusdtCallback(t, "attacker-secret", nil), http.StatusBadRequest},
		"unsigned":             {epusdtOrder(), `{"pid":"1000","order_id":"TRADE1","amount":100,"status":2}`, http.StatusBadRequest},
		"not json":             {epusdtOrder(), `order_id=TRADE1&status=2`, http.StatusBadRequest},
		"unknown order":        {epusdtOrder(), epusdtCallback(t, epusdtSecret, func(p map[string]any) { p["order_id"] = "NOPE" }), http.StatusNotFound},
		"other checkout":       {epusdtOrder(), epusdtCallback(t, epusdtSecret, func(p map[string]any) { p["trade_id"] = "GW2" }), http.StatusNotFound},
		"other provider":       {xunhuOrder, epusdtCallback(t, epusdtSecret, nil), http.StatusNotFound},
		"underpaid amount":     {epusdtOrder(), epusdtCallback(t, epusdtSecret, func(p map[string]any) { p["amount"] = 1 }), http.StatusInternalServerError},
		"amount in wrong unit": {epusdtOrder(), epusdtCallback(t, epusdtSecret, func(p map[string]any) { p["amount"] = 10000 }), http.StatusInternalServerError},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakePaidOrders{order: tc.order}
			rec := postEPUSDTNotify(epusdtServer(epusdtConfig(), store).Handler(), tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%q, want %d", rec.Code, rec.Body.String(), tc.want)
			}
			if len(store.calls) != 0 {
				t.Fatalf("order was fulfilled: %+v", store.calls)
			}
			if body := strings.ToLower(strings.TrimSpace(rec.Body.String())); body == "ok" || body == "success" {
				t.Fatalf("rejection must not read as an acknowledgement: %q", body)
			}
		})
	}
}

func TestEPUSDTNotifyAcknowledgesUnpaidStatusWithoutFulfilling(t *testing.T) {
	store := &fakePaidOrders{order: epusdtOrder()}
	body := epusdtCallback(t, epusdtSecret, func(p map[string]any) { p["status"] = 3 })
	rec := postEPUSDTNotify(epusdtServer(epusdtConfig(), store).Handler(), body)
	if rec.Code != http.StatusOK || len(store.calls) != 0 {
		t.Fatalf("status=%d calls=%+v", rec.Code, store.calls)
	}
}

func TestEPUSDTNotifyIsClosedWhenGatewayIsNotConfigured(t *testing.T) {
	store := &fakePaidOrders{order: epusdtOrder()}
	// A callback signed with an empty key must not be accepted by an
	// unconfigured deployment.
	rec := postEPUSDTNotify(epusdtServer(testConfig(), store).Handler(), epusdtCallback(t, "", nil))
	if rec.Code != http.StatusNotFound || len(store.calls) != 0 {
		t.Fatalf("status=%d calls=%+v", rec.Code, store.calls)
	}
}

func TestCheckoutProviderEnforcesTheAllowlist(t *testing.T) {
	open := epusdtServer(epusdtConfig(), nil)
	restricted := epusdtConfig()
	restricted.EPUSDTAllowEmails = []string{"Owner@Example.com"}
	limited := epusdtServer(restricted, nil)
	off := epusdtServer(testConfig(), nil)

	cases := []struct {
		name      string
		s         *Server
		requested string
		email     string
		want      string
		ok        bool
	}{
		{"default is xunhupay", open, "", "a@example.com", payment.ProviderXunhuPay, true},
		{"explicit xunhupay", open, "XunhuPay", "a@example.com", payment.ProviderXunhuPay, true},
		{"epusdt open to all", open, "epusdt", "a@example.com", payment.ProviderEPUSDT, true},
		{"allowlisted account", limited, "epusdt", "owner@example.com", payment.ProviderEPUSDT, true},
		{"account off the allowlist", limited, "epusdt", "other@example.com", "", false},
		{"allowlist with no email", limited, "epusdt", "", "", false},
		{"gateway not configured", off, "epusdt", "owner@example.com", "", false},
		{"gateway address rejected", epusdtServer(func() config.Config {
			bad := epusdtConfig()
			bad.EPUSDTAPIBase = "http://pay.example.com"
			return bad
		}(), nil), "epusdt", "owner@example.com", "", false},
		{"unknown provider", open, "paypal", "a@example.com", "", false},
	}
	for _, tc := range cases {
		got, ok := tc.s.checkoutProvider(tc.requested, tc.email)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPaymentMethodsOnlyListWhatTheAccountMayUse(t *testing.T) {
	restricted := epusdtConfig()
	restricted.XunhuAppID = "appid"
	restricted.EPUSDTAllowEmails = []string{"owner@example.com"}
	s := epusdtServer(restricted, nil)

	got := s.paymentMethods(&auth.User{Email: "owner@example.com"})
	if len(got) != 2 || got[0] != payment.ProviderXunhuPay || got[1] != payment.ProviderEPUSDT {
		t.Fatalf("allowlisted methods = %v", got)
	}
	got = s.paymentMethods(&auth.User{Email: "other@example.com"})
	if len(got) != 1 || got[0] != payment.ProviderXunhuPay {
		t.Fatalf("other account methods = %v", got)
	}
}
