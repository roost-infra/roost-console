package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/auth"
	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/config"
	"github.com/JingxuanC/roost-console/apps/api/internal/metrics"
	"github.com/JingxuanC/roost-console/apps/api/internal/oidc"
	"github.com/JingxuanC/roost-console/apps/api/internal/payment"
	"github.com/JingxuanC/roost-console/apps/api/internal/ratelimit"
)

// orderAdmin is the admin/reconciliation slice of the billing store: manual
// fulfillment and provider-order lookup.
type orderAdmin interface {
	GetOrderByID(ctx context.Context, orderID string) (billing.Order, error)
	Fulfill(ctx context.Context, orderID, providerTradeNo string, amountCents int, raw []byte) error
}

// orgMembership is the authorization slice of the billing store used by
// requireOrg. It is an interface so handler tests do not need Postgres.
type orgMembership interface {
	Membership(ctx context.Context, orgID, userID string) (string, error)
}

// productTokenAdmin is the slice of the billing store used by the
// causal-memory product-token endpoints.
type productTokenAdmin interface {
	ListProductTokens(ctx context.Context, orgID, product string) ([]billing.ProductToken, error)
	RevokeProductToken(ctx context.Context, orgID, product, tokenHash string) error
}

type Server struct {
	cfg      config.Config
	store    *billing.Store
	verifier *auth.Verifier
	pay      *payment.Client
	// usdt is the optional direct-USDT channel (payer sends to our own OKX
	// address). Nil or unconfigured removes it from the checkout options.
	usdt   *payment.OKXUSDT
	log    *slog.Logger
	signer *oidc.Signer

	// Test seams. New() wires both to store; production paths fall back to
	// store when the fields are nil.
	membership     orgMembership
	productTokens  productTokenAdmin
	relay          relayBilling
	compression    compressionUsageStore
	usage          usageAnalyticsStore
	overview       adminOverviewStore
	sandboxUsage   sandboxUsageRecorder
	billingDetails billingDetailsStore
	referrals      referralBilling
	adminGift      adminGiftBilling
	adminReferral  adminReferralBilling
	orderAdmin     orderAdmin

	// Public path prefixes this deployment is mounted under (e.g. "/console-api"
	// behind the nginx prefix strip). First-party clients sign the public path;
	// the API sees the stripped one, so both are accepted for the same secret.
	signingPrefixes []string
}

type ctxKey string

const userKey ctxKey = "user"

func New(cfg config.Config, store *billing.Store, verifier *auth.Verifier, pay *payment.Client, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	signer, _ := oidc.NewSigner()
	s := &Server{cfg: cfg, store: store, verifier: verifier, pay: pay, log: log, signer: signer}
	s.usdt = usdtChannelFromConfig(cfg)
	s.signingPrefixes = signingPathPrefixes(cfg.PublicBaseURL)
	if store != nil {
		s.membership = store
		s.productTokens = store
		s.relay = store
		s.compression = store
		s.usage = store
		s.overview = store
		s.sandboxUsage = store
		s.billingDetails = store
		s.referrals = store
		s.adminGift = store
		s.adminReferral = store
		s.orderAdmin = store
	}
	return s
}

// signingPathPrefixes returns the public mount path ("" for a root mount) that
// a deployment's proxy strips before a request reaches this process.
func signingPathPrefixes(publicBase string) []string {
	u, err := url.Parse(strings.TrimSpace(publicBase))
	if err != nil {
		return nil
	}
	prefix := strings.TrimRight(u.Path, "/")
	if prefix == "" || prefix == "/" {
		return nil
	}
	return []string{prefix}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	// Public CLI bootstrap: no auth, no secrets (see docs/CLI.md).
	mux.HandleFunc("GET /v1/cli/config", s.cliConfig)
	mux.HandleFunc("GET /v1/me", s.withUser(s.me))
	mux.HandleFunc("GET /v1/billing/plans", s.withUser(s.plans))
	mux.HandleFunc("POST /v1/orgs/{orgID}/products/{product}/token", s.withUser(s.rateLimit("product_token", s.cfg.RateProductToken, s.userKey, s.issueProductToken)))
	mux.HandleFunc("GET /v1/orgs/{orgID}/products/{product}/tokens", s.withUser(s.listProductTokens))
	mux.HandleFunc("DELETE /v1/orgs/{orgID}/products/{product}/tokens/{tokenHash}", s.withUser(s.revokeProductToken))
	mux.HandleFunc("POST /internal/v1/products/{product}/tokens", s.withService(s.internalProductTokens))
	// Internal operations (admin allowlist by email).
	mux.HandleFunc("GET /v1/admin/overview", s.withUser(s.adminOnly(s.adminOverview)))
	mux.HandleFunc("GET /v1/admin/orgs", s.withUser(s.adminOnly(s.adminSearchOrgs)))
	mux.HandleFunc("GET /v1/admin/orgs/{orgID}", s.withUser(s.adminOnly(s.adminGetOrg)))
	mux.HandleFunc("POST /v1/admin/orgs/{orgID}/credits", s.withUser(s.adminOnly(s.adminAdjustCredits)))
	mux.HandleFunc("POST /v1/admin/orgs/{orgID}/gift", s.withUser(s.adminOnly(s.adminGiftCredits)))
	mux.HandleFunc("GET /v1/admin/orgs/{orgID}/ledger", s.withUser(s.adminOnly(s.adminLedger)))
	mux.HandleFunc("GET /v1/admin/orgs/{orgID}/orders", s.withUser(s.adminOnly(s.adminOrders)))
	mux.HandleFunc("POST /v1/admin/orders/{orderID}/fulfill", s.withUser(s.adminOnly(s.adminFulfillOrder)))
	mux.HandleFunc("GET /v1/admin/referrals", s.withUser(s.adminOnly(s.adminReferrals)))
	// Referral program (user session; code creation is owner-only).
	mux.HandleFunc("GET /v1/referral", s.withUser(s.referralOverview))
	mux.HandleFunc("POST /v1/referral/claim", s.withUser(s.rateLimit("referral_claim", s.cfg.RateReferralClaim, s.userKey, s.claimReferral)))
	mux.HandleFunc("GET /v1/orgs/{orgID}/billing", s.withUser(s.billingView))
	mux.HandleFunc("GET /v1/orgs/{orgID}/billing/compression", s.withUser(s.orgCompressionUsage))
	mux.HandleFunc("GET /v1/orgs/{orgID}/billing/details", s.withUser(s.orgBillingDetails))
	mux.HandleFunc("GET /v1/orgs/{orgID}/usage/summary", s.withUser(s.orgUsageSummary))
	mux.HandleFunc("GET /v1/orgs/{orgID}/usage/daily", s.withUser(s.orgUsageDaily))
	mux.HandleFunc("GET /v1/orgs/{orgID}/usage/recent", s.withUser(s.orgUsageRecent))
	mux.HandleFunc("GET /v1/billing/orders", s.withUser(s.listOrders))
	mux.HandleFunc("POST /v1/billing/orders", s.withUser(s.createOrder))
	mux.HandleFunc("GET /v1/billing/orders/{orderID}", s.withUser(s.getOrder))
	mux.HandleFunc("POST /v1/payments/xunhupay/notify", s.rateLimit("payment_notify", s.cfg.RatePaymentNotify, ipKey, s.xunhuNotify))
	mux.HandleFunc("POST /v1/dev/orders/{orderID}/pay", s.withUser(s.devPay))
	mux.HandleFunc("GET /.well-known/openid-configuration", s.oidcDiscovery)
	mux.HandleFunc("GET /oauth/authorize", s.oidcAuthorizeRedirect)
	mux.HandleFunc("POST /oauth/token", s.rateLimit("oidc_token", s.cfg.RateOIDCToken, ipKey, s.oidcToken))
	mux.HandleFunc("GET /oauth/userinfo", s.oidcUserinfo)
	mux.HandleFunc("GET /oauth/jwks.json", s.oidcJWKS)
	mux.HandleFunc("POST /v1/oauth/authorize", s.withUser(s.oidcApprove))
	mux.HandleFunc("GET /api/auth/billing/leases/capability", s.withCompatService(s.compatCapability))
	mux.HandleFunc("POST /api/auth/validate", s.withCompatService(s.compatValidate))
	mux.HandleFunc("POST /api/auth/billing/leases/acquire", s.withCompatService(s.compatAcquire))
	mux.HandleFunc("POST /api/auth/billing/leases/release", s.withCompatService(s.compatRelease))
	// Relay (API marketplace) platform contract.
	mux.HandleFunc("POST /v1/relay/api-keys/introspect", s.withRelayService(s.relayIntrospect))
	mux.HandleFunc("POST /v1/relay/reservations", s.withRelayService(s.relayReserve))
	mux.HandleFunc("POST /v1/relay/reservations/{reservationID}/settle", s.withRelayService(s.relaySettle))
	mux.HandleFunc("POST /v1/relay/reservations/{reservationID}/release", s.withRelayService(s.relayRelease))
	mux.HandleFunc("GET /v1/relay/reservations/{reservationID}/tool-charges", s.withRelayService(s.relayReservationToolCharges))
	mux.HandleFunc("GET /v1/relay/operations/{idempotencyKey}", s.withRelayService(s.relayOperation))

	mux.HandleFunc("POST /internal/v1/orgs/resolve-user", s.withService(s.resolveUserOrg))
	mux.HandleFunc("POST /internal/v1/entitlements/resolve", s.withService(s.resolveEntitlement))
	mux.HandleFunc("POST /internal/v1/usage/leases", s.withService(s.reserve))
	mux.HandleFunc("POST /internal/v1/usage/leases/by-run/settle", s.withService(s.settleByRun))
	mux.HandleFunc("POST /internal/v1/usage/leases/by-run/release", s.withService(s.releaseByRun))
	mux.HandleFunc("POST /internal/v1/usage/leases/by-run/draw", s.withService(s.drawByRun))
	mux.HandleFunc("POST /internal/v1/usage/charge", s.withService(s.charge))
	mux.HandleFunc("POST /internal/v1/usage/sandbox", s.withService(s.recordSandboxUsage))
	mux.HandleFunc("POST /internal/v1/usage/leases/{leaseID}/settle", s.withService(s.settle))
	mux.HandleFunc("POST /internal/v1/usage/leases/{leaseID}/release", s.withService(s.release))
	return s.observe(s.cors(mux))
}

// observe records one Prometheus observation and one structured request log per
// request. Route is the matched ServeMux pattern, so IDs never become labels.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-Id", requestID)
		sw := &statusWriter{ResponseWriter: w}
		metrics.Default.AddInFlight(1)
		next.ServeHTTP(sw, r)
		metrics.Default.AddInFlight(-1)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		route := r.Pattern
		if route == "" {
			route = "other"
		}
		elapsed := time.Since(start)
		metrics.Default.ObserveHTTP(r.Method, route, status, elapsed.Seconds())
		if s.log != nil {
			s.log.Info("request",
				"request_id", requestID,
				"method", r.Method,
				"route", route,
				"status", status,
				"duration_ms", elapsed.Milliseconds(),
				"bytes", sw.bytes,
			)
		}
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "roost-console", "time": time.Now().UTC()})
}

func (s *Server) oidcIssuer() string {
	return strings.TrimRight(strings.TrimSpace(s.cfg.OIDCIssuer), "/")
}

// cliConfig is the public bootstrap document for roost-cli and desktop tooling.
// It carries publishable values only: the Supabase project URL + anon key (public
// by design), platform API URLs and the OIDC endpoints. Service secrets never
// appear here; see docs/CLI.md.
func (s *Server) cliConfig(w http.ResponseWriter, _ *http.Request) {
	issuer := s.oidcIssuer()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"supabase_url":      strings.TrimRight(strings.TrimSpace(s.cfg.SupabaseURL), "/"),
		"supabase_anon_key": strings.TrimSpace(s.cfg.SupabaseAnonKey),
		"console_api_url":   strings.TrimRight(strings.TrimSpace(s.cfg.ConsoleAPIURL), "/"),
		"market_api_url":    strings.TrimRight(strings.TrimSpace(s.cfg.MarketAPIURL), "/"),
		"memory_mcp_url":    s.causalMemoryMCPURL(),
		"oidc": map[string]any{
			"issuer":                 issuer,
			"authorization_endpoint": issuer + "/oauth/authorize",
			"token_endpoint":         issuer + "/oauth/token",
		},
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin == s.cfg.ConsoleOrigin || strings.HasPrefix(origin, "http://127.0.0.1:") || strings.HasPrefix(origin, "http://localhost:")
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type,X-Requested-With")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimit(name string, perMinute int, key func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	limiter := ratelimit.New(perMinute, time.Minute)
	return func(w http.ResponseWriter, r *http.Request) {
		ok, wait := limiter.Allow(key(r), time.Now())
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeError(w, http.StatusTooManyRequests, "rate_limited", name+" rate limit exceeded")
			return
		}
		next(w, r)
	}
}

func ipKey(r *http.Request) string {
	if v := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) userKey(r *http.Request) string {
	if u := userFrom(r); u != nil {
		return "user:" + u.ID
	}
	return "ip:" + ipKey(r)
}

func (s *Server) withUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		user, err := s.verifier.User(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid session")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	}
}

func (s *Server) withService(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "could not read body")
			return
		}
		service := strings.TrimSpace(r.Header.Get("X-Roost-Service"))
		if !s.verifyService(service, r, body) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "bad service signature")
			return
		}
		if !serviceAllows(service, r.Method, r.URL.Path) {
			writeError(w, http.StatusForbidden, "forbidden", "service scope does not allow this endpoint")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		next(w, r)
	}
}

func (s *Server) verifyService(service string, r *http.Request, body []byte) bool {
	if service == "" {
		return false
	}
	for _, secret := range s.cfg.ServiceSecrets[service] {
		if secret == "" {
			continue
		}
		if auth.VerifyServiceSignature(r, body, secret) {
			return true
		}
		// The proxy in front of this API strips the public mount prefix, but a
		// first-party client signs the path it called. Accept that form too.
		for _, prefix := range s.signingPrefixes {
			if auth.VerifyServiceSignatureForURI(r, body, secret, prefix+r.URL.RequestURI()) {
				return true
			}
		}
	}
	return false
}

// verifyLegacyServiceToken accepts the pre-HMAC service token first-party
// builds still send during migration. It is the same shared secret, compared in
// constant time; new builds send the signed headers and take the HMAC path.
func (s *Server) verifyLegacyServiceToken(service, token string) bool {
	if token == "" {
		return false
	}
	for _, secret := range s.cfg.ServiceSecrets[service] {
		if secret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

func serviceAllows(service, method, path string) bool {
	allowed, ok := serviceScopes[service]
	if !ok {
		return false
	}
	for _, prefix := range allowed {
		if prefix == "*" || path == prefix || strings.HasPrefix(path, strings.TrimRight(prefix, "/")+"/") {
			return true
		}
	}
	return false
}

var serviceScopes = map[string][]string{
	"market": {
		"/internal/v1/orgs/resolve-user",
		"/internal/v1/entitlements/resolve",
		"/internal/v1/usage/leases",
		"/internal/v1/usage/sandbox",
	},
	"dslpha": {
		"/api/auth/validate",
		"/api/auth/billing/leases",
		"/internal/v1/orgs/resolve-user",
		"/internal/v1/usage/charge",
		"/internal/v1/usage/sandbox",
	},
	"causal":      {"/internal/v1/usage/charge"},
	"causal-sync": {"/internal/v1/products/causal-memory/tokens"},
	"relay":       {"/v1/relay/", "/internal/v1/products/causal-memory/tokens"},
	"root":        {"*"},
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

func userFrom(r *http.Request) *auth.User {
	u, _ := r.Context().Value(userKey).(*auth.User)
	return u
}

func (s *Server) requireOrg(w http.ResponseWriter, r *http.Request, orgID string) bool {
	membership := s.membership
	if membership == nil && s.store != nil {
		membership = s.store
	}
	u := userFrom(r)
	if u == nil || membership == nil {
		writeError(w, http.StatusForbidden, "forbidden", "not a member of this organization")
		return false
	}
	role, err := membership.Membership(r.Context(), orgID, u.ID)
	if err != nil || role == "" {
		writeError(w, http.StatusForbidden, "forbidden", "not a member of this organization")
		return false
	}
	return true
}

func (s *Server) issueProductToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	if !s.requireOrg(w, r, orgID) {
		return
	}
	product := r.PathValue("product")
	if product != "causal-memory" && product != "relay" {
		writeError(w, http.StatusBadRequest, "unsupported_product", "token issuance is only available for causal-memory and relay")
		return
	}
	// The route requires a user session, so every token issued here is bound to
	// the authenticated user. Legacy tokens (created before user binding) keep a
	// nil created_by_user_id and remain org-scoped.
	userID := ""
	if u := userFrom(r); u != nil {
		userID = u.ID
	}
	token, meta, err := s.store.IssueProductToken(r.Context(), orgID, product, userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if product == "causal-memory" {
		s.pushCausalToken(r.Context(), meta.TokenHash, orgID)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": token, "token_hash": meta.TokenHash, "org_id": orgID, "product": product,
		"mcp_url": s.causalMemoryMCPURL(),
	})
}

func (s *Server) listProductTokens(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	if !s.requireOrg(w, r, orgID) {
		return
	}
	product := r.PathValue("product")
	service := s.productTokenService()
	if service == nil {
		writeError(w, http.StatusServiceUnavailable, "internal_error", "token store is not configured")
		return
	}
	tokens, err := service.ListProductTokens(r.Context(), orgID, product)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens, "mcp_url": s.causalMemoryMCPURL()})
}

// revokeProductToken revokes one causal-memory token of the caller's org.
//
// Requires a Supabase user session and org membership. The token hash comes from
// the stored listing (the raw cm_ token is never stored). Revocation is
// idempotent for an already-revoked token of this org; a hash of another org or
// an unknown hash is 404 so existence is not leaked.
func (s *Server) revokeProductToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	if !s.requireOrg(w, r, orgID) {
		return
	}
	product := r.PathValue("product")
	if product != "causal-memory" && product != "relay" {
		writeError(w, http.StatusBadRequest, "unsupported_product", "token revocation is only available for causal-memory and relay")
		return
	}
	tokenHash := strings.TrimSpace(r.PathValue("tokenHash"))
	if tokenHash == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "token hash is required")
		return
	}
	service := s.productTokenService()
	if service == nil {
		writeError(w, http.StatusServiceUnavailable, "internal_error", "token store is not configured")
		return
	}
	if err := service.RevokeProductToken(r.Context(), orgID, product, tokenHash); err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "token not found")
			return
		}
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) internalProductTokens(w http.ResponseWriter, r *http.Request) {
	product := r.PathValue("product")
	if product != "causal-memory" {
		writeError(w, http.StatusBadRequest, "unsupported_product", "unsupported product")
		return
	}
	if r.Method == http.MethodPost {
		// Relay-scoped issuance: the relay needs an org memory token to call
		// causal-memory on the user's behalf. The token is returned once and
		// never stored raw; the relay caches it per org and must not log it.
		var req struct {
			OrgID string `json:"org_id"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid body")
			return
		}
		orgID := strings.TrimSpace(req.OrgID)
		// causal-sync polls with an empty body to fetch the hash list (the
		// legacy POST shape); only relay-scoped calls ask for issuance.
		if orgID == "" && strings.TrimSpace(r.Header.Get("X-Roost-Service")) == "causal-sync" {
			tokens, err := s.store.ListAllProductTokens(r.Context(), product)
			if err != nil {
				s.fail(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
			return
		}
		if orgID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "org_id is required")
			return
		}
		// Service-authenticated issuance has no console user session: store
		// NULL, the same shape as a legacy org-scoped token.
		token, meta, err := s.store.IssueProductToken(r.Context(), orgID, product, "")
		if err != nil {
			s.fail(w, err)
			return
		}
		s.pushCausalToken(r.Context(), meta.TokenHash, orgID)
		writeJSON(w, http.StatusCreated, map[string]any{
			"token": token, "token_hash": meta.TokenHash, "org_id": orgID, "product": product,
			"mcp_url": s.causalMemoryMCPURL(),
		})
		return
	}
	tokens, err := s.store.ListAllProductTokens(r.Context(), product)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// pushCausalToken makes a newly issued causal-memory token usable immediately
// instead of waiting for the 30s hash sync. The push is signed as causal-sync
// and carries only the hash, never the raw token. Failure is non-fatal: the
// periodic sync and the relay's 401 retry remain the fallback.
func (s *Server) pushCausalToken(ctx context.Context, tokenHash, orgID string) {
	url := strings.TrimSpace(s.cfg.MemoryTokenPushURL)
	if url == "" || tokenHash == "" || orgID == "" {
		return
	}
	body, err := json.Marshal(map[string]string{"token_hash": tokenHash, "org_id": orgID})
	if err != nil {
		return
	}
	secret := ""
	for _, candidate := range []string{"causal-sync", "causal"} {
		if values := s.cfg.ServiceSecrets[candidate]; len(values) > 0 {
			secret = values[0]
			break
		}
	}
	if secret == "" {
		return
	}
	ts, nonce, sig := auth.SignService("causal-sync", http.MethodPost, "/__token", body, secret, time.Now())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Roost-Service", "causal-sync")
	req.Header.Set("X-Roost-Timestamp", ts)
	req.Header.Set("X-Roost-Nonce", nonce)
	req.Header.Set("X-Roost-Signature", sig)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.log.Warn("causal token push failed", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		s.log.Warn("causal token push rejected", "status", resp.StatusCode)
	}
}

func (s *Server) causalMemoryMCPURL() string {
	if v := strings.TrimSpace(s.cfg.MemoryMCPURL); v != "" {
		return v
	}
	return "https://causal-memory.com/memory/mcp"
}

// productTokenService falls back to the real store when no test seam is set.
func (s *Server) productTokenService() productTokenAdmin {
	if s.productTokens != nil {
		return s.productTokens
	}
	if s.store == nil {
		return nil
	}
	return s.store
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	org, err := s.store.EnsureOrg(r.Context(), u.ID, u.Email)
	if err != nil {
		s.fail(w, err)
		return
	}
	view, err := s.store.Billing(r.Context(), org.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": u, "org": org, "billing": view,
		"payment_methods": s.paymentMethods(u),
	})
}

func (s *Server) plans(w http.ResponseWriter, r *http.Request) {
	plans, err := s.store.Plans(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plans": plans})
}

// usdtChannelFromConfig builds the direct-USDT channel from environment
// config. A missing rate or address leaves it nil, which hides the option.
func usdtChannelFromConfig(cfg config.Config) *payment.OKXUSDT {
	if !cfg.OKXUSDTConfigured() {
		return nil
	}
	client := &payment.OKXClient{
		BaseURL:    cfg.OKXAPIBase,
		Key:        cfg.OKXAPIKey,
		Secret:     cfg.OKXAPISecret,
		Passphrase: cfg.OKXAPIPassphrase,
		ChainCodes: map[string]string{
			payment.ChainTRC20:   "TRC20",
			payment.ChainERC20:   "ERC20",
			payment.ChainPolygon: "Polygon",
		},
	}
	return &payment.OKXUSDT{
		Addresses: map[string]string{
			payment.ChainTRC20:   cfg.OKXUSDTAddressTRC20,
			payment.ChainERC20:   cfg.OKXUSDTAddressERC20,
			payment.ChainPolygon: cfg.OKXUSDTAddressPolygon,
		},
		Rate:     cfg.OKXUSDTCNYRate,
		OrderTTL: cfg.OKXUSDTOrderTTL,
		Client:   client,
	}
}

// WithUSDT replaces the direct-USDT channel; nil disables it. Tests use it.
func (s *Server) WithUSDT(c *payment.OKXUSDT) *Server {
	s.usdt = c
	return s
}

// usdtUsable reports whether this account may pay in USDT right now.
func (s *Server) usdtUsable(u *auth.User) bool {
	if s.usdt == nil || !s.usdt.Configured() {
		return false
	}
	if len(s.cfg.OKXUSDTAllowEmails) == 0 {
		return true
	}
	if u == nil {
		return false
	}
	for _, allowed := range s.cfg.OKXUSDTAllowEmails {
		if strings.EqualFold(strings.TrimSpace(u.Email), allowed) {
			return true
		}
	}
	return false
}

// paymentMethods lists the checkout channels this account can use.
func (s *Server) paymentMethods(u *auth.User) []string {
	methods := []string{"online"}
	if s.usdtUsable(u) {
		methods = append(methods, payment.ProviderOKXUSDT)
	}
	return methods
}

// usdtQuote picks the first enabled chain and quotes an amount no other open
// order holds. Uniqueness is what lets a single shared address reconcile.
func (s *Server) usdtQuote(ctx context.Context, chain string, amountCents int64) (payment.USDTQuote, error) {
	chains := s.usdt.Chains()
	if len(chains) == 0 {
		return payment.USDTQuote{}, payment.ErrNoChain
	}
	if chain == "" {
		chain = chains[0]
	}
	found := false
	for _, enabled := range chains {
		if strings.EqualFold(enabled, chain) {
			chain, found = enabled, true
			break
		}
	}
	if !found {
		return payment.USDTQuote{}, fmt.Errorf("unsupported chain %q", chain)
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		suffix := time.Now().UnixNano()%99 + int64(attempt)*7
		quote, err := s.usdt.Quote(chain, amountCents, suffix)
		if err != nil {
			return payment.USDTQuote{}, err
		}
		used, err := s.store.USDTAmountInUse(ctx, payment.ProviderOKXUSDT, chain, quote.Amount)
		if err != nil {
			return payment.USDTQuote{}, err
		}
		if !used {
			return quote, nil
		}
		lastErr = errors.New("amount collision")
	}
	if lastErr == nil {
		lastErr = errors.New("amount collision")
	}
	return payment.USDTQuote{}, lastErr
}

func (s *Server) billingView(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	if !s.requireOrg(w, r, orgID) {
		return
	}
	view, err := s.store.Billing(r.Context(), orgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	org, err := s.store.EnsureOrg(r.Context(), u.ID, u.Email)
	if err != nil {
		s.fail(w, err)
		return
	}
	orders, err := s.store.ListOrders(r.Context(), org.ID, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders})
}

type createOrderRequest struct {
	SKU string `json:"sku"`
	// AmountCents is only read for the custom top-up SKU; every other SKU is
	// priced server-side by Quote and ignores it.
	AmountCents int64 `json:"amount_cents"`
	// Method selects the payment channel. Empty keeps the default (XunhuPay);
	// "okx_usdt" pays in USDT to our own OKX address.
	Method string `json:"method"`
	// Chain selects the USDT network for method "okx_usdt".
	Chain string `json:"chain"`
}

type orderResponse struct {
	*billing.Order
	QRURL  string `json:"qr_url,omitempty"`
	PayURL string `json:"pay_url,omitempty"`
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	org, err := s.store.EnsureOrg(r.Context(), u.ID, u.Email)
	if err != nil {
		s.fail(w, err)
		return
	}
	var req createOrderRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.SKU == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "sku is required")
		return
	}
	var order billing.Order
	var pack billing.CreditPack
	custom := req.SKU == billing.CustomTopUpSKU
	if custom {
		pack, err = billing.CustomTopUpQuote(req.AmountCents)
		if err != nil {
			if errors.Is(err, billing.ErrInvalidTopUpAmount) {
				writeError(w, http.StatusBadRequest, "invalid_amount", "custom top-up must be between ¥10 and ¥5000")
				return
			}
			s.fail(w, err)
			return
		}
	} else {
		pack, _, err = s.store.QuoteForOrg(r.Context(), org.ID, req.SKU)
		if err != nil {
			s.fail(w, err)
			return
		}
	}

	// Direct USDT: no provider call, just a quote the payer sends to our own
	// address. The order is reconcilable only because the amount is unique.
	if req.Method == payment.ProviderOKXUSDT {
		if !s.usdtUsable(u) {
			writeError(w, http.StatusBadRequest, "method_unavailable", "USDT checkout is not available")
			return
		}
		quote, err := s.usdtQuote(r.Context(), req.Chain, int64(pack.AmountCents))
		if err != nil {
			s.log.Warn("usdt quote failed", "err", err, "sku", req.SKU)
			writeError(w, http.StatusServiceUnavailable, "payment_unavailable", "USDT checkout is not available")
			return
		}
		order, err = s.store.CreateUSDTOrder(r.Context(), org.ID, req.SKU,
			int64(pack.AmountCents), pack.Credits, payment.ProviderOKXUSDT,
			quote.Chain, quote.Address, quote.Amount, quote.AmountMicro, quote.Rate, quote.ExpiresAt)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"order": order})
		return
	}

	if custom {
		order, err = s.store.CreateCustomOrder(r.Context(), org.ID, req.AmountCents)
	} else {
		order, err = s.store.CreateOrder(r.Context(), org.ID, req.SKU)
	}
	if err != nil {
		if errors.Is(err, billing.ErrTrialUsed) {
			writeError(w, http.StatusConflict, "trial_used", "trial already used")
			return
		}
		s.fail(w, err)
		return
	}
	if order.Status == "fulfilled" {
		writeJSON(w, http.StatusCreated, map[string]any{"order": order})
		return
	}
	if s.cfg.PaymentConfigured() {
		res, err := s.pay.Create(payment.CreateInput{OutTradeNo: order.OutTradeNo, AmountCents: pack.AmountCents, Title: pack.Title, Attach: org.ID})
		if err != nil {
			s.log.Error("xunhupay create failed", "err", err, "order", order.ID)
			// The row is already persisted; leaving it 'created' makes an
			// abandoned checkout look like an in-progress order forever.
			if markErr := s.store.MarkOrderFailed(r.Context(), order.ID, err.Error()); markErr != nil {
				s.log.Warn("mark order failed", "order", order.ID, "err", markErr)
			}
			writeError(w, http.StatusBadGateway, "payment_unavailable", "payment provider unavailable")
			return
		}
		if err := s.store.AttachPayment(r.Context(), order.ID, res.ProviderOrderID, res.QRURL, res.PayURL); err != nil {
			s.fail(w, err)
			return
		}
		order.QRURL, order.PayURL, order.Status = res.QRURL, res.PayURL, "pending"
	} else if s.cfg.Environment == "development" {
		// Local development without merchant credentials: expose an explicit
		// dev-only payment action instead of pretending a QR code exists.
		_ = s.store.AttachPayment(r.Context(), order.ID, "dev_"+order.ID, "", s.cfg.PublicBaseURL+"/dev-pay?order="+order.ID)
		order.PayURL, order.Status = s.cfg.PublicBaseURL+"/dev-pay?order="+order.ID, "pending"
	} else {
		writeError(w, http.StatusServiceUnavailable, "payment_not_configured", "payment provider is not configured")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"order": order})
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	org, err := s.store.EnsureOrg(r.Context(), u.ID, u.Email)
	if err != nil {
		s.fail(w, err)
		return
	}
	o, err := s.store.GetOrder(r.Context(), org.ID, r.PathValue("orderID"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": o})
}

func (s *Server) devPay(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Environment != "development" {
		http.NotFound(w, r)
		return
	}
	orderID := r.PathValue("orderID")
	u := userFrom(r)
	if u == nil {
		var req struct {
			Email string `json:"email"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeError(w, http.StatusUnauthorized, "unauthorized", "dev pay requires a session")
		return
	}
	org, err := s.store.EnsureOrg(r.Context(), u.ID, u.Email)
	if err != nil {
		s.fail(w, err)
		return
	}
	o, err := s.store.GetOrder(r.Context(), org.ID, orderID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.Fulfill(r.Context(), o.ID, "dev_"+o.ID, o.AmountCents, []byte(`{"dev":true}`)); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) xunhuNotify(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !payment.Verify(r.PostForm, s.cfg.XunhuAppSecret) {
		s.log.Warn("xunhupay notify bad signature")
		http.Error(w, "bad sign", http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("status") != "OD" {
		// Refunds are handled manually in v1; acknowledge and leave the order.
		_, _ = w.Write([]byte("success"))
		return
	}
	tradeNo := r.PostForm.Get("trade_order_id")
	o, err := s.store.OrderByTradeNo(r.Context(), tradeNo)
	if err != nil {
		s.log.Warn("xunhupay notify unknown order", "trade_no", tradeNo)
		http.Error(w, "unknown order", http.StatusNotFound)
		return
	}
	amount, err := payment.AmountCentsFromYuan(r.PostForm.Get("total_fee"))
	if err != nil {
		http.Error(w, "bad amount", http.StatusBadRequest)
		return
	}
	raw, _ := json.Marshal(r.PostForm)
	if err := s.store.Fulfill(r.Context(), o.ID, r.PostForm.Get("transaction_id"), amount, raw); err != nil {
		s.log.Error("xunhupay fulfill failed", "order", o.ID, "err", err)
		http.Error(w, "fulfill failed", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte("success"))
}

type leaseRequest struct {
	OrgID            string `json:"org_id"`
	Product          string `json:"product"`
	RunRef           string `json:"run_ref"`
	EstimatedCredits int64  `json:"estimated_credits"`
	TTLSeconds       int    `json:"ttl_seconds"`
}

func (s *Server) redirectURIAllowed(uri string) bool {
	for _, allowed := range strings.Split(s.cfg.OIDCRedirectURIs, ",") {
		if strings.TrimSpace(allowed) != "" && strings.TrimSpace(allowed) == uri {
			return true
		}
	}
	return false
}

func (s *Server) oidcDiscovery(w http.ResponseWriter, _ *http.Request) {
	issuer := s.oidcIssuer()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                           issuer,
		"authorization_endpoint":           issuer + "/oauth/authorize",
		"token_endpoint":                   issuer + "/oauth/token",
		"userinfo_endpoint":                issuer + "/oauth/userinfo",
		"response_types_supported":         []string{"code"},
		"subject_types_supported":          []string{"public"},
		"scopes_supported":                 []string{"openid", "email", "profile"},
		"code_challenge_methods_supported": []string{"S256"},
		"jwks_uri":                         issuer + "/oauth/jwks.json",
	})
}

func (s *Server) oidcJWKS(w http.ResponseWriter, _ *http.Request) {
	if s.signer == nil {
		http.Error(w, "oidc not available", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, s.signer.JWKS())
}

func (s *Server) oidcAuthorizeRedirect(w http.ResponseWriter, r *http.Request) {
	if s.cfg.OIDCConsentURL == "" {
		http.Error(w, "oauth not configured", http.StatusNotFound)
		return
	}
	target := s.cfg.OIDCConsentURL + "?" + r.URL.RawQuery
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) oidcApprove(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		ResponseType  string `json:"response_type"`
		ClientID      string `json:"client_id"`
		RedirectURI   string `json:"redirect_uri"`
		State         string `json:"state"`
		CodeChallenge string `json:"code_challenge"`
		Scope         string `json:"scope"`
		Nonce         string `json:"nonce"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid authorization body")
		return
	}
	if req.ResponseType != "code" || req.ClientID != s.cfg.OIDCClientID || !s.redirectURIAllowed(req.RedirectURI) || req.CodeChallenge == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid client, redirect_uri or PKCE challenge")
		return
	}
	org, err := s.store.EnsureOrg(r.Context(), u.ID, u.EmailOrID())
	if err != nil {
		s.fail(w, err)
		return
	}
	code, err := s.store.CreateOAuthCode(r.Context(), req.ClientID, u.ID, u.EmailOrID(), org.ID, req.RedirectURI, req.CodeChallenge, req.Nonce, 5*time.Minute)
	if err != nil {
		s.fail(w, err)
		return
	}
	sep := "?"
	if strings.Contains(req.RedirectURI, "?") {
		sep = "&"
	}
	redirect := req.RedirectURI + sep + "code=" + url.QueryEscape(code)
	if req.State != "" {
		redirect += "&state=" + url.QueryEscape(req.State)
	}
	writeJSON(w, http.StatusOK, map[string]any{"redirect_url": redirect})
}

func (s *Server) oidcToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	clientID := r.PostForm.Get("client_id")
	clientSecret := r.PostForm.Get("client_secret")
	if clientID != s.cfg.OIDCClientID || (s.cfg.OIDCClientSecret != "" && subtle.ConstantTimeCompare([]byte(clientSecret), []byte(s.cfg.OIDCClientSecret)) != 1) {
		writeError(w, http.StatusUnauthorized, "invalid_client", "invalid client")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}
	grant, err := s.store.ConsumeOAuthCode(r.Context(), r.PostForm.Get("code"), clientID, r.PostForm.Get("redirect_uri"), r.PostForm.Get("code_verifier"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_grant", "code is invalid or expired")
		return
	}
	token, expires, err := s.store.CreateOAuthToken(r.Context(), clientID, grant.UserID, grant.Email, grant.OrgID, time.Hour)
	if err != nil {
		s.fail(w, err)
		return
	}
	idToken := ""
	if s.signer != nil {
		idToken, err = s.signer.SignIDToken(s.cfg.OIDCIssuer, clientID, grant.UserID, grant.Email, grant.OrgID, grant.Nonce, time.Hour)
		if err != nil {
			s.fail(w, err)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token, "id_token": idToken, "token_type": "Bearer",
		"expires_in": int(time.Until(expires).Seconds()), "scope": "openid email profile",
	})
}

func (s *Server) oidcUserinfo(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	g, _, err := s.store.OAuthTokenUser(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_token", "invalid access token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sub": g.UserID, "email": g.Email, "email_verified": true, "org_id": g.OrgID,
	})
}

func (s *Server) withCompatService(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "could not read body")
			return
		}
		service := strings.TrimSpace(r.Header.Get("X-Roost-Service"))
		if service == "" {
			service = "dslpha"
		}
		verified := s.verifyService(service, r, body)
		if !verified {
			// Migration compatibility: older dslpha builds send only the shared
			// X-Service-Token, without the signed headers. The same secret
			// authenticates both forms; the HMAC path wins once present.
			if s.verifyLegacyServiceToken(service, strings.TrimSpace(r.Header.Get("X-Service-Token"))) {
				verified = true
				s.log.Debug("legacy service token accepted", "service", service)
			}
		}
		if !verified {
			writeError(w, http.StatusUnauthorized, "unauthorized", "bad service signature")
			return
		}
		if !serviceAllows(service, r.Method, r.URL.Path) {
			writeError(w, http.StatusForbidden, "forbidden", "service scope does not allow this endpoint")
			return
		}
		// Capability is a deployment-level probe: it has no user and its
		// answer is the same for every user, so it authenticates as the
		// service only. Every lease call does need the user.
		if r.URL.Path == "/api/auth/billing/leases/capability" {
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			next(w, r)
			return
		}
		userID := strings.TrimSpace(r.Header.Get("X-User-Id"))
		if userID == "" {
			// Older first-party builds carry the user in the body (the legacy
			// lease contract) and omit the header. Read it from there rather
			// than refusing a call the same secret just authenticated.
			var probe struct {
				UserID string `json:"user_id"`
			}
			_ = json.Unmarshal(body, &probe)
			userID = strings.TrimSpace(probe.UserID)
		}
		if userID == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "X-User-Id is required")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, &auth.User{ID: userID})))
	}
}

func (s *Server) compatCapability(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "lease_ttl_seconds": 300,
		"rate_multiplier_min": 0.1, "rate_multiplier_max": 100.0,
	})
}

func specScopes(code string) []string {
	switch code {
	case "dsalpha_plus":
		return []string{"workspace:spec:performance"}
	case "dsalpha_pro", "dsalpha_max", "pro", "max", "byok-pro":
		return []string{"workspace:spec:performance", "workspace:spec:max"}
	default:
		return nil
	}
}

func isSpecQuota(check string) bool {
	return check == "spec_performance" || check == "spec_max" || check == "spec_xlarge"
}

func planAllowsSpec(code, check string) bool {
	switch check {
	case "spec_performance":
		return code == "dsalpha_plus" || code == "dsalpha_pro" || code == "dsalpha_max" || code == "pro" || code == "max" || code == "byok-pro"
	case "spec_max":
		return code == "dsalpha_pro" || code == "dsalpha_max" || code == "byok-pro"
	default:
		return false
	}
}

func accessTier(code string) int {
	switch code {
	case "max", "dsalpha_max":
		return 3
	case "pro", "byok-pro", "dsalpha_pro":
		return 2
	case "starter", "dsalpha_plus":
		return 1
	default:
		return 0
	}
}

func (s *Server) compatValidate(w http.ResponseWriter, r *http.Request) {
	userID := userFrom(r).ID
	org, err := s.store.EnsureOrgByUserID(r.Context(), userID)
	if err != nil {
		s.fail(w, err)
		return
	}
	e, err := s.store.Resolve(r.Context(), org.ID, "dsalpha")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"access_tier": -1, "plan_display_name": "", "scopes": []string{}, "quota": map[string]any{"allowed": false, "limit_type": "plan", "message": "Roost 组织未开通投研产品"}})
		return
	}
	var req struct {
		CheckQuota string `json:"check_quota"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	allowed := e.Available > 0
	limitType := "credit_limit"
	if req.CheckQuota == "computer_count" || req.CheckQuota == "workspace" {
		allowed = true
		limitType = "workspace_limit"
	}
	monthlyUsed, err := s.store.MonthlyUsage(r.Context(), org.ID, "dsalpha")
	if err != nil {
		s.fail(w, err)
		return
	}
	quota := map[string]any{
		"allowed": allowed, "limit_type": limitType,
		"used_credits": monthlyUsed, "credit_limit": e.Balance,
		"remaining_credits": e.Available, "reserved_credits": e.Reserved,
		"message": "Roost 积分不足，请充值",
	}
	scopes := make([]string, 0, 3)
	if s.store.AlwaysOnActive(r.Context(), org.ID) {
		scopes = append(scopes, "workspace:always_on")
	}
	scopes = append(scopes, specScopes(e.PlanCode)...)

	// Elevated computer sizes are scope-gated, not credit-gated. The Console
	// plan owns which sizes exist; dslalpha enforces them through these scopes
	// and the spec_* count quota names. A missing scope is why every paid plan
	// looked like Small-only in the picker.
	if isSpecQuota(req.CheckQuota) {
		allowed := planAllowsSpec(e.PlanCode, req.CheckQuota)
		quota = map[string]any{
			"allowed": allowed, "limit_type": req.CheckQuota,
			"capacity_used": 0, "capacity_limit": map[bool]int{true: -1, false: 0}[allowed],
			"message": "当前套餐不包含该电脑规格，请升级套餐",
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_tier": accessTier(e.PlanCode), "plan_display_name": e.PlanCode,
		"scopes": scopes, "quota": quota,
	})
}

func (s *Server) compatAcquire(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID       string  `json:"user_id"`
		RunRef       string  `json:"run_ref"`
		SpentCredits float64 `json:"spent_credits"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RunRef == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "run_ref is required")
		return
	}
	org, err := s.store.EnsureOrgByUserID(r.Context(), req.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	estimate := int64(math.Ceil(math.Max(200, req.SpentCredits*2+200)))
	lease, err := s.store.Reserve(r.Context(), org.ID, "dsalpha", req.RunRef, estimate, 300*time.Second)
	if err != nil {
		if errors.Is(err, billing.ErrInsufficientCredits) || errors.Is(err, billing.ErrProductCap) || errors.Is(err, billing.ErrProductNotEnabled) {
			writeJSON(w, http.StatusOK, map[string]any{"granted": false, "quota": map[string]any{"allowed": false, "message": "Roost 积分不足，请充值", "limit_type": "credit_limit"}})
			return
		}
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"granted": true, "ceiling_credits": lease.ReservedCredits,
		"expires_in_seconds": 300, "generation": 1,
		"quota": map[string]any{"allowed": true},
	})
}

func (s *Server) compatRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID       string  `json:"user_id"`
		RunRef       string  `json:"run_ref"`
		SpentCredits float64 `json:"spent_credits"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RunRef == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "run_ref is required")
		return
	}
	org, err := s.store.EnsureOrgByUserID(r.Context(), req.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	actual := int64(math.Ceil(math.Max(0, req.SpentCredits)))
	if actual > 0 {
		_, err = s.store.SettleByRun(r.Context(), org.ID, "dsalpha", req.RunRef, actual)
	} else {
		err = s.store.ReleaseByRun(r.Context(), org.ID, "dsalpha", req.RunRef)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"released": true})
}

func (s *Server) resolveUserOrg(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "user_id is required")
		return
	}
	org, err := s.store.OrgForUser(r.Context(), req.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, org)
}

func (s *Server) resolveEntitlement(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrgID   string `json:"org_id"`
		Product string `json:"product"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OrgID == "" || req.Product == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "org_id and product are required")
		return
	}
	e, err := s.store.Resolve(r.Context(), req.OrgID, req.Product)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	var req leaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OrgID == "" || req.Product == "" || req.RunRef == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "org_id, product and run_ref are required")
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	l, err := s.store.Reserve(r.Context(), req.OrgID, req.Product, req.RunRef, req.EstimatedCredits, ttl)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (s *Server) settle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ActualCredits int64 `json:"actual_credits"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "actual_credits is required")
		return
	}
	l, err := s.store.Settle(r.Context(), r.PathValue("leaseID"), req.ActualCredits)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) charge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrgID   string `json:"org_id"`
		Product string `json:"product"`
		Ref     string `json:"ref"`
		Credits int64  `json:"credits"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OrgID == "" || req.Product == "" || req.Ref == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "org_id, product and ref are required")
		return
	}
	ok, err := s.store.Charge(r.Context(), req.OrgID, req.Product, req.Ref, req.Credits)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"charged": ok})
}

func (s *Server) settleByRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrgID         string `json:"org_id"`
		Product       string `json:"product"`
		RunRef        string `json:"run_ref"`
		ActualCredits int64  `json:"actual_credits"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OrgID == "" || req.Product == "" || req.RunRef == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "org_id, product and run_ref are required")
		return
	}
	found, err := s.store.SettleByRun(r.Context(), req.OrgID, req.Product, req.RunRef, req.ActualCredits)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settled": found})
}

func (s *Server) drawByRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrgID   string `json:"org_id"`
		Product string `json:"product"`
		RunRef  string `json:"run_ref"`
		Ref     string `json:"ref"`
		Credits int64  `json:"credits"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OrgID == "" || req.Product == "" || req.RunRef == "" || req.Ref == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "org_id, product, run_ref and ref are required")
		return
	}
	ok, err := s.store.DrawByRun(r.Context(), req.OrgID, req.Product, req.RunRef, req.Ref, req.Credits)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drawn": ok})
}

func (s *Server) releaseByRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrgID   string `json:"org_id"`
		Product string `json:"product"`
		RunRef  string `json:"run_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OrgID == "" || req.Product == "" || req.RunRef == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "org_id, product and run_ref are required")
		return
	}
	if err := s.store.ReleaseByRun(r.Context(), req.OrgID, req.Product, req.RunRef); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"released": true})
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Release(r.Context(), r.PathValue("leaseID")); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"released": true})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, billing.ErrTrialUsed):
		writeError(w, http.StatusConflict, "trial_used", "trial already used")
	case errors.Is(err, billing.ErrInsufficientCredits):
		writeError(w, http.StatusPaymentRequired, "insufficient_credits", "insufficient credits")
	case errors.Is(err, billing.ErrProductCap):
		writeError(w, http.StatusTooManyRequests, "product_cap", "product monthly cap reached")
	case errors.Is(err, billing.ErrProductNotEnabled):
		writeError(w, http.StatusForbidden, "product_disabled", "product is not enabled")
	case errors.Is(err, billing.ErrAddonNotEligible):
		writeError(w, http.StatusBadRequest, "addon_not_eligible", "the always-on add-on requires the Pro or Max plan")
	case errors.Is(err, billing.ErrInvalidSandboxUsage):
		writeError(w, http.StatusBadRequest, "invalid_sandbox_usage", err.Error())
	default:
		s.log.Error("api error", "err", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

var _ = fmt.Sprintf
var _ = strconv.Itoa
