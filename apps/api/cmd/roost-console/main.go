package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/auth"
	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/config"
	"github.com/JingxuanC/roost-console/apps/api/internal/db"
	"github.com/JingxuanC/roost-console/apps/api/internal/httpapi"
	"github.com/JingxuanC/roost-console/apps/api/internal/metrics"
	"github.com/JingxuanC/roost-console/apps/api/internal/payment"
)

func supabaseHTTPClient(cfg config.Config) *http.Client {
	transport := &http.Transport{}
	if cfg.SupabaseDialIP != "" {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(cfg.SupabaseDialIP, port))
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: transport}
}

// startPaymentJobs closes abandoned checkouts and reconciles pending orders
// against the provider. A webhook alone is not a delivery guarantee: if the
// notify is lost or rejected the order must still be fulfilled, and an
// abandoned checkout must not read as "in progress" forever.
func startPaymentJobs(ctx context.Context, cfg config.Config, store *billing.Store, pay *payment.Client, usdt *payment.OKXUSDT, log *slog.Logger) {
	if store == nil {
		return
	}
	go func() {
		tick := func() {
			runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			// Reconcile before expiring: a paid order whose notify was lost must
			// be fulfilled while it can still be queried, not closed as stale.
			if cfg.PaymentConfigured() {
				reconcileOrders(runCtx, store, pay, log)
			}
			// Direct USDT has no callback at all: deposits are found by asking
			// OKX what landed on our address, then matched to open quotes.
			if usdt != nil && usdt.Configured() {
				reconcileUSDTDeposits(runCtx, store, usdt, log)
				if n, err := store.ExpireStaleUSDTOrders(runCtx, payment.ProviderOKXUSDT, cfg.OKXUSDTOrderTTL); err != nil {
					log.Warn("usdt order expiry failed", "err", err)
				} else if n > 0 {
					log.Info("expired stale usdt orders", "count", n)
				}
			}
			if n, err := store.ExpireStaleOrders(runCtx, cfg.PaymentOrderTTL); err != nil {
				log.Warn("order expiry failed", "err", err)
			} else if n > 0 {
				log.Info("expired stale orders", "count", n)
			}
		}
		tick()
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			tick()
		}
	}()
}

// reconcileOrders queries the provider for pending orders whose notify should
// have arrived by now. Paid orders are fulfilled; cancelled checkouts are
// closed. Failures are logged and retried on the next tick.
func reconcileOrders(ctx context.Context, store *billing.Store, pay *payment.Client, log *slog.Logger) {
	orders, err := store.ListReconcileOrders(ctx, 2*time.Minute, 20)
	if err != nil {
		log.Warn("order reconcile list failed", "err", err)
		return
	}
	for _, order := range orders {
		result, err := pay.Query(ctx, order.OutTradeNo)
		// Back off this order for 10 minutes even on error: a broken secret or
		// provider outage must not turn into a per-minute query storm.
		if touchErr := store.TouchOrderReconcile(ctx, order.ID); touchErr != nil {
			log.Warn("order reconcile touch failed", "order", order.ID, "err", touchErr)
		}
		if err != nil {
			log.Warn("order reconcile query failed", "order", order.ID, "err", err)
			continue
		}
		switch result.Status {
		case "OD":
			raw, _ := json.Marshal(result.Raw)
			tradeNo := reconciledTradeNo(result.Raw, order.ID)
			if err := store.Fulfill(ctx, order.ID, tradeNo, order.AmountCents, raw); err != nil {
				log.Error("order reconcile fulfill failed", "order", order.ID, "err", err)
				continue
			}
			log.Info("order reconciled as paid", "order", order.ID, "out_trade_no", order.OutTradeNo)
		case "CD":
			if err := store.CloseOrder(ctx, order.ID, "expired"); err != nil {
				log.Warn("order reconcile close failed", "order", order.ID, "err", err)
				continue
			}
			log.Info("order reconciled as cancelled", "order", order.ID, "out_trade_no", order.OutTradeNo)
		}
	}
}

// reconciledTradeNo prefers the provider's own identifiers so the row matches
// any later notify; the fallback is unique per order.
func reconciledTradeNo(raw map[string]any, orderID string) string {
	if data, ok := raw["data"].(map[string]any); ok {
		for _, key := range []string{"transaction_id", "trade_no", "open_order_id"} {
			if value, ok := data[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return "query_" + orderID
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		log.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	if cfg.SupabaseURL == "" || cfg.SupabaseAnonKey == "" {
		log.Error("SUPABASE_URL and SUPABASE_ANON_KEY are required")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database connection failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	store := billing.New(pool)
	verifier := auth.NewVerifier(cfg.SupabaseURL, cfg.SupabaseAnonKey, supabaseHTTPClient(cfg))
	pay := &payment.Client{
		AppID: cfg.XunhuAppID, AppSecret: cfg.XunhuAppSecret, APIBase: cfg.XunhuAPIBase,
		QueryURL:  cfg.XunhuQueryURL,
		NotifyURL: cfg.XunhuNotifyURL, ReturnURL: cfg.XunhuReturnURL, HTTP: &http.Client{Timeout: 20 * time.Second},
	}
	usdt := okxUSDTChannel(cfg)
	srv := httpapi.New(cfg, store, verifier, pay, log)
	startPaymentJobs(context.Background(), cfg, store, pay, usdt, log)
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			n, err := store.ExpireLeases(context.Background())
			if err != nil {
				log.Warn("lease expiry failed", "err", err)
			} else if n > 0 {
				log.Info("expired stale leases", "count", n)
			}
		}
	}()
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	var metricsServer *http.Server
	if cfg.MetricsListen != "" {
		metricsServer = &http.Server{
			Addr:              cfg.MetricsListen,
			Handler:           metrics.Default.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			log.Info("metrics listening", "addr", cfg.MetricsListen)
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics server failed", "err", err)
			}
		}()
	}
	go func() {
		log.Info("roost console api listening", "addr", httpServer.Addr, "env", cfg.Environment)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = httpServer.Shutdown(shutdown)
	if metricsServer != nil {
		_ = metricsServer.Shutdown(shutdown)
	}
}

// okxUSDTChannel builds the direct-USDT channel from the environment.
func okxUSDTChannel(cfg config.Config) *payment.OKXUSDT {
	if !cfg.OKXUSDTConfigured() {
		return nil
	}
	return &payment.OKXUSDT{
		Addresses: map[string]string{
			payment.ChainTRC20:   cfg.OKXUSDTAddressTRC20,
			payment.ChainERC20:   cfg.OKXUSDTAddressERC20,
			payment.ChainPolygon: cfg.OKXUSDTAddressPolygon,
		},
		Rate:     cfg.OKXUSDTCNYRate,
		OrderTTL: cfg.OKXUSDTOrderTTL,
		Client: &payment.OKXClient{
			BaseURL:    cfg.OKXAPIBase,
			Key:        cfg.OKXAPIKey,
			Secret:     cfg.OKXAPISecret,
			Passphrase: cfg.OKXAPIPassphrase,
			ChainCodes: map[string]string{
				payment.ChainTRC20:   "TRC20",
				payment.ChainERC20:   "ERC20",
				payment.ChainPolygon: "Polygon",
			},
		},
	}
}

// reconcileUSDTDeposits fulfils USDT orders whose deposit landed on our
// address. It asks OKX once per tick for everything after the oldest open
// order and matches in memory, so N open orders cost one API call.
func reconcileUSDTDeposits(ctx context.Context, store *billing.Store, usdt *payment.OKXUSDT, log *slog.Logger) {
	if store == nil || usdt == nil || !usdt.AutoConfirm() {
		return
	}
	orders, err := store.PendingUSDTOrders(ctx, payment.ProviderOKXUSDT, 100)
	if err != nil {
		log.Warn("usdt reconcile list failed", "err", err)
		return
	}
	if len(orders) == 0 {
		return
	}
	open := make([]payment.OpenUSDTOrder, 0, len(orders))
	since := time.Now().Add(-24 * time.Hour)
	for _, o := range orders {
		if o.USDT == nil {
			continue
		}
		if o.CreatedAt.Before(since) {
			since = o.CreatedAt
		}
		open = append(open, payment.OpenUSDTOrder{
			OrderID:     o.ID,
			Chain:       o.USDT.Chain,
			AmountMicro: o.USDT.AmountMicro,
			CreatedAt:   o.CreatedAt,
			ExpiresAt:   o.USDT.ExpiresAt,
		})
	}
	if len(open) == 0 {
		return
	}
	deposits, err := usdt.Client.Deposits(ctx, since.Add(-time.Hour))
	if err != nil {
		log.Warn("usdt deposit fetch failed", "err", err)
		return
	}
	for _, match := range usdt.MatchDeposits(open, deposits) {
		order, ok := findByID(orders, match.OrderID)
		if !ok {
			continue
		}
		raw, _ := json.Marshal(map[string]any{
			"usdt_deposit_id":   match.DepositID,
			"usdt_tx_id":        match.TxID,
			"usdt_chain":        match.Chain,
			"usdt_amount_micro": match.AmountMicro,
			"usdt_at":           match.At.UTC().Format(time.RFC3339),
		})
		tradeNo := "okx_" + match.DepositID
		if match.DepositID == "" {
			tradeNo = "okx_" + match.TxID
		}
		if err := store.Fulfill(ctx, order.ID, tradeNo, order.AmountCents, raw); err != nil {
			log.Error("usdt fulfill failed", "order", order.ID, "err", err)
			continue
		}
		log.Info("usdt order fulfilled", "order", order.ID, "deposit", match.DepositID,
			"chain", match.Chain, "amount", payment.FormatMicro(match.AmountMicro))
	}
}

func findByID(orders []billing.Order, id string) (billing.Order, bool) {
	for _, o := range orders {
		if o.ID == id {
			return o, true
		}
	}
	return billing.Order{}, false
}
