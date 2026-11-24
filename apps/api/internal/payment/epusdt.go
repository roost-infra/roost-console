package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
)

// ProviderXunhuPay and ProviderEPUSDT are the billing.orders.provider values.
const (
	ProviderXunhuPay = "xunhupay"
	ProviderEPUSDT   = "epusdt"
)

// EPUSDT order statuses, as returned by create-transaction and check-status.
const (
	EPUSDTStatusWaitPay      = 1
	EPUSDTStatusPaid         = 2
	EPUSDTStatusExpired      = 3
	EPUSDTStatusWaitSelect   = 4
	EPUSDTStatusConfirming   = 5
	EPUSDTStatusManualReview = 6
)

// ErrBadSignature is returned when an EPUSDT callback fails verification.
var ErrBadSignature = errors.New("epusdt: bad signature")

// EPUSDT talks to a self-hosted EPUSDT (GM Pay) gateway over its GMPay v1 API.
// The gateway is non-custodial: it watches the merchant's own addresses and
// sends a signed callback once a transfer is confirmed on chain.
type EPUSDT struct {
	// APIBase is the server-to-server base URL, e.g. http://127.0.0.1:8000.
	APIBase   string
	PID       string
	SecretKey string
	NotifyURL string
	ReturnURL string
	HTTP      *http.Client
}

func (c *EPUSDT) Configured() bool {
	return c != nil && c.APIBase != "" && c.PID != "" && c.SecretKey != ""
}

// EPUSDTSign reproduces the gateway's canonical signature: every non-empty
// parameter except "signature" is rendered as key=value, the rendered pairs are
// sorted, joined with "&" and signed with HMAC-SHA256 (lowercase hex).
//
// Numbers are rendered the way the gateway renders a decoded JSON number
// (shortest decimal, no exponent), so both sides agree on the signed text.
func EPUSDTSign(params map[string]any, secret string) (string, error) {
	pairs := make([]string, 0, len(params))
	for key, value := range params {
		if key == "signature" || value == nil {
			continue
		}
		var text string
		switch v := value.(type) {
		case string:
			text = v
		case float64:
			text = strconv.FormatFloat(v, 'f', -1, 64)
		case int:
			text = strconv.Itoa(v)
		case int64:
			text = strconv.FormatInt(v, 10)
		case json.Number:
			f, err := v.Float64()
			if err != nil {
				return "", fmt.Errorf("epusdt sign: %q is not a number", key)
			}
			text = strconv.FormatFloat(f, 'f', -1, 64)
		default:
			return "", fmt.Errorf("epusdt sign: unsupported value for %q", key)
		}
		if text == "" {
			continue
		}
		pairs = append(pairs, key+"="+text)
	}
	// The gateway sorts the rendered pairs, not the keys.
	sort.Strings(pairs)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strings.Join(pairs, "&")))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

type epusdtEnvelope struct {
	StatusCode int             `json:"status_code"`
	Message    string          `json:"message"`
	Data       json.RawMessage `json:"data"`
}

// Create opens a checkout for one order. Token and network are left out on
// purpose: the gateway then creates a placeholder and the payer picks the chain
// and asset on the hosted cashier page.
func (c *EPUSDT) Create(ctx context.Context, in CreateInput) (CreateResult, error) {
	if !c.Configured() {
		return CreateResult{}, ErrNotConfigured
	}
	if in.AmountCents <= 0 {
		return CreateResult{}, errors.New("epusdt: amount must be positive")
	}
	params := map[string]any{
		"pid":        c.PID,
		"order_id":   in.OutTradeNo,
		"currency":   "cny",
		"amount":     float64(in.AmountCents) / 100,
		"notify_url": c.NotifyURL,
		"name":       in.Title,
		// Version 2 adds the settled network to the signed callback.
		"callback_version": 2,
	}
	if c.ReturnURL != "" {
		params["redirect_url"] = c.ReturnURL
	}
	signature, err := EPUSDTSign(params, c.SecretKey)
	if err != nil {
		return CreateResult{}, err
	}
	params["signature"] = signature
	body, err := json.Marshal(params)
	if err != nil {
		return CreateResult{}, err
	}

	var data struct {
		TradeID    string  `json:"trade_id"`
		OrderID    string  `json:"order_id"`
		Amount     float64 `json:"amount"`
		PaymentURL string  `json:"payment_url"`
	}
	if err := c.call(ctx, http.MethodPost, "/payments/gmpay/v1/order/create-transaction", body, &data); err != nil {
		return CreateResult{}, err
	}
	if data.TradeID == "" || data.OrderID != in.OutTradeNo {
		return CreateResult{}, errors.New("epusdt: create response does not match the order")
	}
	// The callback reports this amount; a gateway that rounded it would make
	// every fulfillment fail, so refuse the checkout up front.
	if got := centsFromYuan(data.Amount); got != in.AmountCents {
		return CreateResult{}, fmt.Errorf("epusdt: gateway priced the order at %d cents, want %d", got, in.AmountCents)
	}
	payURL, err := url.Parse(data.PaymentURL)
	if err != nil || (payURL.Scheme != "https" && payURL.Scheme != "http") || payURL.Host == "" {
		return CreateResult{}, errors.New("epusdt: create response has no usable payment_url")
	}
	return CreateResult{ProviderOrderID: data.TradeID, PayURL: data.PaymentURL}, nil
}

// Status asks the gateway for an order's current state by its trade id. It is
// the reconciliation path for a lost callback. The gateway has no signed query
// API; like the XunhuPay query this relies on the configured gateway endpoint,
// and fulfillment still applies only the order's own stored amount.
func (c *EPUSDT) Status(ctx context.Context, tradeID string) (int, error) {
	if !c.Configured() {
		return 0, ErrNotConfigured
	}
	if strings.TrimSpace(tradeID) == "" {
		return 0, errors.New("epusdt: trade id is required")
	}
	var data struct {
		TradeID string `json:"trade_id"`
		Status  int    `json:"status"`
	}
	if err := c.call(ctx, http.MethodGet, "/pay/check-status/"+url.PathEscape(tradeID), nil, &data); err != nil {
		return 0, err
	}
	if data.TradeID != tradeID {
		return 0, errors.New("epusdt: status response does not match the order")
	}
	return data.Status, nil
}

func (c *EPUSDT) call(ctx context.Context, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.APIBase, "/")+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var envelope epusdtEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("epusdt response (http %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || envelope.StatusCode != http.StatusOK {
		return fmt.Errorf("epusdt: %d %s", envelope.StatusCode, envelope.Message)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("epusdt response data: %w", err)
	}
	return nil
}

// EPUSDTNotification is one verified payment callback.
type EPUSDTNotification struct {
	TradeID            string
	OrderID            string
	AmountCents        int
	ActualAmount       float64
	Token              string
	Network            string
	ReceiveAddress     string
	BlockTransactionID string
	Status             int
}

// ParseNotification verifies a callback body against the merchant secret and
// returns its fields. Nothing in the body is trusted before the signature and
// the merchant PID check out.
func (c *EPUSDT) ParseNotification(body []byte) (EPUSDTNotification, error) {
	if !c.Configured() {
		return EPUSDTNotification{}, ErrNotConfigured
	}
	var params map[string]any
	if err := json.Unmarshal(body, &params); err != nil {
		return EPUSDTNotification{}, fmt.Errorf("epusdt notify body: %w", err)
	}
	got, _ := params["signature"].(string)
	want, err := EPUSDTSign(params, c.SecretKey)
	if err != nil || got == "" || !hmac.Equal([]byte(strings.ToLower(strings.TrimSpace(got))), []byte(want)) {
		return EPUSDTNotification{}, ErrBadSignature
	}
	if pid, _ := params["pid"].(string); pid != c.PID {
		return EPUSDTNotification{}, ErrBadSignature
	}
	text := func(key string) string {
		value, _ := params[key].(string)
		return strings.TrimSpace(value)
	}
	number := func(key string) float64 {
		value, _ := params[key].(float64)
		return value
	}
	return EPUSDTNotification{
		TradeID:            text("trade_id"),
		OrderID:            text("order_id"),
		AmountCents:        centsFromYuan(number("amount")),
		ActualAmount:       number("actual_amount"),
		Token:              text("token"),
		Network:            text("network"),
		ReceiveAddress:     text("receive_address"),
		BlockTransactionID: text("block_transaction_id"),
		Status:             int(number("status")),
	}, nil
}

func centsFromYuan(yuan float64) int {
	return int(math.Round(yuan * 100))
}
