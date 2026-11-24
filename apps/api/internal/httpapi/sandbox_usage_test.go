package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/auth"
	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/config"
)

type fakeSandboxUsageRecorder struct {
	result billing.SandboxChargeResult
	err    error
	calls  []billing.SandboxUsageInput
}

func (f *fakeSandboxUsageRecorder) RecordSandboxUsage(_ context.Context, in billing.SandboxUsageInput) (billing.SandboxChargeResult, error) {
	f.calls = append(f.calls, in)
	return f.result, f.err
}

func sandboxTestSecrets() map[string][]string {
	return map[string][]string{
		"market": {"market-secret"},
		"dslpha": {"dslpha-secret"},
		"causal": {"causal-secret"},
	}
}

func signedSandboxRequest(service string, body []byte) *http.Request {
	const uri = "/internal/v1/usage/sandbox"
	req := httptest.NewRequest(http.MethodPost, "http://example"+uri, bytes.NewReader(body))
	ts, nonce, sig := auth.SignService(service, http.MethodPost, uri, body, service+"-secret", time.Now())
	req.Header.Set("X-Roost-Service", service)
	req.Header.Set("X-Roost-Timestamp", ts)
	req.Header.Set("X-Roost-Nonce", nonce)
	req.Header.Set("X-Roost-Signature", sig)
	return req
}

func sandboxHTTPTestServer(rec sandboxUsageRecorder) *Server {
	return &Server{
		cfg:          config.Config{ServiceSecrets: sandboxTestSecrets()},
		sandboxUsage: rec,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func validSandboxBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(billing.SandboxUsageInput{
		OrgID:             "4d1e4d5e-8f0f-4a3a-9a3e-2b1c0d9e8f7a",
		UserID:            "user-1",
		Product:           "market",
		SandboxID:         "sbx-1",
		Provider:          "daytona",
		ProviderSandboxID: "daytona-1",
		RunID:             "run-1",
		BillableSeconds:   600,
		CPUSeconds:        600,
		MemoryMiBSeconds:  600,
		DiskMiBSeconds:    600,
		CPUCredits:        2,
		Currency:          "CNY",
		IdempotencyKey:    "sbx-run-1",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return body
}

func TestSandboxUsageEndpointCreatesCharge(t *testing.T) {
	rec := &fakeSandboxUsageRecorder{result: billing.SandboxChargeResult{
		Usage:          billing.SandboxUsage{ID: 42, TotalCredits: 2, CreditsMinor: 2000},
		ChargedCredits: 2,
		BalanceCredits: 98,
	}}
	srv := sandboxHTTPTestServer(rec)
	body := validSandboxBody(t)
	httpRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(httpRec, signedSandboxRequest("market", body))
	if httpRec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", httpRec.Code, httpRec.Body.String())
	}
	if len(rec.calls) != 1 {
		t.Fatalf("record calls=%+v", rec.calls)
	}
	if got := rec.calls[0]; got.OrgID != "4d1e4d5e-8f0f-4a3a-9a3e-2b1c0d9e8f7a" ||
		got.Product != "market" || got.SandboxID != "sbx-1" || got.CPUCredits != 2 ||
		got.IdempotencyKey != "sbx-run-1" || got.BillableSeconds != 600 {
		t.Fatalf("record input=%+v", got)
	}
	out := map[string]any{}
	if err := json.Unmarshal(httpRec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["id"] != float64(42) || out["charged_credits"] != float64(2) ||
		out["balance_credits"] != float64(98) || out["credits_minor"] != float64(2000) ||
		out["replayed"] != false {
		t.Fatalf("body=%v", out)
	}
}

func TestSandboxUsageEndpointReplayIs200(t *testing.T) {
	rec := &fakeSandboxUsageRecorder{result: billing.SandboxChargeResult{
		Usage:          billing.SandboxUsage{ID: 7, TotalCredits: 2, CreditsMinor: 2000},
		ChargedCredits: 2,
		BalanceCredits: 40,
		Replayed:       true,
	}}
	httpRec := httptest.NewRecorder()
	sandboxHTTPTestServer(rec).Handler().ServeHTTP(httpRec, signedSandboxRequest("dslpha", validSandboxBody(t)))
	if httpRec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", httpRec.Code, httpRec.Body.String())
	}
	out := map[string]any{}
	_ = json.Unmarshal(httpRec.Body.Bytes(), &out)
	if out["replayed"] != true || out["id"] != float64(7) {
		t.Fatalf("body=%v", out)
	}
}

func TestSandboxUsageEndpointValidationAndErrors(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		err    error
		status int
		code   string
	}{
		{name: "invalid json", body: `{`, status: http.StatusBadRequest, code: "invalid_sandbox_usage"},
		{name: "invalid usage", body: `{}`, err: billing.ErrInvalidSandboxUsage, status: http.StatusBadRequest, code: "invalid_sandbox_usage"},
		{name: "insufficient", body: `{}`, err: billing.ErrInsufficientCredits, status: http.StatusPaymentRequired, code: "insufficient_credits"},
		{name: "unknown org", body: `{}`, err: billing.ErrNotFound, status: http.StatusNotFound, code: "not_found"},
		{name: "internal", body: `{}`, err: io.ErrUnexpectedEOF, status: http.StatusInternalServerError, code: "internal_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &fakeSandboxUsageRecorder{err: c.err}
			httpRec := httptest.NewRecorder()
			sandboxHTTPTestServer(rec).Handler().ServeHTTP(httpRec, signedSandboxRequest("market", []byte(c.body)))
			if httpRec.Code != c.status {
				t.Fatalf("status=%d body=%s", httpRec.Code, httpRec.Body.String())
			}
			out := map[string]any{}
			_ = json.Unmarshal(httpRec.Body.Bytes(), &out)
			errBody, _ := out["error"].(map[string]any)
			if errBody["code"] != c.code {
				t.Fatalf("body=%v", out)
			}
		})
	}
}

func TestSandboxUsageEndpointAuthAndScope(t *testing.T) {
	rec := &fakeSandboxUsageRecorder{}
	srv := sandboxHTTPTestServer(rec)

	// No signature: 401.
	req := httptest.NewRequest(http.MethodPost, "http://example/internal/v1/usage/sandbox", bytes.NewReader(validSandboxBody(t)))
	httpRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(httpRec, req)
	if httpRec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned status=%d body=%s", httpRec.Code, httpRec.Body.String())
	}

	// A valid causal signature is still out of scope for sandbox reporting.
	httpRec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(httpRec, signedSandboxRequest("causal", validSandboxBody(t)))
	if httpRec.Code != http.StatusForbidden {
		t.Fatalf("causal status=%d body=%s", httpRec.Code, httpRec.Body.String())
	}
	if len(rec.calls) != 0 {
		t.Fatalf("rejected calls reached the store: %+v", rec.calls)
	}
}

func TestSandboxUsageRouteWithoutStoreReportsUnavailable(t *testing.T) {
	srv := &Server{
		cfg: config.Config{ServiceSecrets: sandboxTestSecrets()},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	httpRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(httpRec, signedSandboxRequest("market", validSandboxBody(t)))
	if httpRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", httpRec.Code, httpRec.Body.String())
	}
}

func TestSandboxUsageEndpointResourceGauges(t *testing.T) {
	rec := &fakeSandboxUsageRecorder{result: billing.SandboxChargeResult{
		Usage:          billing.SandboxUsage{ID: 9, TotalCredits: 157, CreditsMinor: 157000},
		ChargedCredits: 157,
		BalanceCredits: 843,
	}}
	body := []byte(`{
		"org_id":"4d1e4d5e-8f0f-4a3a-9a3e-2b1c0d9e8f7a","product":"dsalpha","sandbox_id":"sbx-2",
		"billable_seconds":3600,"cpu_cores":2,"memory_gb":4,"disk_gb":16,
		"cpu_seconds":3600,"memory_mib_seconds":1048576,"disk_mib_seconds":1024,
		"idempotency_key":"sbx-resource-1"
	}`)
	httpRec := httptest.NewRecorder()
	sandboxHTTPTestServer(rec).Handler().ServeHTTP(httpRec, signedSandboxRequest("dslpha", body))
	if httpRec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", httpRec.Code, httpRec.Body.String())
	}
	if len(rec.calls) != 1 {
		t.Fatalf("calls=%+v", rec.calls)
	}
	if got := rec.calls[0]; got.CPUCores != 2 || got.MemoryGB != 4 || got.DiskGB != 16 || got.BillableSeconds != 3600 {
		t.Fatalf("resource gauges not forwarded: %+v", got)
	}
}
