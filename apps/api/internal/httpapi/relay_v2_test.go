package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
)

func postRelayReserve(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://example/v1/relay/reservations", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.relayReserve(rec, req)
	return rec
}

func TestRelayReserveRecordsPriceVersion(t *testing.T) {
	f := &fakeRelayBilling{}
	rec := postRelayReserve(t, relayTestServer(f), `{
		"request_id":"req-1","subject_ref":"4d1e4d5e-8f0f-4a3a-9a3e-2b1c0d9e8f7a","amount_minor":2500,
		"currency":"CREDIT","ttl_seconds":900,"price_version":"2026-11-25.1"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.reserveCalls) != 1 {
		t.Fatalf("reserve calls=%+v", f.reserveCalls)
	}
	call := f.reserveCalls[0]
	if call.PriceVersion != "2026-11-25.1" || call.Amount != 3 || call.OrgID != "4d1e4d5e-8f0f-4a3a-9a3e-2b1c0d9e8f7a" || call.RequestID != "req-1" {
		t.Fatalf("reserve input=%+v", call)
	}
	body := decodeRelayResponse(t, rec)
	if body["price_version"] != "2026-11-25.1" || body["settings_version"] != "2026-11-25.1" {
		t.Fatalf("response versions=%v", body)
	}
	if body["amount_credits"] != float64(3) {
		t.Fatalf("amount_credits=%v", body["amount_credits"])
	}
}

func TestRelayReservePriceVersionMismatch(t *testing.T) {
	f := &fakeRelayBilling{reserveErr: billing.ErrPriceVersionMismatch}
	rec := postRelayReserve(t, relayTestServer(f), `{
		"request_id":"req-1","subject_ref":"4d1e4d5e-8f0f-4a3a-9a3e-2b1c0d9e8f7a","amount_minor":100,
		"currency":"CREDIT","price_version":"bogus"
	}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeRelayResponse(t, rec)
	errBody, _ := body["error"].(map[string]any)
	if errBody["code"] != "price_version_mismatch" {
		t.Fatalf("error=%v", body)
	}
}

func TestRelaySettleV2RecomputesAndEchoesBreakdown(t *testing.T) {
	f := &fakeRelayBilling{settleOp: billing.RelayOperation{
		Found: true, Operation: "settle", Status: "settled", ReservationID: "res-1",
		Amount: 166, AmountMinor: 165283,
		PolicyVersion: "2026-11-25.1", SettingsVersion: "2026-11-25.1",
		UpstreamID: "fyt",
		Pricing: &billing.Charge{Compression: &billing.CompressionCharge{
			SavedTokens: 63000, UserShareTokens: 15750, NetGainMinor: 16573,
		}},
	}}
	rec := postRelaySettle(t, relayTestServer(f), `{
		"settlement_id":"stl-1",
		"idempotency_key":"settle:req-1",
		"model":"claude-sonnet-5.5",
		"upstream_id":"fyt",
		"started_at":"2026-11-23T10:00:00+08:00",
		"price_version":"2026-11-25.1",
		"usage":{"input":150000,"output":2000,"cache_read":0,"cache_write":0},
		"basis":{"mode":"original","before_input_tokens":150000,"after_input_tokens":90000,
			"summary_cost_minor":20,"cache_loss_minor":0},
		"tool_charges":[{"tool":"search","unit":"call","quantity":1,"status":"succeeded"}],
		"amount_minor":165283
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.settleCalls) != 0 {
		t.Fatalf("v2 request took the legacy settle path: %+v", f.settleCalls)
	}
	if len(f.settleV2Calls) != 1 {
		t.Fatalf("v2 settle calls=%d", len(f.settleV2Calls))
	}
	call := f.settleV2Calls[0]
	if call.Model != "claude-sonnet-5.5" || call.UpstreamID != "fyt" || call.PriceVersion != "2026-11-25.1" {
		t.Fatalf("v2 input=%+v", call)
	}
	if call.Usage == nil || call.Usage.Input != 150000 || call.Usage.Output != 2000 {
		t.Fatalf("usage=%+v", call.Usage)
	}
	if call.Basis == nil || call.Basis.BeforeInputTokens != 150000 || call.Basis.AfterInputTokens != 90000 ||
		call.Basis.SummaryCostMinor != 20 {
		t.Fatalf("basis=%+v", call.Basis)
	}
	if call.AmountMinor == nil || *call.AmountMinor != 165283 {
		t.Fatalf("amount_minor=%v", call.AmountMinor)
	}
	if len(call.ToolCharges) != 1 || call.ToolCharges[0].Tool != "search" {
		t.Fatalf("tool charges=%+v", call.ToolCharges)
	}
	if !call.StartedAt.Equal(time.Date(2026, 11, 23, 10, 0, 0, 0, time.FixedZone("CST", 8*3600))) {
		t.Fatalf("started_at=%v", call.StartedAt)
	}
	body := decodeRelayResponse(t, rec)
	if body["amount_minor"] != float64(165283) || body["amount_credits"] != float64(166) {
		t.Fatalf("amounts=%v", body)
	}
	if body["policy_version"] != "2026-11-25.1" || body["settings_version"] != "2026-11-25.1" {
		t.Fatalf("versions=%v", body)
	}
	compression, ok := body["compression"].(map[string]any)
	if !ok || compression["saved_tokens"] != float64(63000) ||
		compression["user_share_tokens"] != float64(15750) ||
		compression["net_gain_minor"] != float64(16573) {
		t.Fatalf("compression=%v", body["compression"])
	}
	debts, ok := body["debts"].([]any)
	if !ok || len(debts) != 0 {
		t.Fatalf("debts=%v want empty array", body["debts"])
	}
}

func TestRelaySettleV2WithoutCompression(t *testing.T) {
	f := &fakeRelayBilling{settleOp: billing.RelayOperation{
		Found: true, Operation: "settle", Status: "settled", Amount: 180, AmountMinor: 179595,
	}}
	rec := postRelaySettle(t, relayTestServer(f), `{
		"settlement_id":"stl-1","idempotency_key":"settle:req-1",
		"model":"claude-sonnet-5.5","price_version":"2026-11-25.1",
		"usage":{"input":150000,"output":2000}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeRelayResponse(t, rec)
	if body["compression"] != nil {
		t.Fatalf("compression=%v want null", body["compression"])
	}
	if body["amount_minor"] != float64(179595) {
		t.Fatalf("amount_minor=%v", body["amount_minor"])
	}
}

func TestRelaySettleV2RequiresUsage(t *testing.T) {
	f := &fakeRelayBilling{}
	rec := postRelaySettle(t, relayTestServer(f), `{
		"settlement_id":"stl-1","idempotency_key":"settle:req-1","model":"claude-sonnet-5.5"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.settleV2Calls) != 0 || len(f.settleCalls) != 0 {
		t.Fatalf("malformed v2 request reached the store")
	}
}

func TestRelaySettleV2ErrorCodes(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"amount mismatch":        {billing.ErrAmountMismatch, http.StatusConflict, "amount_mismatch"},
		"price version mismatch": {billing.ErrPriceVersionMismatch, http.StatusConflict, "price_version_mismatch"},
		"unknown model":          {billing.ErrUnknownModel, http.StatusUnprocessableEntity, "unknown_model"},
		"released lease":         {billing.ErrRelayReleased, http.StatusConflict, "lease_expired"},
		"invalid pricing":        {billing.ErrInvalidPricing, http.StatusBadRequest, "invalid_request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeRelayBilling{settleErr: tc.err}
			rec := postRelaySettle(t, relayTestServer(f), `{
				"settlement_id":"stl-1","idempotency_key":"settle:req-1",
				"model":"claude-sonnet-5.5","price_version":"2026-11-25.1",
				"usage":{"input":1}
			}`)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := decodeRelayResponse(t, rec)
			errBody, _ := body["error"].(map[string]any)
			if errBody["code"] != tc.code {
				t.Fatalf("error=%v want code %q", body, tc.code)
			}
		})
	}
}

func TestRelaySettleLegacyStatsShapeStaysLegacy(t *testing.T) {
	f := &fakeRelayBilling{settleOp: billing.RelayOperation{
		Found: true, Operation: "settle", Status: "settled", Amount: 2,
	}}
	rec := postRelaySettle(t, relayTestServer(f), `{
		"settlement_id":"stl-1","idempotency_key":"settle:req-1","amount_minor":2000,
		"usage":{"model":"deepseek-v4-pro","requests":1,"input_tokens":1000,"output_tokens":10}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.settleV2Calls) != 0 {
		t.Fatalf("stats-shaped usage took the v2 path: %+v", f.settleV2Calls)
	}
	if len(f.settleCalls) != 1 {
		t.Fatalf("legacy settle calls=%d", len(f.settleCalls))
	}
	call := f.settleCalls[0]
	if call.amount != 2 || call.usage == nil || call.usage.Model != "deepseek-v4-pro" || call.usage.InputTokens != 1000 {
		t.Fatalf("legacy settle=%+v", call)
	}
}

func TestRelaySettleV2AcceptsUsageStatsForReporting(t *testing.T) {
	f := &fakeRelayBilling{settleOp: billing.RelayOperation{
		Found: true, Operation: "settle", Status: "settled", Amount: 1, AmountMinor: 1,
	}}
	rec := postRelaySettle(t, relayTestServer(f), `{
		"settlement_id":"stl-1","idempotency_key":"settle:req-1",
		"model":"claude-sonnet-5.5","price_version":"2026-11-25.1",
		"usage":{"input":10,"output":0},
		"usage_stats":{"model":"claude-sonnet-5.5","requests":3,"input_tokens":10}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.settleV2Calls) != 1 || f.settleV2Calls[0].Stats == nil {
		t.Fatalf("usage_stats not forwarded: %+v", f.settleV2Calls)
	}
	if f.settleV2Calls[0].Stats.Requests != 3 {
		t.Fatalf("usage stats=%+v", f.settleV2Calls[0].Stats)
	}
}

func TestRelaySettleV2RejectsNegativeUsage(t *testing.T) {
	f := &fakeRelayBilling{}
	rec := postRelaySettle(t, relayTestServer(f), `{
		"settlement_id":"stl-1","idempotency_key":"settle:req-1",
		"model":"claude-sonnet-5.5","usage":{"input":-1}
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeRelayResponse(t, rec)
	errBody, _ := body["error"].(map[string]any)
	if errBody["code"] != "invalid_usage" {
		t.Fatalf("error=%v", body)
	}
	if len(f.settleV2Calls) != 0 {
		t.Fatalf("negative usage reached the store")
	}
}
