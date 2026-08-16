package engine_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/engine"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage/sqlite"
)

// Fills must reach the database.
//
// The paper broker fills synchronously from inside PlaceOrder, so the fill
// callback runs before the engine has saved the order. fills.order_id is a
// foreign key, so inserting the fill first violated the constraint; the engine
// logged the error and carried on, and every paper fill was silently discarded.
// A real database had 34 COMPLETE orders and 0 fill rows — which destroys every
// trade-history and performance report downstream while looking healthy.
//
// This is an external test package on purpose: it needs the real SQLite store
// to exercise the foreign key, and an in-memory fake would pass either way.
func TestFillsPersistWhenBrokerFillsSynchronously(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "fills.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	paper := broker.NewPaperBroker(nil, nil)
	eng := engine.New(paper, store, risk.NewManager(risk.Limits{}), false, nil)

	// A known price makes the market order fill immediately, inside PlaceOrder.
	paper.OnPrice("NIFTY2681824350CE", 146.95)

	if _, err := eng.PlaceManualOrder(ctx, broker.OrderRequest{
		Exchange:      "NFO",
		TradingSymbol: "NIFTY2681824350CE",
		Product:       broker.ProductMIS,
		OrderType:     broker.OrderTypeMarket,
		Side:          broker.SideSell,
		Quantity:      65,
		Validity:      broker.ValidityDay,
	}); err != nil {
		t.Fatalf("PlaceManualOrder: %v", err)
	}

	fills, err := store.Fills(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Fills: %v", err)
	}
	if len(fills) != 1 {
		t.Fatalf("got %d persisted fills, want 1 — the fill was dropped", len(fills))
	}
	if got := fills[0].TradingSymbol; got != "NIFTY2681824350CE" {
		t.Errorf("fill symbol = %q", got)
	}
	if got := fills[0].Quantity; got != 65 {
		t.Errorf("fill quantity = %d, want 65", got)
	}
	if got := fills[0].Side; got != broker.SideSell {
		t.Errorf("fill side = %q, want SELL", got)
	}

	// The parent order must be there too, and consistent.
	orders, err := store.Orders(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].ID != fills[0].OrderID {
		t.Errorf("fill references order %q but the stored order is %q",
			fills[0].OrderID, orders[0].ID)
	}
}

// A round trip through the engine must produce a pairable history: sell then
// buy back is what the short-straddle strategy does all day.
func TestRoundTripProducesTwoFills(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.New(ctx, filepath.Join(t.TempDir(), "rt.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	paper := broker.NewPaperBroker(nil, nil)
	eng := engine.New(paper, store, risk.NewManager(risk.Limits{}), false, nil)
	paper.OnPrice("NIFTY2681824350PE", 74.95)

	sell := broker.OrderRequest{
		Exchange: "NFO", TradingSymbol: "NIFTY2681824350PE",
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideSell, Quantity: 65, Validity: broker.ValidityDay,
	}
	if _, err := eng.PlaceManualOrder(ctx, sell); err != nil {
		t.Fatalf("sell: %v", err)
	}

	paper.OnPrice("NIFTY2681824350PE", 52.35)
	buy := sell
	buy.Side = broker.SideBuy
	if _, err := eng.PlaceManualOrder(ctx, buy); err != nil {
		t.Fatalf("buy: %v", err)
	}

	fills, err := store.Fills(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Fills: %v", err)
	}
	if len(fills) != 2 {
		t.Fatalf("got %d fills for a round trip, want 2", len(fills))
	}
	// Ascending order is what FIFO pairing depends on.
	if fills[0].Timestamp.After(fills[1].Timestamp) {
		t.Error("fills returned newest-first; FIFO pairing would match backwards")
	}
	if fills[0].Side != broker.SideSell || fills[1].Side != broker.SideBuy {
		t.Errorf("fill order = %s then %s, want SELL then BUY", fills[0].Side, fills[1].Side)
	}
}
