package payment

// USDT paid straight into the operator's own OKX deposit address.
//
// There is no gateway in this path: the console quotes an exact amount for
// every order (base amount plus a small unique suffix), shows the address, and
// reconciles later by asking OKX for the deposits that landed on that address.
// Matching is by (chain, amount), so the payer must send the quoted amount
// exactly — wallets charge their network fee on top, exchanges may deduct it,
// and a deducted amount will not match (the operator can always settle such an
// order by hand from the admin panel).
//
// Reconciliation needs an OKX API key with read access to the asset endpoints.
// Without one the channel still works as "pay, then an admin confirms".

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ProviderOKXUSDT is the billing.orders.provider value for this channel.
const ProviderOKXUSDT = "okx_usdt"

// Supported chains, in the order the console offers them. Cheapest first.
const (
	ChainTRC20   = "TRC20"
	ChainERC20   = "ERC20"
	ChainPolygon = "POLYGON"
)

var chainOrder = []string{ChainTRC20, ChainERC20, ChainPolygon}

// MicroUSDT is one USDT in the integer unit used for exact matching.
const MicroUSDT int64 = 1_000_000

// ErrNoChain is returned when no deposit address is configured for any chain.
var ErrNoChain = errors.New("okx usdt: no deposit address configured")

// USDTQuote is the payment instruction shown to the payer.
type USDTQuote struct {
	Chain       string    `json:"chain"`
	Address     string    `json:"address"`
	Amount      string    `json:"amount"` // display form, 4 decimals
	AmountMicro int64     `json:"-"`      // exact amount to match on
	Rate        float64   `json:"rate"`   // CNY per USDT used for the quote
	ExpiresAt   time.Time `json:"expires_at"`
}

// OKXUSDT holds the channel's configuration.
type OKXUSDT struct {
	// Addresses maps a chain to the deposit address shown to payers.
	Addresses map[string]string
	// Rate is the fallback CNY-per-USDT rate: it seeds the feed and is used
	// whenever a live quote is unavailable.
	Rate float64
	// Rates, when set, supplies a live CNY-per-USDT rate and takes precedence
	// over Rate.
	Rates *RateFeed
	// OrderTTL bounds how long a quote stays valid.
	OrderTTL time.Duration
	// ToleranceMicro absorbs the last-decimal noise some wallets produce.
	ToleranceMicro int64
	// Client is nil when automatic reconciliation is not configured.
	Client *OKXClient
}

// Configured reports whether the channel can take orders at all.
func (c *OKXUSDT) Configured() bool {
	if c == nil || len(c.Chains()) == 0 {
		return false
	}
	_, ok := c.currentRate()
	return ok
}

// currentRate returns the CNY-per-USDT rate to quote with, preferring the live
// feed and falling back to the configured static rate.
func (c *OKXUSDT) currentRate() (float64, bool) {
	if c == nil {
		return 0, false
	}
	if c.Rates != nil {
		if rate, at, ok := c.Rates.Current(); ok && rate > 0 {
			_ = at
			return rate, true
		}
	}
	if c.Rate > 0 {
		return c.Rate, true
	}
	return 0, false
}

// RateInfo reports the rate a quote would use and when it was obtained, for
// display next to the checkout.
func (c *OKXUSDT) RateInfo() (float64, time.Time, bool) {
	if c == nil {
		return 0, time.Time{}, false
	}
	if c.Rates != nil {
		if rate, at, ok := c.Rates.Current(); ok && rate > 0 {
			return rate, at, true
		}
	}
	if c.Rate > 0 {
		return c.Rate, time.Time{}, true
	}
	return 0, time.Time{}, false
}

// AutoConfirm reports whether deposits are reconciled through the OKX API.
func (c *OKXUSDT) AutoConfirm() bool {
	return c != nil && c.Client != nil && c.Client.Configured()
}

// Chains lists the enabled chains in display order.
func (c *OKXUSDT) Chains() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(chainOrder))
	for _, chain := range chainOrder {
		if strings.TrimSpace(c.Addresses[chain]) != "" {
			out = append(out, chain)
		}
	}
	return out
}

// Address returns the deposit address for a chain, or "".
func (c *OKXUSDT) Address(chain string) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Addresses[chain])
}

// Quote computes the exact amount to request for a CNY price.
//
// The base amount is rounded down to whole cents so the payer sees an amount
// that looks like a normal price; the suffix (1–99 hundred-thousandths of a
// USDT) makes it unique among open orders, which is what lets a plain address
// be reconciled without per-user deposit addresses.
func (c *OKXUSDT) Quote(chain string, cnyCents int64, suffix int64) (USDTQuote, error) {
	rate, ok := c.currentRate()
	if !ok {
		return USDTQuote{}, errors.New("okx usdt: rate is not configured")
	}
	address := c.Address(chain)
	if address == "" {
		return USDTQuote{}, fmt.Errorf("okx usdt: chain %q has no address", chain)
	}
	if cnyCents <= 0 {
		return USDTQuote{}, errors.New("okx usdt: amount must be positive")
	}
	// micro = cents/100 CNY / (CNY per USDT) * 1e6
	base := int64(math.Round(float64(cnyCents) * 1e4 / rate))
	base = base / 10_000 * 10_000 // round down to whole cents
	if base < 10_000 {
		base = 10_000 // never quote less than 0.01 USDT
	}
	if suffix < 1 {
		suffix = 1
	}
	amount := base + (((suffix-1)%99)+1)*100
	ttl := c.OrderTTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	return USDTQuote{
		Chain:       chain,
		Address:     address,
		Amount:      FormatMicro(amount),
		AmountMicro: amount,
		Rate:        rate,
		ExpiresAt:   time.Now().Add(ttl).UTC(),
	}, nil
}

// FormatMicro renders micro-USDT with exactly four decimals.
func FormatMicro(micro int64) string {
	sign := ""
	if micro < 0 {
		sign = "-"
		micro = -micro
	}
	whole := micro / MicroUSDT
	frac := micro % MicroUSDT
	// four decimals: drop the last two digits of the six-digit fraction
	frac4 := (frac + 50) / 100
	if frac4 >= 10_000 {
		whole++
		frac4 -= 10_000
	}
	return fmt.Sprintf("%s%d.%04d", sign, whole, frac4)
}

// ParseAmount turns an OKX amount string into micro-USDT.
func ParseAmount(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("okx usdt: empty amount")
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("okx usdt: bad amount %q: %w", raw, err)
	}
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("okx usdt: bad amount %q", raw)
	}
	return int64(math.Round(value * float64(MicroUSDT))), nil
}

// Deposit is one credited USDT deposit from the OKX asset API.
type Deposit struct {
	DepositID   string
	Chain       string
	AmountMicro int64
	At          time.Time
	TxID        string
}

// OpenUSDTOrder is the part of a pending order reconciliation needs.
type OpenUSDTOrder struct {
	OrderID     string
	Chain       string
	AmountMicro int64
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

// USDTMatch pairs a deposit with the order it settles.
type USDTMatch struct {
	OrderID     string
	DepositID   string
	TxID        string
	AmountMicro int64
	Chain       string
	At          time.Time
}

// MatchDeposits pairs deposits with open orders by exact (chain, amount).
//
// An order is matched at most once and a deposit at most once. Deposits that
// landed before the order was created, or after its quote expired plus a grace
// window, are ignored: a recycled amount from an older order must not settle a
// newer one.
func (c *OKXUSDT) MatchDeposits(orders []OpenUSDTOrder, deposits []Deposit) []USDTMatch {
	if len(orders) == 0 || len(deposits) == 0 {
		return nil
	}
	tolerance := int64(0)
	if c != nil && c.ToleranceMicro > 0 {
		tolerance = c.ToleranceMicro
	}
	usedOrder := make(map[string]bool, len(orders))
	usedDeposit := make(map[string]bool, len(deposits))
	out := make([]USDTMatch, 0, len(deposits))

	// Oldest deposits settle oldest orders, so a fresh order cannot claim a
	// deposit that belonged to an expiring one.
	sorted := append([]Deposit(nil), deposits...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	open := append([]OpenUSDTOrder(nil), orders...)
	sort.Slice(open, func(i, j int) bool { return open[i].CreatedAt.Before(open[j].CreatedAt) })

	for _, deposit := range sorted {
		if deposit.Chain == "" || deposit.AmountMicro <= 0 || usedDeposit[deposit.DepositIDOrDefault()] {
			continue
		}
		for _, order := range open {
			if usedOrder[order.OrderID] || order.Chain != deposit.Chain {
				continue
			}
			if abs64(order.AmountMicro-deposit.AmountMicro) > tolerance {
				continue
			}
			if deposit.At.Before(order.CreatedAt.Add(-2 * time.Minute)) {
				continue
			}
			if !order.ExpiresAt.IsZero() && deposit.At.After(order.ExpiresAt.Add(24*time.Hour)) {
				continue
			}
			usedOrder[order.OrderID] = true
			usedDeposit[deposit.DepositIDOrDefault()] = true
			out = append(out, USDTMatch{
				OrderID:     order.OrderID,
				DepositID:   deposit.DepositID,
				TxID:        deposit.TxID,
				AmountMicro: deposit.AmountMicro,
				Chain:       deposit.Chain,
				At:          deposit.At,
			})
			break
		}
	}
	return out
}

// DepositIDOrDefault keeps the used-deposit bookkeeping working when the API
// omits a deposit id but the transaction hash is present.
func (d Deposit) DepositIDOrDefault() string {
	if d.DepositID != "" {
		return d.DepositID
	}
	return d.TxID
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// OKXClient reads deposit history from the OKX v5 asset API.
type OKXClient struct {
	BaseURL    string
	Key        string
	Secret     string
	Passphrase string
	HTTP       *http.Client
	// ChainCodes maps the console's chain names to OKX chain values.
	ChainCodes map[string]string
	// Now is injectable for tests.
	Now func() time.Time
}

func (c *OKXClient) Configured() bool {
	return c != nil && c.Key != "" && c.Secret != "" && c.Passphrase != "" && validHTTPSBase(c.BaseURL)
}

func (c *OKXClient) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *OKXClient) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return okxHTTP
}

// okxHTTP is shared by every API call: short timeout, redirects refused so a
// 3xx cannot downgrade the signed request to another host.
var okxHTTP = &http.Client{
	Timeout: 20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// validHTTPSBase requires an https endpoint (or http on loopback for tests).
func validHTTPSBase(base string) bool {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1"
	}
	return false
}

// OKX timestamp format: RFC3339 with milliseconds.
const okxTimeLayout = "2006-01-02T15:04:05.000Z07:00"

// OKXSign builds the v5 signature: base64(HMAC-SHA256(secret,
// timestamp + method + path + body)).
func OKXSign(secret, timestamp, method, requestPath, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + method + requestPath + body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Deposits returns credited USDT deposits at or after since.
func (c *OKXClient) Deposits(ctx context.Context, since time.Time) ([]Deposit, error) {
	if !c.Configured() {
		return nil, errors.New("okx api is not configured")
	}
	query := url.Values{}
	query.Set("ccy", "USDT")
	query.Set("state", "2") // 2 = credited
	query.Set("limit", "100")
	if !since.IsZero() {
		query.Set("after", strconv.FormatInt(since.UnixMilli(), 10))
	}
	requestPath := "/api/v5/asset/deposit-history?" + query.Encode()
	timestamp := c.now().UTC().Format(okxTimeLayout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+requestPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("OK-ACCESS-KEY", c.Key)
	req.Header.Set("OK-ACCESS-SIGN", OKXSign(c.Secret, timestamp, http.MethodGet, requestPath, ""))
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", c.Passphrase)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("okx: http %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var payload struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			DepID string `json:"depId"`
			Chain string `json:"chain"`
			Amt   string `json:"amt"`
			Ts    string `json:"ts"`
			TxID  string `json:"txId"`
			State string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("okx: bad json: %w", err)
	}
	if payload.Code != "0" {
		return nil, fmt.Errorf("okx: %s %s", payload.Code, truncate(payload.Msg, 200))
	}
	out := make([]Deposit, 0, len(payload.Data))
	for _, row := range payload.Data {
		if row.State != "" && row.State != "2" {
			continue
		}
		micro, err := ParseAmount(row.Amt)
		if err != nil {
			continue
		}
		chain := c.consoleChain(row.Chain)
		if chain == "" {
			continue
		}
		at := time.UnixMilli(0)
		if ms, err := strconv.ParseInt(row.Ts, 10, 64); err == nil {
			at = time.UnixMilli(ms)
		}
		out = append(out, Deposit{
			DepositID:   row.DepID,
			Chain:       chain,
			AmountMicro: micro,
			At:          at,
			TxID:        row.TxID,
		})
	}
	return out, nil
}

// consoleChain maps an OKX chain value ("USDT-TRC20") onto this console's name.
func (c *OKXClient) consoleChain(okxChain string) string {
	upper := strings.ToUpper(strings.TrimSpace(okxChain))
	for chain, code := range c.ChainCodes {
		if upper == strings.ToUpper(code) || strings.HasSuffix(upper, "-"+strings.ToUpper(code)) {
			return chain
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
