package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/auth"
	"github.com/JingxuanC/roost-console/apps/api/internal/config"
)

func testServiceServer() *Server {
	return &Server{
		cfg: config.Config{
			PublicBaseURL: "https://www.roostbots.com/console-api",
			ServiceSecrets: map[string][]string{
				"dslpha": {"dslpha-secret"},
			},
		},
		signingPrefixes: []string{"/console-api"},
	}
}

func TestVerifyServiceAcceptsPublicPathSignature(t *testing.T) {
	s := testServiceServer()
	body := []byte(`{"user_id":"u1","run_ref":"r1"}`)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/auth/billing/leases/acquire", nil)
	publicURI := "/console-api" + r.URL.RequestURI()
	ts, nonce, sig := auth.SignService("dslpha", r.Method, publicURI, body, "dslpha-secret", time.Now())
	r.Header.Set("X-Roost-Service", "dslpha")
	r.Header.Set("X-Roost-Timestamp", ts)
	r.Header.Set("X-Roost-Nonce", nonce)
	r.Header.Set("X-Roost-Signature", sig)
	if !s.verifyService("dslpha", r, body) {
		t.Fatal("signature over the public path was rejected")
	}
}

func TestVerifyServiceAcceptsRenamedProduct(t *testing.T) {
	s := testServiceServer()
	body := []byte(`{"user_id":"u1","run_ref":"r1"}`)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/auth/billing/leases/acquire", nil)
	publicURI := "/console-api" + r.URL.RequestURI()
	ts, nonce, sig := auth.SignService("dsalpha", r.Method, publicURI, body, "dslpha-secret", time.Now())
	r.Header.Set("X-Roost-Service", "dsalpha")
	r.Header.Set("X-Roost-Timestamp", ts)
	r.Header.Set("X-Roost-Nonce", nonce)
	r.Header.Set("X-Roost-Signature", sig)
	if !s.verifyService("dsalpha", r, body) {
		t.Fatal("renamed product signature was rejected")
	}
	if !serviceAllows("dsalpha", http.MethodPost, "/api/auth/billing/leases/acquire") {
		t.Fatal("renamed product scope was rejected")
	}
	if !s.verifyLegacyServiceToken("dsalpha", "dslpha-secret") {
		t.Fatal("renamed product legacy token was rejected")
	}
}

func TestVerifyServiceRejectsWrongSignature(t *testing.T) {
	s := testServiceServer()
	body := []byte(`{}`)
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/auth/billing/leases/acquire", nil)
	if s.verifyService("dslpha", r, body) {
		t.Fatal("unsigned request accepted")
	}
}

func TestLegacyServiceToken(t *testing.T) {
	s := testServiceServer()
	if !s.verifyLegacyServiceToken("dslpha", "dslpha-secret") {
		t.Fatal("legacy token rejected")
	}
	if s.verifyLegacyServiceToken("dslpha", "wrong") {
		t.Fatal("wrong legacy token accepted")
	}
	if s.verifyLegacyServiceToken("unknown", "dslpha-secret") {
		t.Fatal("token accepted for an unknown service")
	}
}
