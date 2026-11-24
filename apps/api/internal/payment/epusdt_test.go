package payment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const epusdtTestSecret = "merchant-secret"

func epusdtClient(base string) *EPUSDT {
	return &EPUSDT{
		APIBase: base, PID: "1000", SecretKey: epusdtTestSecret,
		NotifyURL: "https://console.example.com/v1/payments/epusdt/notify",
		ReturnURL: "https://console.example.com/console/billing",
		HTTP:      http.DefaultClient,
	}
}

// The vector is the gateway's own fixed test case
// (util/sign TestGetHMACSHA256FixedVector), so this client and the gateway agree
// on the canonical text byte for byte.
func TestEPUSDTSignMatchesGatewayVector(t *testing.T) {
	got, err := EPUSDTSign(map[string]any{
		"signature": "ignored",
		"pid":       "1000",
		"empty":     "",
		"nil":       nil,
		"name":      "VIP",
		"amount":    100,
	}, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	const want = "ced9141fab53a83d1178f903e7c22d8a2a7033520f31e319934e92455a008b6c"
	if got != want {
		t.Fatalf("sign = %s, want %s", got, want)
	}
}

func TestEPUSDTSignRendersNumbersLikeTheGateway(t *testing.T) {
	// A decoded JSON number and a native int must sign identically, and a
	// fractional amount must not pick up float noise or an exponent.
	a, err := EPUSDTSign(map[string]any{"amount": float64(19.99), "callback_version": float64(2)}, "k")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EPUSDTSign(map[string]any{"amount": json.Number("19.99"), "callback_version": 2}, "k")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("float and integer renderings disagree: %s vs %s", a, b)
	}
	if _, err := EPUSDTSign(map[string]any{"nested": map[string]any{}}, "k"); err == nil {
		t.Fatal("nested value must not be signable")
	}
}

// fakeGateway verifies the request the way the gateway does: decode the JSON
// body into a generic map and check the signature over it.
func fakeGateway(t *testing.T, respond func(params map[string]any) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/payments/gmpay/v1/order/create-transaction":
			body, _ := io.ReadAll(r.Body)
			var params map[string]any
			if err := json.Unmarshal(body, &params); err != nil {
				t.Errorf("create body: %v", err)
			}
			want, err := EPUSDTSign(params, epusdtTestSecret)
			if err != nil || params["signature"] != want {
				t.Errorf("create signature = %v, want %s (%v)", params["signature"], want, err)
			}
			status, payload := respond(params)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(payload))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/pay/check-status/"):
			status, payload := respond(map[string]any{"trade_id": strings.TrimPrefix(r.URL.Path, "/pay/check-status/")})
			w.WriteHeader(status)
			_, _ = w.Write([]byte(payload))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEPUSDTCreateSendsPlaceholderOrder(t *testing.T) {
	var seen map[string]any
	srv := fakeGateway(t, func(params map[string]any) (int, string) {
		seen = params
		return http.StatusOK, `{"status_code":200,"message":"success","data":{"trade_id":"T123","order_id":"ORDER1","amount":19.99,"currency":"CNY","actual_amount":0,"receive_address":"","token":"","status":4,"expiration_time":1,"payment_url":"https://pay.example.com/pay/checkout-counter/T123"}}`
	})
	res, err := epusdtClient(srv.URL).Create(context.Background(), CreateInput{OutTradeNo: "ORDER1", AmountCents: 1999, Title: "Roost 自定义充值"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProviderOrderID != "T123" || res.PayURL != "https://pay.example.com/pay/checkout-counter/T123" || res.QRURL != "" {
		t.Fatalf("result = %+v", res)
	}
	if seen["pid"] != "1000" || seen["order_id"] != "ORDER1" || seen["currency"] != "cny" || seen["amount"] != 19.99 ||
		seen["callback_version"] != float64(2) || seen["name"] != "Roost 自定义充值" ||
		seen["notify_url"] != "https://console.example.com/v1/payments/epusdt/notify" ||
		seen["redirect_url"] != "https://console.example.com/console/billing" {
		t.Fatalf("request = %v", seen)
	}
	// Leaving both out is what makes the gateway show its asset picker.
	if _, ok := seen["token"]; ok {
		t.Fatalf("token must be omitted: %v", seen)
	}
	if _, ok := seen["network"]; ok {
		t.Fatalf("network must be omitted: %v", seen)
	}
}

func TestEPUSDTCreateRejectsBadResponses(t *testing.T) {
	cases := map[string]struct {
		status  int
		payload string
	}{
		"gateway error":    {http.StatusBadRequest, `{"status_code":10004,"message":"signature error"}`},
		"repriced amount":  {http.StatusOK, `{"status_code":200,"data":{"trade_id":"T1","order_id":"ORDER1","amount":20,"payment_url":"https://pay.example.com/x"}}`},
		"other order":      {http.StatusOK, `{"status_code":200,"data":{"trade_id":"T1","order_id":"OTHER","amount":19.99,"payment_url":"https://pay.example.com/x"}}`},
		"no trade id":      {http.StatusOK, `{"status_code":200,"data":{"order_id":"ORDER1","amount":19.99,"payment_url":"https://pay.example.com/x"}}`},
		"unusable pay url": {http.StatusOK, `{"status_code":200,"data":{"trade_id":"T1","order_id":"ORDER1","amount":19.99,"payment_url":"javascript:alert(1)"}}`},
		"not json":         {http.StatusBadGateway, `<html>bad gateway</html>`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := fakeGateway(t, func(map[string]any) (int, string) { return tc.status, tc.payload })
			if _, err := epusdtClient(srv.URL).Create(context.Background(), CreateInput{OutTradeNo: "ORDER1", AmountCents: 1999, Title: "x"}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestEPUSDTCreateNeedsConfiguration(t *testing.T) {
	_, err := (&EPUSDT{}).Create(context.Background(), CreateInput{OutTradeNo: "ORDER1", AmountCents: 100})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestEPUSDTStatus(t *testing.T) {
	srv := fakeGateway(t, func(params map[string]any) (int, string) {
		if params["trade_id"] != "T123" {
			return http.StatusBadRequest, `{"status_code":10008,"message":"order not found"}`
		}
		return http.StatusOK, `{"status_code":200,"message":"success","data":{"trade_id":"T123","status":2}}`
	})
	client := epusdtClient(srv.URL)
	status, err := client.Status(context.Background(), "T123")
	if err != nil || status != EPUSDTStatusPaid {
		t.Fatalf("status = %d, %v", status, err)
	}
	if _, err := client.Status(context.Background(), "MISSING"); err == nil {
		t.Fatal("unknown order must be an error, not a status")
	}
	if _, err := client.Status(context.Background(), " "); err == nil {
		t.Fatal("empty trade id must be an error")
	}
}

func signedNotification(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	params := map[string]any{
		"pid": "1000", "trade_id": "T123", "order_id": "ORDER1",
		"amount": 19.99, "actual_amount": 2.94, "receive_address": "TXYZ",
		"token": "USDT", "network": "tron", "block_transaction_id": "abc123", "status": 2,
	}
	if mutate != nil {
		mutate(params)
	}
	sig, err := EPUSDTSign(params, epusdtTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	params["signature"] = sig
	body, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestEPUSDTParseNotification(t *testing.T) {
	n, err := epusdtClient("http://127.0.0.1:8000").ParseNotification(signedNotification(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := EPUSDTNotification{
		TradeID: "T123", OrderID: "ORDER1", AmountCents: 1999, ActualAmount: 2.94,
		Token: "USDT", Network: "tron", ReceiveAddress: "TXYZ", BlockTransactionID: "abc123", Status: 2,
	}
	if n != want {
		t.Fatalf("notification = %+v, want %+v", n, want)
	}
}

func TestEPUSDTParseNotificationRejectsForgeries(t *testing.T) {
	client := epusdtClient("http://127.0.0.1:8000")
	valid := signedNotification(t, nil)

	tampered := []byte(strings.Replace(string(valid), `"amount":19.99`, `"amount":1999.99`, 1))
	if string(tampered) == string(valid) {
		t.Fatal("fixture did not change")
	}
	unsigned := []byte(`{"pid":"1000","order_id":"ORDER1","amount":19.99,"status":2}`)
	otherSecret := &EPUSDT{APIBase: "http://127.0.0.1:8000", PID: "1000", SecretKey: "other"}
	// Correctly signed, but for a different merchant on the same gateway key.
	otherPID := signedNotification(t, func(p map[string]any) { p["pid"] = "2000" })

	for name, body := range map[string][]byte{"tampered amount": tampered, "unsigned": unsigned, "other pid": otherPID} {
		if _, err := client.ParseNotification(body); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("%s: err = %v, want ErrBadSignature", name, err)
		}
	}
	if _, err := otherSecret.ParseNotification(valid); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret: err = %v", err)
	}
	if _, err := client.ParseNotification([]byte(`not json`)); err == nil || errors.Is(err, ErrBadSignature) {
		t.Fatalf("malformed body: err = %v", err)
	}
}

func TestValidGatewayBase(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:8000":   true,
		"http://localhost:8000":   true,
		"http://[::1]:8000":       true,
		"https://pay.example.com": true,
		"http://pay.example.com":  false,
		"http://10.0.0.5:8000":    false,
		"https://":                false,
		"":                        false,
		"ftp://127.0.0.1":         false,
	}
	for base, want := range cases {
		if got := ValidGatewayBase(base); got != want {
			t.Errorf("ValidGatewayBase(%q) = %v, want %v", base, got, want)
		}
	}
	// A rejected base disables the whole channel rather than sending the
	// reconciliation query over a forgeable transport.
	c := &EPUSDT{APIBase: "http://pay.example.com", PID: "1000", SecretKey: "k"}
	if c.Configured() {
		t.Fatal("a plain-HTTP remote gateway must not be treated as configured")
	}
	if _, err := c.Status(context.Background(), "T1"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}
