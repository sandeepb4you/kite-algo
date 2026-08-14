package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage/sqlite"
)

// newTestApp builds the platform exactly as main() does, so the wiring under
// test is the wiring that actually ships.
func newTestApp(t *testing.T) *App {
	t.Helper()
	ctx := context.Background()

	cfg := &config.Config{
		Mode:    config.ModePaper,
		Kite:    config.KiteConfig{APIKey: "k", APISecret: "s"},
		Storage: config.StorageConfig{SQLitePath: filepath.Join(t.TempDir(), "wiring.db")},
		Web:     config.WebConfig{Addr: "127.0.0.1:0", SessionTTL: time.Hour},
		Risk: config.RiskConfig{
			MaxDailyLoss: 100000, MaxOrderValue: 1000000,
			MaxLotsPerTrade: 100, MaxOpenPositions: 20,
		},
	}

	store, err := sqlite.New(ctx, cfg.Storage.SQLitePath, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	a, err := New(ctx, cfg, store, nil)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	// Run the platform for real: Start wires the background loops that refresh
	// the position cache, so a test that skipped it would not see fills land.
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() { _ = a.Run(runCtx) }()

	return a
}

// waitForPosition polls the position cache, which the engine's sync loop
// refreshes every few seconds.
func waitForPosition(t *testing.T, a *App, symbol string, qty int) bool {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range a.Engine.Positions() {
			if p.TradingSymbol == symbol && p.NetQuantity == qty {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestPaperOrdersActuallyFill is the end-to-end wiring check that was missing.
//
// Every engine-level test constructed the engine by hand and passed
// WithPaperBroker explicitly, so they all passed while app.New — the only
// construction that ships — omitted it. The result: e.paperBroker was nil,
// handleTick never fed the simulated broker a price, and NO paper order could
// ever fill. Orders sat PENDING for ever with nothing logged.
//
// This test drives the real app wiring, so the mistake cannot recur unnoticed.
func TestPaperOrdersActuallyFill(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	const symbol = "NIFTY25AUG24500CE"

	// A tick, exactly as the Kite ticker would deliver it.
	a.Engine.HandleTickForTest(marketdata.Tick{
		TradingSymbol: symbol,
		Exchange:      "NFO",
		LastPrice:     120.5,
		Timestamp:     time.Now(),
	})

	order, err := a.Engine.PlaceManualOrder(ctx, broker.OrderRequest{
		Exchange:      "NFO",
		TradingSymbol: symbol,
		Product:       broker.ProductMIS,
		OrderType:     broker.OrderTypeMarket,
		Side:          broker.SideBuy,
		Quantity:      75,
	})
	if err != nil {
		t.Fatalf("PlaceManualOrder: %v", err)
	}

	if order.Status != broker.StatusComplete {
		t.Fatalf("market order status = %s, want COMPLETE.\n"+
			"A market order placed against a known price must fill immediately; "+
			"PENDING means the paper broker is not receiving prices.", order.Status)
	}
	if order.FilledQuantity != 75 {
		t.Errorf("filled quantity = %d, want 75", order.FilledQuantity)
	}
	if order.Price != 0 && order.Price != 120.5 {
		t.Errorf("fill price = %v, want the market price 120.5", order.Price)
	}

	// And the fill must reach the position book, not just the order.
	a.Engine.HandleTickForTest(marketdata.Tick{
		TradingSymbol: symbol, LastPrice: 121, Timestamp: time.Now(),
	})
	if !waitForPosition(t, a, symbol, 75) {
		t.Error("the fill did not produce a position; the fill callback is not wired")
	}
}

// TestMarketOrderFillsOnTheNextTick covers ordering the other way round: an
// order placed before any tick for that symbol must fill as soon as one arrives,
// rather than waiting for a second order or sitting pending indefinitely.
func TestMarketOrderFillsOnTheNextTick(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	const symbol = "NIFTY25AUG24600PE"

	order, err := a.Engine.PlaceManualOrder(ctx, broker.OrderRequest{
		Exchange: "NFO", TradingSymbol: symbol, Product: broker.ProductMIS,
		OrderType: broker.OrderTypeMarket, Side: broker.SideSell, Quantity: 75,
	})
	if err != nil {
		t.Fatalf("PlaceManualOrder: %v", err)
	}
	if order.Status != broker.StatusPending {
		t.Logf("order status before any tick: %s", order.Status)
	}

	// First tick for the symbol.
	a.Engine.HandleTickForTest(marketdata.Tick{
		TradingSymbol: symbol, LastPrice: 98.25, Timestamp: time.Now(),
	})

	open, err := a.Engine.OpenOrders(ctx)
	if err != nil {
		t.Fatalf("OpenOrders: %v", err)
	}
	for _, o := range open {
		if o.ID == order.ID {
			t.Error("the order was still pending after a tick arrived for its symbol")
		}
	}

	if !waitForPosition(t, a, symbol, -75) {
		t.Error("no short position after the sell filled")
	}
}

// TestFillUpdatesPositionsPromptly covers the latency an operator actually
// feels. The position book used to refresh only on a 3-second timer, so a fill
// took up to three seconds to appear — precisely when it is being watched.
// A fill now nudges the sync loop, so the book catches up in well under a second.
func TestFillUpdatesPositionsPromptly(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	const symbol = "NIFTY25AUG24700CE"

	a.Engine.HandleTickForTest(marketdata.Tick{
		TradingSymbol: symbol, LastPrice: 55.5, Timestamp: time.Now(),
	})

	// Let the initial sync settle so the timer is not about to fire anyway.
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	if _, err := a.Engine.PlaceManualOrder(ctx, broker.OrderRequest{
		Exchange: "NFO", TradingSymbol: symbol, Product: broker.ProductMIS,
		OrderType: broker.OrderTypeMarket, Side: broker.SideBuy, Quantity: 75,
	}); err != nil {
		t.Fatalf("PlaceManualOrder: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var seen bool
	for time.Now().Before(deadline) && !seen {
		for _, p := range a.Engine.Positions() {
			if p.TradingSymbol == symbol && p.NetQuantity == 75 {
				seen = true
			}
		}
		if !seen {
			time.Sleep(20 * time.Millisecond)
		}
	}
	elapsed := time.Since(start)

	if !seen {
		t.Fatal("the position never appeared")
	}
	// The old behaviour waited for the 3s timer; anything near that means the
	// on-fill refresh is not working.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("position appeared after %s — a fill should refresh the book "+
			"immediately, not wait for the sync timer", elapsed.Round(time.Millisecond))
	}
	t.Logf("position visible %s after the order", elapsed.Round(time.Millisecond))
}

// TestEngineDerivesThePaperBroker pins the fix directly: constructing an engine
// with a paper broker must wire the price feed whether or not the caller also
// passes WithPaperBroker.
func TestEngineDerivesThePaperBroker(t *testing.T) {
	a := newTestApp(t)
	if a.Engine.BrokerMode() != "paper" {
		t.Fatalf("broker mode = %q, want paper", a.Engine.BrokerMode())
	}

	// Feeding a tick must reach the simulated broker; if it does not, a market
	// order cannot fill.
	const symbol = "TESTSYM"
	a.Engine.HandleTickForTest(marketdata.Tick{
		TradingSymbol: symbol, LastPrice: 42, Timestamp: time.Now(),
	})
	if got := a.Engine.LTP(symbol); got != 42 {
		t.Errorf("engine LTP = %v, want 42", got)
	}

	o, err := a.Engine.PlaceManualOrder(context.Background(), broker.OrderRequest{
		Exchange: "NFO", TradingSymbol: symbol, Product: broker.ProductMIS,
		OrderType: broker.OrderTypeMarket, Side: broker.SideBuy, Quantity: 1,
	})
	if err != nil {
		t.Fatalf("PlaceManualOrder: %v", err)
	}
	if o.Status != broker.StatusComplete {
		t.Errorf("status = %s, want COMPLETE — the paper broker is not receiving prices", o.Status)
	}
}
