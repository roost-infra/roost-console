package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/payment"
)

type reconcileFake struct {
	orders    []billing.Order
	touched   []string
	closed    []string
	fulfilled map[string]struct {
		tradeNo string
		amount  int
		raw     string
	}
}

func (f *reconcileFake) ListReconcileOrders(context.Context, time.Duration, int) ([]billing.Order, error) {
	return f.orders, nil
}

func (f *reconcileFake) TouchOrderReconcile(_ context.Context, orderID string) error {
	f.touched = append(f.touched, orderID)
	return nil
}

func (f *reconcileFake) Fulfill(_ context.Context, orderID, tradeNo string, amountCents int, raw []byte) error {
	if f.fulfilled == nil {
		f.fulfilled = map[string]struct {
			tradeNo string
			amount  int
			raw     string
		}{}
	}
	f.fulfilled[orderID] = struct {
		tradeNo string
		amount  int
		raw     string
	}{tradeNo, amountCents, string(raw)}
	return nil
}

func (f *reconcileFake) CloseOrder(_ context.Context, orderID, status string) error {
	f.closed = append(f.closed, orderID+":"+status)
	return nil
}

type fakeXunhu struct {
	asked  []string
	status string
}

func (f *fakeXunhu) Query(_ context.Context, outTradeNo string) (payment.QueryResult, error) {
	f.asked = append(f.asked, outTradeNo)
	return payment.QueryResult{Status: f.status, Raw: map[string]any{"data": map[string]any{"status": f.status, "transaction_id": "wx-1"}}}, nil
}

type fakeEPUSDT struct {
	asked    []string
	statuses map[string]int
	err      error
}

func (f *fakeEPUSDT) Status(_ context.Context, tradeID string) (int, error) {
	f.asked = append(f.asked, tradeID)
	if f.err != nil {
		return 0, f.err
	}
	return f.statuses[tradeID], nil
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func cryptoOrder(id, gatewayID string) billing.Order {
	return billing.Order{ID: id, OutTradeNo: "OUT-" + id, AmountCents: 5000, Status: "pending",
		Provider: payment.ProviderEPUSDT, ProviderOrderID: gatewayID}
}

func TestReconcileAsksEachOrdersOwnProvider(t *testing.T) {
	store := &reconcileFake{orders: []billing.Order{
		{ID: "wx", OutTradeNo: "OUT-wx", AmountCents: 4900, Status: "pending", Provider: payment.ProviderXunhuPay},
		cryptoOrder("paid", "GW-paid"),
		cryptoOrder("expired", "GW-expired"),
		cryptoOrder("waiting", "GW-waiting"),
		cryptoOrder("confirming", "GW-confirming"),
		cryptoOrder("review", "GW-review"),
	}}
	xunhu := &fakeXunhu{status: "OD"}
	gateway := &fakeEPUSDT{statuses: map[string]int{
		"GW-paid": payment.EPUSDTStatusPaid, "GW-expired": payment.EPUSDTStatusExpired,
		"GW-waiting": payment.EPUSDTStatusWaitSelect, "GW-confirming": payment.EPUSDTStatusConfirming,
		"GW-review": payment.EPUSDTStatusManualReview,
	}}
	reconcileOrders(context.Background(), store, orderGateways{xunhu: xunhu, epusdt: gateway}, quietLog())

	if strings.Join(xunhu.asked, ",") != "OUT-wx" {
		t.Fatalf("xunhupay was asked about %v", xunhu.asked)
	}
	if strings.Join(gateway.asked, ",") != "GW-paid,GW-expired,GW-waiting,GW-confirming,GW-review" {
		t.Fatalf("epusdt was asked about %v", gateway.asked)
	}
	if len(store.fulfilled) != 2 {
		t.Fatalf("fulfilled = %v", store.fulfilled)
	}
	if got := store.fulfilled["wx"]; got.tradeNo != "wx-1" || got.amount != 4900 {
		t.Fatalf("xunhupay fulfill = %+v", got)
	}
	// The stored order amount is applied, never a figure from the gateway.
	if got := store.fulfilled["paid"]; got.tradeNo != "epusdt_GW-paid" || got.amount != 5000 || !strings.Contains(got.raw, `"reconciled":true`) {
		t.Fatalf("epusdt fulfill = %+v", got)
	}
	// An in-flight or under-review payment is left for the callback or an operator.
	if strings.Join(store.closed, ",") != "expired:expired" {
		t.Fatalf("closed = %v", store.closed)
	}
	if len(store.touched) != 6 {
		t.Fatalf("touched = %v", store.touched)
	}
}

func TestReconcileLeavesUnconfiguredProvidersAlone(t *testing.T) {
	store := &reconcileFake{orders: []billing.Order{
		cryptoOrder("crypto", "GW-1"),
		{ID: "wx", OutTradeNo: "OUT-wx", AmountCents: 4900, Status: "pending", Provider: payment.ProviderXunhuPay},
		// Checkout never reached the gateway, so there is no id to ask about.
		cryptoOrder("no-checkout", ""),
		{ID: "odd", OutTradeNo: "OUT-odd", AmountCents: 100, Status: "pending", Provider: "something-else"},
	}}

	// Only XunhuPay is configured: a paid answer there must not fulfill the crypto order.
	xunhu := &fakeXunhu{status: "OD"}
	reconcileOrders(context.Background(), store, orderGateways{xunhu: xunhu}, quietLog())
	if strings.Join(xunhu.asked, ",") != "OUT-wx" || len(store.fulfilled) != 1 || store.fulfilled["wx"].tradeNo == "" {
		t.Fatalf("asked=%v fulfilled=%v", xunhu.asked, store.fulfilled)
	}

	// Only the crypto gateway is configured.
	store = &reconcileFake{orders: store.orders}
	gateway := &fakeEPUSDT{statuses: map[string]int{"GW-1": payment.EPUSDTStatusPaid}}
	reconcileOrders(context.Background(), store, orderGateways{epusdt: gateway}, quietLog())
	if strings.Join(gateway.asked, ",") != "GW-1" || len(store.fulfilled) != 1 || store.fulfilled["crypto"].tradeNo != "epusdt_GW-1" {
		t.Fatalf("asked=%v fulfilled=%v", gateway.asked, store.fulfilled)
	}
	// Orders nobody can be asked about still back off, so they cannot fill
	// every batch and starve the ones that can be reconciled.
	if strings.Join(store.touched, ",") != "crypto,wx,no-checkout,odd" {
		t.Fatalf("touched = %v", store.touched)
	}
}

func TestReconcileStillCatchesALateSettlementOfAnExpiredOrder(t *testing.T) {
	// The gateway's checkout expired before the transfer confirmed, so the row
	// reads 'expired' locally; the money still arrives. Asking about it must
	// credit the order, and a gateway that agrees it expired must not touch it.
	expired := cryptoOrder("late", "GW-late")
	expired.Status = "expired"
	gone := cryptoOrder("gone", "GW-gone")
	gone.Status = "expired"
	store := &reconcileFake{orders: []billing.Order{expired, gone}}
	gateway := &fakeEPUSDT{statuses: map[string]int{
		"GW-late": payment.EPUSDTStatusPaid, "GW-gone": payment.EPUSDTStatusExpired,
	}}
	reconcileOrders(context.Background(), store, orderGateways{epusdt: gateway}, quietLog())
	if got, ok := store.fulfilled["late"]; !ok || got.amount != 5000 {
		t.Fatalf("late settlement not credited: %v", store.fulfilled)
	}
	if len(store.fulfilled) != 1 {
		t.Fatalf("fulfilled = %v", store.fulfilled)
	}
	if len(store.closed) != 0 {
		t.Fatalf("an already-expired order was closed again: %v", store.closed)
	}
}

func TestReconcileBacksOffWhenTheGatewayFails(t *testing.T) {
	store := &reconcileFake{orders: []billing.Order{cryptoOrder("crypto", "GW-1")}}
	gateway := &fakeEPUSDT{err: errors.New("connection refused")}
	reconcileOrders(context.Background(), store, orderGateways{epusdt: gateway}, quietLog())
	if len(store.fulfilled) != 0 || len(store.closed) != 0 {
		t.Fatalf("a failed query changed the order: %v %v", store.fulfilled, store.closed)
	}
	if strings.Join(store.touched, ",") != "crypto" {
		t.Fatalf("touched = %v", store.touched)
	}
}
