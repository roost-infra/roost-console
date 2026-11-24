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
func startPaymentJobs(ctx context.Context, cfg config.Config, store *billing.Store, pay *payment.Client, epusdt *payment.EPUSDT, log *slog.Logger) {
	if store == nil {
		return
	}
	go func() {
		tick := func() {
			runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			// Reconcile before expiring: a paid order whose notify was lost must
			// be fulfilled while it can still be queried, not closed as stale.
			if cfg.PaymentConfigured() || cfg.EPUSDTConfigured() {
				var gateways orderGateways
				if cfg.PaymentConfigured() {
					gateways.xunhu = pay
				}
				if cfg.EPUSDTConfigured() {
					gateways.epusdt = epusdt
				}
				reconcileOrders(runCtx, store, gateways, log)
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

// reconcileStore is the slice of the billing store reconciliation needs.
type reconcileStore interface {
	ListReconcileOrders(ctx context.Context, minAge time.Duration, limit int) ([]billing.Order, error)
	TouchOrderReconcile(ctx context.Context, orderID string) error
	Fulfill(ctx context.Context, orderID, providerTradeNo string, amountCents int, raw []byte) error
	CloseOrder(ctx context.Context, orderID, status string) error
}

type xunhuQuerier interface {
	Query(ctx context.Context, outTradeNo string) (payment.QueryResult, error)
}

type epusdtQuerier interface {
	Status(ctx context.Context, tradeID string) (int, error)
}

// orderGateways holds the configured providers; a nil entry is not configured
// and its orders are left alone.
type orderGateways struct {
	xunhu  xunhuQuerier
	epusdt epusdtQuerier
}

// orderState is a provider's answer reduced to what reconciliation acts on.
type orderState struct {
	paid, closed bool
	tradeNo      string
	raw          []byte
}

// reconcileOrders queries each pending order's own provider once its notify
// should have arrived. Paid orders are fulfilled; cancelled or expired
// checkouts are closed. Failures are logged and retried on a later tick.
func reconcileOrders(ctx context.Context, store reconcileStore, gateways orderGateways, log *slog.Logger) {
	orders, err := store.ListReconcileOrders(ctx, 2*time.Minute, 20)
	if err != nil {
		log.Warn("order reconcile list failed", "err", err)
		return
	}
	for _, order := range orders {
		query := gateways.queryFor(order)
		var state orderState
		var err error
		if query != nil {
			state, err = query(ctx)
		}
		// Back off this order for 10 minutes even on error: a broken secret or
		// provider outage must not turn into a per-minute query storm. An order
		// nobody can be asked about backs off too, so it cannot fill the batch.
		if touchErr := store.TouchOrderReconcile(ctx, order.ID); touchErr != nil {
			log.Warn("order reconcile touch failed", "order", order.ID, "err", touchErr)
		}
		if query == nil {
			// Its provider is not configured here. Never ask a different one.
			continue
		}
		if err != nil {
			log.Warn("order reconcile query failed", "order", order.ID, "provider", order.Provider, "err", err)
			continue
		}
		switch {
		case state.paid:
			if err := store.Fulfill(ctx, order.ID, state.tradeNo, order.AmountCents, state.raw); err != nil {
				log.Error("order reconcile fulfill failed", "order", order.ID, "err", err)
				continue
			}
			log.Info("order reconciled as paid", "order", order.ID, "provider", order.Provider, "out_trade_no", order.OutTradeNo)
		case state.closed:
			if err := store.CloseOrder(ctx, order.ID, "expired"); err != nil {
				log.Warn("order reconcile close failed", "order", order.ID, "err", err)
				continue
			}
			log.Info("order reconciled as cancelled", "order", order.ID, "provider", order.Provider, "out_trade_no", order.OutTradeNo)
		}
	}
}

// queryFor returns the provider lookup for one order, or nil when that order's
// provider is not configured.
func (g orderGateways) queryFor(order billing.Order) func(context.Context) (orderState, error) {
	switch order.Provider {
	case payment.ProviderXunhuPay:
		if g.xunhu == nil {
			return nil
		}
		return func(ctx context.Context) (orderState, error) {
			result, err := g.xunhu.Query(ctx, order.OutTradeNo)
			if err != nil {
				return orderState{}, err
			}
			raw, _ := json.Marshal(result.Raw)
			return orderState{
				paid:    result.Status == "OD",
				closed:  result.Status == "CD",
				tradeNo: reconciledTradeNo(result.Raw, order.ID),
				raw:     raw,
			}, nil
		}
	case payment.ProviderEPUSDT:
		// An order without a gateway trade id never reached the cashier, so
		// there is nothing to ask about; the TTL closes it.
		if g.epusdt == nil || order.ProviderOrderID == "" {
			return nil
		}
		return func(ctx context.Context) (orderState, error) {
			status, err := g.epusdt.Status(ctx, order.ProviderOrderID)
			if err != nil {
				return orderState{}, err
			}
			raw, _ := json.Marshal(map[string]any{
				"reconciled": true, "provider": payment.ProviderEPUSDT,
				"trade_id": order.ProviderOrderID, "status": status,
			})
			return orderState{
				paid:   status == payment.EPUSDTStatusPaid,
				closed: status == payment.EPUSDTStatusExpired,
				// A later callback carries the chain hash instead; Fulfill is
				// idempotent, so whichever arrives first wins.
				tradeNo: "epusdt_" + order.ProviderOrderID,
				raw:     raw,
			}, nil
		}
	}
	return nil
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
	epusdt := &payment.EPUSDT{
		APIBase: cfg.EPUSDTAPIBase, PID: cfg.EPUSDTPID, SecretKey: cfg.EPUSDTSecretKey,
		NotifyURL: cfg.EPUSDTNotifyURL, ReturnURL: cfg.EPUSDTReturnURL, HTTP: &http.Client{Timeout: 20 * time.Second},
	}
	srv := httpapi.New(cfg, store, verifier, pay, epusdt, log)
	startPaymentJobs(context.Background(), cfg, store, pay, epusdt, log)
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
