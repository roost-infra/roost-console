package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/auth"
	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/config"
)

// --- test doubles -----------------------------------------------------------

type fakeMembership struct {
	roles map[string]string // "orgID/userID" -> role
}

func (f fakeMembership) Membership(_ context.Context, orgID, userID string) (string, error) {
	if role, ok := f.roles[orgID+"/"+userID]; ok {
		return role, nil
	}
	return "", billing.ErrNotFound
}

type fakeTokenStore struct {
	mu        sync.Mutex
	list      []billing.ProductToken
	listErr   error
	revokeErr error
	revoked   [][3]string
}

func (f *fakeTokenStore) ListProductTokens(_ context.Context, _, _ string) ([]billing.ProductToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list, f.listErr
}

func (f *fakeTokenStore) RevokeProductToken(_ context.Context, orgID, product, tokenHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, [3]string{orgID, product, tokenHash})
	return f.revokeErr
}

func (f *fakeTokenStore) revokeCalls() [][3]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][3]string, len(f.revoked))
	copy(out, f.revoked)
	return out
}

// fakeSupabase verifies the bearer token the way apps/api's auth.Verifier does:
// GET {SUPABASE_URL}/auth/v1/user with the caller token.
func fakeSupabase(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/v1/user" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var user auth.User
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "jwt-owner":
			user = auth.User{ID: "user-owner", Email: "owner@example.com"}
		case "jwt-other":
			user = auth.User{ID: "user-other", Email: "other@example.com"}
		case "jwt-foreign":
			user = auth.User{ID: "user-foreign", Email: "foreign@example.com"}
		default:
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(user)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testHandler(t *testing.T, cfg config.Config, membership orgMembership, tokens productTokenAdmin) http.Handler {
	t.Helper()
	supabase := fakeSupabase(t)
	return (&Server{
		cfg:           cfg,
		verifier:      auth.NewVerifier(supabase.URL, "console-anon-key", supabase.Client()),
		membership:    membership,
		productTokens: tokens,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler()
}

func testConfig() config.Config {
	return config.Config{
		SupabaseURL:      "https://proj.supabase.co",
		SupabaseAnonKey:  "public-anon-key",
		ConsoleOrigin:    "https://www.roostbots.com",
		OIDCIssuer:       "https://www.roostbots.com/console-api/",
		ConsoleAPIURL:    "https://www.roostbots.com/console-api",
		MarketAPIURL:     "https://www.roostbots.com/api",
		MemoryMCPURL:     "https://causal-memory.com/memory/mcp",
		ServiceSecret:    "super-secret-service",
		ServiceSecrets:   map[string][]string{"root": {"root-secret-value"}},
		XunhuAppSecret:   "xunhu-secret-value",
		EPUSDTSecretKey:  "epusdt-secret-value",
		OIDCClientSecret: "oidc-client-secret",
	}
}

func callJSON(t *testing.T, h http.Handler, method, path, bearer, origin string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
		}
	}
	return rec, out
}

// --- GET /v1/cli/config -----------------------------------------------------

func TestCLIConfigPublicShape(t *testing.T) {
	h := testHandler(t, testConfig(), nil, nil)
	rec, body := callJSON(t, h, http.MethodGet, "/v1/cli/config", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := body["supabase_url"]; got != "https://proj.supabase.co" {
		t.Fatalf("supabase_url=%v", got)
	}
	if got := body["supabase_anon_key"]; got != "public-anon-key" {
		t.Fatalf("supabase_anon_key=%v", got)
	}
	if got := body["console_api_url"]; got != "https://www.roostbots.com/console-api" {
		t.Fatalf("console_api_url=%v", got)
	}
	if got := body["market_api_url"]; got != "https://www.roostbots.com/api" {
		t.Fatalf("market_api_url=%v", got)
	}
	if got := body["memory_mcp_url"]; got != "https://causal-memory.com/memory/mcp" {
		t.Fatalf("memory_mcp_url=%v", got)
	}
	oidc, ok := body["oidc"].(map[string]any)
	if !ok {
		t.Fatalf("oidc=%v", body["oidc"])
	}
	if oidc["issuer"] != "https://www.roostbots.com/console-api" {
		t.Fatalf("oidc.issuer=%v", oidc["issuer"])
	}
	if oidc["authorization_endpoint"] != "https://www.roostbots.com/console-api/oauth/authorize" {
		t.Fatalf("oidc.authorization_endpoint=%v", oidc["authorization_endpoint"])
	}
	if oidc["token_endpoint"] != "https://www.roostbots.com/console-api/oauth/token" {
		t.Fatalf("oidc.token_endpoint=%v", oidc["token_endpoint"])
	}
	// The public document must be uncacheable and free of server-only config.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control=%q", got)
	}
}

func TestCLIConfigDoesNotLeakSecrets(t *testing.T) {
	h := testHandler(t, testConfig(), nil, nil)
	rec, body := callJSON(t, h, http.MethodGet, "/v1/cli/config", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	raw, _ := json.Marshal(body)
	text := string(raw)
	for _, secret := range []string{
		"super-secret-service", "root-secret-value", "xunhu-secret-value", "epusdt-secret-value", "oidc-client-secret",
		"database_url", "service_secrets", "service_secret", "oidc_client_secret", "xunhu_app_secret",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("public config leaked %q: %s", secret, text)
		}
	}
}

func TestCLIConfigCORS(t *testing.T) {
	cfg := testConfig()
	h := testHandler(t, cfg, nil, nil)

	// A console-origin GET is allowed and exposes the response to the browser.
	rec, _ := callJSON(t, h, http.MethodGet, "/v1/cli/config", "", cfg.ConsoleOrigin)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != cfg.ConsoleOrigin {
		t.Fatalf("allow-origin=%q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Fatalf("allow-methods=%q", got)
	}

	// Preflight for the public endpoint.
	req := httptest.NewRequest(http.MethodOptions, "/v1/cli/config", nil)
	req.Header.Set("Origin", cfg.ConsoleOrigin)
	preflight := httptest.NewRecorder()
	h.ServeHTTP(preflight, req)
	if preflight.Code != http.StatusNoContent {
		t.Fatalf("preflight status=%d", preflight.Code)
	}
	if got := preflight.Header().Get("Access-Control-Allow-Origin"); got != cfg.ConsoleOrigin {
		t.Fatalf("preflight allow-origin=%q", got)
	}
}

func TestCLIConfigDefaultsWithoutOptionalConfig(t *testing.T) {
	// Missing MARKET/CONSOLE/MEMORY URLs fall back to the documented defaults in
	// config.Load; the handler itself only emits what the config carries.
	cfg := testConfig()
	cfg.MemoryMCPURL = ""
	cfg.MarketAPIURL = ""
	cfg.ConsoleAPIURL = ""
	h := testHandler(t, cfg, nil, nil)
	rec, body := callJSON(t, h, http.MethodGet, "/v1/cli/config", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if body["memory_mcp_url"] != "https://causal-memory.com/memory/mcp" {
		t.Fatalf("memory fallback=%v", body["memory_mcp_url"])
	}
	if body["market_api_url"] != "" || body["console_api_url"] != "" {
		t.Fatalf("unexpected values: %v %v", body["market_api_url"], body["console_api_url"])
	}
}

// --- DELETE /v1/orgs/{orgID}/products/causal-memory/tokens/{hash} -----------

func membershipFixture() fakeMembership {
	return fakeMembership{roles: map[string]string{
		"org-1/user-owner":  "owner",
		"org-2/user-other":  "owner",
		"org-1/user-credit": "member",
	}}
}

func TestRevokeProductToken(t *testing.T) {
	tokens := &fakeTokenStore{}
	h := testHandler(t, testConfig(), membershipFixture(), tokens)

	rec, body := callJSON(t, h, http.MethodDelete, "/v1/orgs/org-1/products/causal-memory/tokens/hash-1", "jwt-owner", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%v", rec.Code, body)
	}
	calls := tokens.revokeCalls()
	if len(calls) != 1 || calls[0] != [3]string{"org-1", "causal-memory", "hash-1"} {
		t.Fatalf("revoke calls=%v", calls)
	}

	// Revoking an already-revoked token is idempotent (the store treats it as a
	// no-op success, the handler keeps answering 204).
	again, _ := callJSON(t, h, http.MethodDelete, "/v1/orgs/org-1/products/causal-memory/tokens/hash-1", "jwt-owner", "")
	if again.Code != http.StatusNoContent {
		t.Fatalf("idempotent status=%d body=%s", again.Code, again.Body.String())
	}
	if got := len(tokens.revokeCalls()); got != 2 {
		t.Fatalf("revoke calls=%d", got)
	}
}

func TestRevokeProductTokenNotFound(t *testing.T) {
	tokens := &fakeTokenStore{revokeErr: billing.ErrNotFound}
	h := testHandler(t, testConfig(), membershipFixture(), tokens)

	rec, body := callJSON(t, h, http.MethodDelete, "/v1/orgs/org-1/products/causal-memory/tokens/foreign-hash", "jwt-owner", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%v", rec.Code, body)
	}
	errBody, _ := body["error"].(map[string]any)
	if errBody["code"] != "not_found" {
		t.Fatalf("error=%v", body)
	}
}

func TestRevokeProductTokenCrossOrg(t *testing.T) {
	tokens := &fakeTokenStore{}
	h := testHandler(t, testConfig(), membershipFixture(), tokens)

	// Not a member of the org in the path: 403 before the store is touched.
	rec, body := callJSON(t, h, http.MethodDelete, "/v1/orgs/org-2/products/causal-memory/tokens/hash-1", "jwt-owner", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-org status=%d body=%v", rec.Code, body)
	}
	if got := len(tokens.revokeCalls()); got != 0 {
		t.Fatalf("cross-org request reached the token store: %v", tokens.revokeCalls())
	}

	// A member probing a hash that belongs to another org gets 404 (not 403), so
	// token existence is not leaked.
	foreign := &fakeTokenStore{revokeErr: billing.ErrNotFound}
	h = testHandler(t, testConfig(), membershipFixture(), foreign)
	rec, _ = callJSON(t, h, http.MethodDelete, "/v1/orgs/org-1/products/causal-memory/tokens/other-org-hash", "jwt-owner", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign hash status=%d", rec.Code)
	}
}

func TestRevokeProductTokenAnonymous(t *testing.T) {
	tokens := &fakeTokenStore{}
	h := testHandler(t, testConfig(), membershipFixture(), tokens)
	rec, body := callJSON(t, h, http.MethodDelete, "/v1/orgs/org-1/products/causal-memory/tokens/hash-1", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%v", rec.Code, body)
	}
	if got := len(tokens.revokeCalls()); got != 0 {
		t.Fatalf("anonymous request reached the token store")
	}
}

func TestRevokeProductTokenUnsupportedProduct(t *testing.T) {
	tokens := &fakeTokenStore{}
	h := testHandler(t, testConfig(), membershipFixture(), tokens)
	rec, body := callJSON(t, h, http.MethodDelete, "/v1/orgs/org-1/products/dslpha/tokens/hash-1", "jwt-owner", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%v", rec.Code, body)
	}
	if got := len(tokens.revokeCalls()); got != 0 {
		t.Fatalf("unsupported product reached the token store")
	}
}

// --- GET .../tokens includes last_used_at -----------------------------------

func TestListProductTokensIncludesLastUsedAt(t *testing.T) {
	usedAt := time.Date(2026, 11, 12, 8, 30, 0, 0, time.UTC)
	createdAt := usedAt.Add(-time.Hour)
	tokens := &fakeTokenStore{list: []billing.ProductToken{
		{TokenHash: "hash-used", OrgID: "org-1", Product: "causal-memory", CreatedAt: createdAt, LastUsedAt: &usedAt},
		{TokenHash: "hash-new", OrgID: "org-1", Product: "causal-memory", CreatedAt: createdAt},
	}}
	h := testHandler(t, testConfig(), membershipFixture(), tokens)

	rec, body := callJSON(t, h, http.MethodGet, "/v1/orgs/org-1/products/causal-memory/tokens", "jwt-owner", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body["mcp_url"] != "https://causal-memory.com/memory/mcp" {
		t.Fatalf("mcp_url=%v", body["mcp_url"])
	}
	list, _ := body["tokens"].([]any)
	if len(list) != 2 {
		t.Fatalf("tokens=%v", body["tokens"])
	}
	used, _ := list[0].(map[string]any)
	if used["last_used_at"] != "2026-11-12T08:30:00Z" {
		t.Fatalf("last_used_at=%v", used["last_used_at"])
	}
	fresh, _ := list[1].(map[string]any)
	if _, ok := fresh["last_used_at"]; ok {
		t.Fatalf("never-used token must omit last_used_at: %v", fresh)
	}
}
