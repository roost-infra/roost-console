package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/auth"
	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/config"
)

// testReservationUUID is the valid UUID every handler test uses for the
// {reservationID} path value.
const testReservationUUID = "6f1b1e5a-8f8e-4c4c-9a9e-2a7d6c5b4a30"

type fakeRelayBilling struct {
	settleCalls   []fakeSettleCall
	settleV2Calls []billing.RelaySettleInput
	reserveCalls  []billing.RelayReserveInput
	reserveErr    error
	settleOp      billing.RelayOperation
	settleErr     error
	operation     billing.RelayOperation
	operationErr  error
	charges       []billing.ToolCharge
	chargesErr    error
	identity      billing.RelayIdentity
	apiKeyErr     error
}

type fakeSettleCall struct {
	reservationID  string
	settlementID   string
	idempotencyKey string
	amount         int64
	charges        []billing.ToolCharge
	compression    *billing.Compression
	usage          *billing.UsageEvent
}

func (f *fakeRelayBilling) RelayAPIKey(context.Context, string) (billing.RelayIdentity, error) {
	if f.apiKeyErr != nil {
		return billing.RelayIdentity{}, f.apiKeyErr
	}
	if f.identity.OrgID == "" {
		return billing.RelayIdentity{OrgID: "org-1"}, nil
	}
	return f.identity, nil
}

func (f *fakeRelayBilling) RelayBalance(context.Context, string) (int64, error) {
	return 1000, nil
}

func (f *fakeRelayBilling) RelayReserve(context.Context, string, string, int64, string, time.Duration) (billing.RelayReservation, error) {
	return billing.RelayReservation{ID: "res-1"}, nil
}

func (f *fakeRelayBilling) RelayReserveWithDetails(_ context.Context, in billing.RelayReserveInput) (billing.RelayReservation, error) {
	f.reserveCalls = append(f.reserveCalls, in)
	if f.reserveErr != nil {
		return billing.RelayReservation{}, f.reserveErr
	}
	return billing.RelayReservation{ID: "res-1", Amount: in.Amount, PriceVersion: in.PriceVersion, SettingsVersion: "2026-11-25.1"}, nil
}

func (f *fakeRelayBilling) RelaySettleWithDetails(_ context.Context, reservationID, settlementID, idempotencyKey string, amount int64, charges []billing.ToolCharge, compression *billing.Compression, usage *billing.UsageEvent) (billing.RelayOperation, error) {
	f.settleCalls = append(f.settleCalls, fakeSettleCall{
		reservationID:  reservationID,
		settlementID:   settlementID,
		idempotencyKey: idempotencyKey,
		amount:         amount,
		charges:        charges,
		compression:    compression,
		usage:          usage,
	})
	f.settleOp.ToolCharges = charges
	f.settleOp.Compression = compression
	f.settleOp.Usage = usage
	return f.settleOp, f.settleErr
}

func (f *fakeRelayBilling) RelaySettleV2(_ context.Context, in billing.RelaySettleInput) (billing.RelayOperation, error) {
	f.settleV2Calls = append(f.settleV2Calls, in)
	f.settleOp.ToolCharges = in.ToolCharges
	f.settleOp.Usage = in.Stats
	return f.settleOp, f.settleErr
}

func (f *fakeRelayBilling) RelayRelease(context.Context, string) (billing.RelayOperation, error) {
	return billing.RelayOperation{Found: true, Operation: "release", Status: "released"}, nil
}

func (f *fakeRelayBilling) RelayOperation(context.Context, string) (billing.RelayOperation, error) {
	return f.operation, f.operationErr
}

func (f *fakeRelayBilling) RelayToolCharges(context.Context, string) ([]billing.ToolCharge, error) {
	return f.charges, f.chargesErr
}

func relayTestServer(f *fakeRelayBilling) *Server {
	return &Server{relay: f}
}

func postRelaySettle(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://example/v1/relay/reservations/6f1b1e5a-8f8e-4c4c-9a9e-2a7d6c5b4a30/settle", strings.NewReader(body))
	req.SetPathValue("reservationID", testReservationUUID)
	rec := httptest.NewRecorder()
	s.relaySettle(rec, req)
	return rec
}

func decodeRelayResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	return out
}

func TestRelaySettleWithToolCharges(t *testing.T) {
	f := &fakeRelayBilling{settleOp: billing.RelayOperation{Found: true, Operation: "settle", Status: "settled", ReservationID: "res-1", Amount: 1}}
	s := relayTestServer(f)
	rec := postRelaySettle(t, s, `{
		"settlement_id":"stl-1",
		"idempotency_key":"settle:req-1",
		"amount_minor":7,
		"currency":"CREDIT",
		"tool_charges":[
			{"tool":"search","unit":"call","quantity":1,"unit_price_micro":700000,"amount_minor":7,"provider_cost_micro":750,"status":"succeeded"}
		]
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.settleCalls) != 1 {
		t.Fatalf("settle calls=%d", len(f.settleCalls))
	}
	call := f.settleCalls[0]
	if call.reservationID != testReservationUUID || call.settlementID != "stl-1" || call.idempotencyKey != "settle:req-1" {
		t.Fatalf("call identifiers=%+v", call)
	}
	if call.amount != 1 {
		t.Fatalf("amount credits=%d want 1 (7 minor rounds up)", call.amount)
	}
	if len(call.charges) != 1 || call.charges[0].Tool != "search" || call.charges[0].AmountMinor != 7 {
		t.Fatalf("charges=%+v", call.charges)
	}

	body := decodeRelayResponse(t, rec)
	if body["operation"] != "settle" || body["status"] != "settled" {
		t.Fatalf("body=%v", body)
	}
	if body["tool_charges_amount_minor"] != float64(7) || body["tool_charges_provider_cost_micro"] != float64(750) {
		t.Fatalf("totals=%v", body)
	}
	total, _ := body["tool_charges_total"].(map[string]any)
	if total["amount_minor"] != float64(7) || total["provider_cost_micro"] != float64(750) {
		t.Fatalf("totals object=%v", body["tool_charges_total"])
	}
	charges, _ := body["tool_charges"].([]any)
	if len(charges) != 1 {
		t.Fatalf("tool_charges=%v", body["tool_charges"])
	}
}

func TestRelaySettleMalformedToolChargesBooksNothing(t *testing.T) {
	valid := `{"tool":"search","unit":"call","quantity":1,"unit_price_micro":700000,"amount_minor":7,"provider_cost_micro":750,"status":"succeeded"}`
	cases := []struct {
		name string
		body string
	}{
		{"empty tool", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"","unit":"call","quantity":1,"amount_minor":7,"status":"succeeded"}]}`},
		{"blank tool", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"  ","unit":"call","quantity":1,"amount_minor":7,"status":"succeeded"}]}`},
		{"empty unit", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"","quantity":1,"amount_minor":7,"status":"succeeded"}]}`},
		{"unknown unit", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"token","quantity":1,"amount_minor":7,"status":"succeeded"}]}`},
		{"zero quantity", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"call","quantity":0,"amount_minor":7,"status":"succeeded"}]}`},
		{"negative quantity", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"call","quantity":-1,"amount_minor":7,"status":"succeeded"}]}`},
		{"negative unit price", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"call","quantity":1,"unit_price_micro":-1,"amount_minor":7,"status":"succeeded"}]}`},
		{"negative amount", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"call","quantity":1,"amount_minor":-1,"status":"succeeded"}]}`},
		{"negative provider cost", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"call","quantity":1,"amount_minor":7,"provider_cost_micro":-1,"status":"succeeded"}]}`},
		{"empty status", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"call","quantity":1,"amount_minor":7,"status":""}]}`},
		{"unknown status", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"call","quantity":1,"amount_minor":7,"status":"failed"}]}`},
		{"object instead of array", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":{"tool":"search"}}`},
		{"string quantity", `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"call","quantity":"1","amount_minor":7,"status":"succeeded"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRelayBilling{}
			rec := postRelaySettle(t, relayTestServer(f), tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if len(f.settleCalls) != 0 {
				t.Fatalf("malformed request reached the store: %+v", f.settleCalls)
			}
		})
	}

	// The same valid entry must still be accepted; this protects the malformed
	// cases above from passing because the request shape was wrong.
	f := &fakeRelayBilling{settleOp: billing.RelayOperation{Found: true, Operation: "settle", Status: "settled", Amount: 1}}
	rec := postRelaySettle(t, relayTestServer(f), `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[`+valid+`]}`)
	if rec.Code != http.StatusOK || len(f.settleCalls) != 1 {
		t.Fatalf("valid charge rejected: status=%d body=%s calls=%d", rec.Code, rec.Body.String(), len(f.settleCalls))
	}
}

func TestRelaySettleCapsToolChargeArray(t *testing.T) {
	entry := `{"tool":"search","unit":"call","quantity":1,"unit_price_micro":700000,"amount_minor":7,"provider_cost_micro":750,"status":"succeeded"}`
	build := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = entry
		}
		return `{"idempotency_key":"settle:r1","amount_minor":7,"tool_charges":[` + strings.Join(parts, ",") + `]}`
	}

	tooMany := &fakeRelayBilling{}
	rec := postRelaySettle(t, relayTestServer(tooMany), build(billing.MaxToolCharges+1))
	if rec.Code != http.StatusBadRequest || len(tooMany.settleCalls) != 0 {
		t.Fatalf("over cap: status=%d calls=%d", rec.Code, len(tooMany.settleCalls))
	}

	atCap := &fakeRelayBilling{settleOp: billing.RelayOperation{Found: true, Operation: "settle", Status: "settled", Amount: 1}}
	rec = postRelaySettle(t, relayTestServer(atCap), build(billing.MaxToolCharges))
	if rec.Code != http.StatusOK || len(atCap.settleCalls) != 1 {
		t.Fatalf("at cap: status=%d body=%s calls=%d", rec.Code, rec.Body.String(), len(atCap.settleCalls))
	}
}

func TestRelaySettleWithoutToolChargesPreservesOldBehavior(t *testing.T) {
	f := &fakeRelayBilling{settleOp: billing.RelayOperation{Found: true, Operation: "settle", Status: "settled", ReservationID: "res-1", Amount: 3}}
	rec := postRelaySettle(t, relayTestServer(f), `{"settlement_id":"stl-1","idempotency_key":"settle:req-1","amount_minor":2500}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.settleCalls) != 1 || f.settleCalls[0].charges != nil {
		t.Fatalf("calls=%+v", f.settleCalls)
	}
	if f.settleCalls[0].amount != 3 {
		t.Fatalf("amount=%d want 3", f.settleCalls[0].amount)
	}
	body := decodeRelayResponse(t, rec)
	if body["amount_minor"] != float64(3000) {
		t.Fatalf("amount_minor=%v", body["amount_minor"])
	}
	charges, ok := body["tool_charges"].([]any)
	if !ok || len(charges) != 0 {
		t.Fatalf("tool_charges=%v", body["tool_charges"])
	}
	if body["tool_charges_amount_minor"] != float64(0) || body["tool_charges_provider_cost_micro"] != float64(0) {
		t.Fatalf("totals=%v", body)
	}
}

func TestRelayOperationIncludesToolCharges(t *testing.T) {
	f := &fakeRelayBilling{operation: billing.RelayOperation{
		Found: true, Operation: "settle", Status: "settled", ReservationID: "res-1", Amount: 1,
		ToolCharges: []billing.ToolCharge{{
			Tool: "search", Unit: "call", Quantity: 1, UnitPriceMicro: 700000,
			AmountMinor: 7, ProviderCostMicro: 750, Status: "succeeded",
		}},
	}}
	req := httptest.NewRequest(http.MethodGet, "http://example/v1/relay/operations/settle:req-1", nil)
	req.SetPathValue("idempotencyKey", "settle:req-1")
	rec := httptest.NewRecorder()
	relayTestServer(f).relayOperation(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeRelayResponse(t, rec)
	if body["found"] != true || body["tool_charges_amount_minor"] != float64(7) {
		t.Fatalf("body=%v", body)
	}
	charges, _ := body["tool_charges"].([]any)
	if len(charges) != 1 {
		t.Fatalf("tool_charges=%v", body["tool_charges"])
	}
}

func TestRelayReservationToolChargesEndpoint(t *testing.T) {
	f := &fakeRelayBilling{charges: []billing.ToolCharge{{
		Tool: "search", Unit: "call", Quantity: 1, UnitPriceMicro: 700000,
		AmountMinor: 7, ProviderCostMicro: 750, Status: "succeeded",
	}}}
	req := httptest.NewRequest(http.MethodGet, "http://example/v1/relay/reservations/6f1b1e5a-8f8e-4c4c-9a9e-2a7d6c5b4a30/tool-charges", nil)
	req.SetPathValue("reservationID", testReservationUUID)
	rec := httptest.NewRecorder()
	relayTestServer(f).relayReservationToolCharges(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeRelayResponse(t, rec)
	if body["reservation_id"] != testReservationUUID || body["tool_charges_amount_minor"] != float64(7) || body["tool_charges_provider_cost_micro"] != float64(750) {
		t.Fatalf("body=%v", body)
	}
}

func TestRelaySettleStoreInvalidChargesMapsTo400(t *testing.T) {
	f := &fakeRelayBilling{settleErr: fmt.Errorf("%w: quantity", billing.ErrInvalidToolCharges)}
	rec := postRelaySettle(t, relayTestServer(f), `{"idempotency_key":"settle:r1","amount_minor":7}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// signedRelayRequest signs a request exactly like the relay service does, so
// these tests exercise routing, service auth and the handler together.
func signedRelayRequest(method, uri string, body []byte) *http.Request {
	req := httptest.NewRequest(method, "http://example"+uri, bytes.NewReader(body))
	ts, nonce, sig := auth.SignService("relay", method, uri, body, "relay-secret", time.Now())
	req.Header.Set("X-Roost-Service", "relay")
	req.Header.Set("X-Roost-Timestamp", ts)
	req.Header.Set("X-Roost-Nonce", nonce)
	req.Header.Set("X-Roost-Signature", sig)
	return req
}

func relayHTTPTestServer(f *fakeRelayBilling) http.Handler {
	return (&Server{
		cfg:   config.Config{ServiceSecrets: map[string][]string{"relay": {"relay-secret"}}},
		relay: f,
	}).Handler()
}

func TestRelaySettleRouteWithToolCharges(t *testing.T) {
	f := &fakeRelayBilling{settleOp: billing.RelayOperation{Found: true, Operation: "settle", Status: "settled", ReservationID: "res-1", Amount: 1}}
	body := []byte(`{"settlement_id":"stl-1","idempotency_key":"settle:req-1","amount_minor":7,"tool_charges":[{"tool":"search","unit":"call","quantity":1,"unit_price_micro":700000,"amount_minor":7,"provider_cost_micro":750,"status":"succeeded"}]}`)
	req := signedRelayRequest(http.MethodPost, "/v1/relay/reservations/6f1b1e5a-8f8e-4c4c-9a9e-2a7d6c5b4a30/settle", body)
	rec := httptest.NewRecorder()
	relayHTTPTestServer(f).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.settleCalls) != 1 || len(f.settleCalls[0].charges) != 1 {
		t.Fatalf("settle calls=%+v", f.settleCalls)
	}
	out := decodeRelayResponse(t, rec)
	if out["tool_charges_amount_minor"] != float64(7) {
		t.Fatalf("body=%v", out)
	}
}

func TestRelayToolChargesRoute(t *testing.T) {
	f := &fakeRelayBilling{charges: []billing.ToolCharge{{
		Tool: "search", Unit: "call", Quantity: 1, UnitPriceMicro: 700000,
		AmountMinor: 7, ProviderCostMicro: 750, Status: "succeeded",
	}}}
	req := signedRelayRequest(http.MethodGet, "/v1/relay/reservations/6f1b1e5a-8f8e-4c4c-9a9e-2a7d6c5b4a30/tool-charges", nil)
	rec := httptest.NewRecorder()
	relayHTTPTestServer(f).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeRelayResponse(t, rec)
	if body["tool_charges_amount_minor"] != float64(7) || body["tool_charges_provider_cost_micro"] != float64(750) {
		t.Fatalf("body=%v", body)
	}
}

func TestRelayToolChargesRouteRequiresServiceAuth(t *testing.T) {
	f := &fakeRelayBilling{}
	req := httptest.NewRequest(http.MethodGet, "http://example/v1/relay/reservations/6f1b1e5a-8f8e-4c4c-9a9e-2a7d6c5b4a30/tool-charges", nil)
	rec := httptest.NewRecorder()
	relayHTTPTestServer(f).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
