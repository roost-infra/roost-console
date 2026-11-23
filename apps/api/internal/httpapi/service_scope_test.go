package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServiceScopes(t *testing.T) {
	cases := []struct {
		service, method, path string
		want                  bool
	}{
		{"market", "POST", "/internal/v1/orgs/resolve-user", true},
		{"market", "POST", "/internal/v1/entitlements/resolve", true},
		{"market", "POST", "/internal/v1/usage/leases", true},
		{"market", "POST", "/internal/v1/usage/leases/by-run/draw", true},
		{"market", "POST", "/internal/v1/usage/charge", false},
		{"market", "POST", "/internal/v1/usage/sandbox", true},
		{"market", "POST", "/internal/v1/products/causal-memory/tokens", false},
		{"dslpha", "POST", "/api/auth/validate", true},
		{"dslpha", "POST", "/api/auth/billing/leases/acquire", true},
		{"dslpha", "POST", "/internal/v1/orgs/resolve-user", true},
		{"dslpha", "POST", "/internal/v1/usage/charge", true},
		{"dslpha", "POST", "/internal/v1/usage/sandbox", true},
		{"causal", "POST", "/internal/v1/usage/sandbox", false},
		{"causal", "POST", "/internal/v1/usage/charge", true},
		{"causal", "POST", "/internal/v1/usage/leases", false},
		{"causal-sync", "POST", "/internal/v1/products/causal-memory/tokens", true},
		{"relay", "POST", "/internal/v1/products/causal-memory/tokens", true},
		{"causal-sync", "POST", "/internal/v1/usage/charge", false},
		{"unknown", "POST", "/internal/v1/usage/charge", false},
	}
	for _, c := range cases {
		if got := serviceAllows(c.service, c.method, c.path); got != c.want {
			t.Errorf("serviceAllows(%q,%q)=%v want %v", c.service, c.path, got, c.want)
		}
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	s := &Server{}
	h := s.rateLimit("test", 2, ipKey, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	codes := []int{}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://example/oauth/token", nil)
		req.RemoteAddr = "203.0.113.7:1234"
		rec := httptest.NewRecorder()
		h(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("codes %v", codes)
	}
}
