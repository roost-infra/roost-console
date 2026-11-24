package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultReferralInviterCredits int64 = 1000
	DefaultReferralInviteeCredits int64 = 1000
)

type Config struct {
	Port string
	// MetricsListen is a separate loopback listener for Prometheus /metrics.
	MetricsListen   string
	DatabaseURL     string
	SupabaseURL     string
	SupabaseAnonKey string
	SupabaseDialIP  string
	PublicBaseURL   string
	ConsoleOrigin   string
	ServiceSecret   string
	ServiceSecrets  map[string][]string
	XunhuAppID      string
	XunhuAppSecret  string
	XunhuAPIBase    string
	XunhuQueryURL   string
	XunhuNotifyURL  string
	XunhuReturnURL  string
	// USDT paid straight into the operator's own OKX deposit address. Each
	// chain has its own address; the console quotes an exact amount per order
	// and reconciles deposits through the OKX asset API when credentials are
	// present. Without credentials the channel falls back to manual settlement.
	OKXUSDTAddressTRC20   string
	OKXUSDTAddressERC20   string
	OKXUSDTAddressPolygon string
	OKXUSDTCNYRate        float64
	OKXUSDTOrderTTL       time.Duration
	OKXUSDTAllowEmails    []string
	OKXAPIKey             string
	OKXAPISecret          string
	OKXAPIPassphrase      string
	OKXAPIBase            string
	Environment           string
	AdminEmails           []string
	// PaymentOrderTTL expires unpaid orders after this window so an abandoned
	// checkout cannot sit in the order list forever.
	PaymentOrderTTL   time.Duration
	RatePaymentNotify int
	RateOIDCToken     int
	RateProductToken  int
	// Referral program. Reward amounts are copied onto each referral row at
	// claim time, so changing the env only affects future claims.
	ReferralInviterCredits int64
	ReferralInviteeCredits int64
	ReferralMaxUses        int
	RateReferralClaim      int
	OIDCIssuer             string
	OIDCClientID           string
	OIDCClientSecret       string
	OIDCRedirectURIs       string
	OIDCConsentURL         string
	// Public URLs handed to the CLI bootstrap endpoint. They are display/config
	// values, never secrets.
	MarketAPIURL       string
	ConsoleAPIURL      string
	MemoryMCPURL       string
	MemoryTokenPushURL string
}

func Load() Config {
	get := func(k, d string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return d
	}
	secrets := map[string][]string{}
	if raw := get("SERVICE_SECRETS", ""); raw != "" {
		_ = json.Unmarshal([]byte(raw), &secrets)
	}
	if len(secrets) == 0 {
		legacy := get("SERVICE_SECRET", "")
		for _, name := range []string{"market", "dslpha", "causal", "causal-sync", "root"} {
			secrets[name] = []string{legacy}
		}
	}
	return Config{
		Port:                  get("PORT", "8080"),
		MetricsListen:         get("METRICS_LISTEN", "127.0.0.1:9095"),
		DatabaseURL:           get("DATABASE_URL", ""),
		SupabaseURL:           strings.TrimRight(get("SUPABASE_URL", ""), "/"),
		SupabaseAnonKey:       get("SUPABASE_ANON_KEY", ""),
		SupabaseDialIP:        get("SUPABASE_DIAL_IP", ""),
		PublicBaseURL:         strings.TrimRight(get("PUBLIC_BASE_URL", "http://127.0.0.1:8080"), "/"),
		ConsoleOrigin:         get("CONSOLE_ORIGIN", "http://127.0.0.1:5173"),
		ServiceSecret:         get("SERVICE_SECRET", "change-me"),
		ServiceSecrets:        secrets,
		XunhuAppID:            get("XUNHUPAY_APPID", ""),
		XunhuAppSecret:        get("XUNHUPAY_APPSECRET", ""),
		XunhuAPIBase:          get("XUNHUPAY_API_BASE", "https://api.xunhupay.com/payment/do.html"),
		XunhuQueryURL:         get("XUNHUPAY_QUERY_URL", "https://api.xunhupay.com/payment/query.html"),
		XunhuNotifyURL:        get("XUNHUPAY_NOTIFY_URL", ""),
		XunhuReturnURL:        get("XUNHUPAY_RETURN_URL", ""),
		OKXUSDTAddressTRC20:   strings.TrimSpace(get("OKX_USDT_ADDRESS_TRC20", "")),
		OKXUSDTAddressERC20:   strings.TrimSpace(get("OKX_USDT_ADDRESS_ERC20", "")),
		OKXUSDTAddressPolygon: strings.TrimSpace(get("OKX_USDT_ADDRESS_POLYGON", "")),
		OKXUSDTCNYRate:        getFloat("OKX_USDT_CNY_RATE", 0),
		OKXUSDTOrderTTL:       getDuration("OKX_USDT_ORDER_TTL", 30*time.Minute),
		OKXUSDTAllowEmails:    splitCSV(get("OKX_USDT_ALLOW_EMAILS", "")),
		OKXAPIKey:             get("OKX_API_KEY", ""),
		OKXAPISecret:          get("OKX_API_SECRET", ""),
		OKXAPIPassphrase:      get("OKX_API_PASSPHRASE", ""),
		OKXAPIBase:            strings.TrimRight(get("OKX_API_BASE", "https://www.okx.com"), "/"),
		Environment:           get("APP_ENV", "development"),
		PaymentOrderTTL:       getDuration("PAYMENT_ORDER_TTL", 2*time.Hour),
		RatePaymentNotify:     getInt("RATE_PAYMENT_NOTIFY_PER_MIN", 120),
		RateOIDCToken:         getInt("RATE_OIDC_TOKEN_PER_MIN", 60),
		RateProductToken:      getInt("RATE_PRODUCT_TOKEN_PER_MIN", 5),
		AdminEmails:           splitCSV(get("ADMIN_EMAILS", "")),

		ReferralInviterCredits: getInt64("REFERRAL_INVITER_CREDITS", DefaultReferralInviterCredits),
		ReferralInviteeCredits: getInt64("REFERRAL_INVITEE_CREDITS", DefaultReferralInviteeCredits),
		ReferralMaxUses:        getInt("REFERRAL_MAX_USES_PER_CODE", 0),
		RateReferralClaim:      getInt("RATE_REFERRAL_CLAIM_PER_MIN", 10),

		OIDCIssuer:         strings.TrimRight(get("OIDC_ISSUER", "https://www.roostbots.com/console-api"), "/"),
		OIDCClientID:       get("OIDC_CLIENT_ID", "roostbot"),
		OIDCClientSecret:   get("OIDC_CLIENT_SECRET", ""),
		OIDCRedirectURIs:   get("OIDC_REDIRECT_URIS", ""),
		OIDCConsentURL:     get("OIDC_CONSENT_URL", "https://www.roostbots.com/console/oauth/authorize"),
		MarketAPIURL:       strings.TrimRight(get("MARKET_API_URL", "https://www.roostbots.com/api"), "/"),
		ConsoleAPIURL:      strings.TrimRight(get("CONSOLE_API_URL", "https://www.roostbots.com/console-api"), "/"),
		MemoryMCPURL:       strings.TrimRight(get("CAUSAL_MEMORY_MCP_URL", get("MEMORY_MCP_URL", "https://causal-memory.com/memory/mcp")), "/"),
		MemoryTokenPushURL: strings.TrimRight(get("CAUSAL_MEMORY_TOKEN_PUSH_URL", "https://causal-memory.com/memory/__token"), "/"),
	}
}

func getDuration(k string, d time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return d
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return d
	}
	return parsed
}

func getFloat(k string, d float64) float64 {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return d
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || parsed < 0 {
		return d
	}
	return parsed
}

func getInt(k string, d int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return d
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// getInt64 parses a non-negative int64 env value. Invalid or negative values
// fall back to the default, so a typo cannot silently disable rewards.
func getInt64(k string, d int64) int64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return d
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return d
	}
	return n
}

// OKXUSDTConfigured reports whether the direct USDT channel can take orders.
// A positive CNY/USDT rate is required: without it the console cannot quote the
// amount to send, which is the only thing matching relies on.
func (c Config) OKXUSDTConfigured() bool {
	return c.OKXUSDTCNYRate > 0 &&
		(c.OKXUSDTAddressTRC20 != "" || c.OKXUSDTAddressERC20 != "" || c.OKXUSDTAddressPolygon != "")
}

func (c Config) PaymentConfigured() bool {
	return c.XunhuAppID != "" && c.XunhuAppSecret != ""
}

// splitCSV parses a comma separated environment value, trimming blanks.
func splitCSV(raw string) []string {
	out := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, strings.ToLower(part))
		}
	}
	return out
}
