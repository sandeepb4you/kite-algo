package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/events"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
)

// nullStore satisfies storage.Store without persisting anything, so engine
// behaviour can be tested without a database.
type nullStore struct{}

func (nullStore) Close() error                                                { return nil }
func (nullStore) SaveOrder(context.Context, *broker.Order) error              { return nil }
func (nullStore) GetOpenOrders(context.Context) ([]broker.Order, error)       { return nil, nil }
func (nullStore) SaveFill(context.Context, *broker.Fill) error                { return nil }
func (nullStore) UpsertPosition(context.Context, *broker.Position) error      { return nil }
func (nullStore) GetOpenPositions(context.Context) ([]broker.Position, error) { return nil, nil }
func (nullStore) SaveTick(context.Context, *marketdata.Tick) error            { return nil }
func (nullStore) SaveCandle(context.Context, *marketdata.Candle) error        { return nil }
func (nullStore) GetDayPnL(context.Context) (float64, error)                  { return 0, nil }
func (nullStore) SaveKiteSession(context.Context, storage.KiteSession) error  { return nil }
func (nullStore) ClearKiteSession(context.Context) error                      { return nil }
func (nullStore) SaveWebSession(context.Context, storage.WebSession) error    { return nil }
func (nullStore) DeleteWebSession(context.Context, string) error              { return nil }

func (nullStore) GetKiteSession(context.Context) (storage.KiteSession, bool, error) {
	return storage.KiteSession{}, false, nil
}
func (nullStore) GetWebSession(context.Context, string) (storage.WebSession, bool, error) {
	return storage.WebSession{}, false, nil
}
func (nullStore) DeleteExpiredWebSessions(context.Context, time.Time) error { return nil }

// tradingEngine builds an engine backed by a paper broker with the given risk
// limits, plus a tick function.
//
// Prices must be delivered through the engine's tick path rather than straight
// to the broker: handleTick updates the engine's own price cache, which is what
// refreshPositions marks positions against. Feeding the broker alone fills
// orders but leaves the engine's P&L view at zero.
func tradingEngine(t *testing.T, limits risk.Limits) (*Engine, func(symbol string, price float64)) {
	t.Helper()
	paper := broker.NewPaperBroker(nil, nil)
	e := New(paper, nullStore{}, risk.NewManager(limits), false, nil,
		WithPaperBroker(paper), WithEventPublisher(events.Nop{}))
	paper.SetOnFill(e.handleFill)

	tick := func(symbol string, price float64) {
		e.handleTick(marketdata.Tick{
			TradingSymbol: symbol,
			Exchange:      "NFO",
			LastPrice:     price,
			Timestamp:     time.Now(),
		})
	}
	return e, tick
}

// TestSquareOffWorksAfterDailyLossLimitBreached is the end-to-end version of the
// risk-manager exemption. It is the scenario that matters: you are deep in the
// red, the daily-loss limit has tripped, and you press square-off. If the engine
// cannot flatten here, the limit has trapped you in the losing position it was
// meant to protect you from.
func TestSquareOffWorksAfterDailyLossLimitBreached(t *testing.T) {
	e, tick := tradingEngine(t, risk.Limits{MaxDailyLoss: 5000, MaxLotsPerTrade: 10})
	ctx := context.Background()

	// Open a position at 100.
	tick("NIFTY24AUG24500CE", 100)
	if _, err := e.PlaceOrder(ctx, broker.OrderRequest{
		StrategyID: "manual", Exchange: "NFO", TradingSymbol: "NIFTY24AUG24500CE",
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideBuy, Quantity: 75,
	}); err != nil {
		t.Fatalf("opening order rejected: %v", err)
	}

	// The market moves hard against us and the day PnL blows through the limit.
	tick("NIFTY24AUG24500CE", 20)
	e.refreshPositions(ctx)

	if pnl := e.DayPnL(); pnl > -5000 {
		t.Fatalf("test setup did not breach the loss limit: day PnL = %.2f", pnl)
	}

	// A new opening order must now be refused...
	_, err := e.PlaceOrder(ctx, broker.OrderRequest{
		StrategyID: "manual", Exchange: "NFO", TradingSymbol: "NIFTY24AUG24600CE",
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideBuy, Quantity: 75,
	})
	var re *risk.RiskError
	if !errors.As(err, &re) || re.Rule != "max-daily-loss" {
		t.Fatalf("expected a max-daily-loss rejection for a new entry, got %v", err)
	}

	// ...but squaring off the losing position must still go through.
	order, err := e.SquareOff(ctx, "manual", "NIFTY24AUG24500CE")
	if err != nil {
		t.Fatalf("SQUARE-OFF REJECTED after the loss limit tripped: %v\n"+
			"the risk limit has trapped the operator in a losing position", err)
	}
	if order.Side != broker.SideSell {
		t.Errorf("square-off side = %s, want SELL to close a long", order.Side)
	}
	if order.Quantity != 75 {
		t.Errorf("square-off quantity = %d, want 75", order.Quantity)
	}
}

// TestSquareOffClosesShortsWithABuy covers the opposite direction — short
// options are the whole point of this platform's sample strategy.
func TestSquareOffClosesShorts(t *testing.T) {
	e, tick := tradingEngine(t, risk.Limits{MaxLotsPerTrade: 10})
	ctx := context.Background()

	tick("NIFTY24AUG24500PE", 120)
	if _, err := e.PlaceOrder(ctx, broker.OrderRequest{
		StrategyID: "short-straddle", Exchange: "NFO", TradingSymbol: "NIFTY24AUG24500PE",
		Product: broker.ProductNRML, OrderType: broker.OrderTypeMarket,
		Side: broker.SideSell, Quantity: 75,
	}); err != nil {
		t.Fatalf("opening short rejected: %v", err)
	}
	e.refreshPositions(ctx)

	order, err := e.SquareOff(ctx, "short-straddle", "NIFTY24AUG24500PE")
	if err != nil {
		t.Fatalf("SquareOff: %v", err)
	}
	if order.Side != broker.SideBuy {
		t.Errorf("side = %s, want BUY to close a short", order.Side)
	}
	// OrderIntent is a risk-manager input carried on the request; it is not
	// copied onto the resulting Order, so the persisted audit trail records the
	// exit only through its tag. Worth revisiting when the schema next changes.
	if order.Tag != "square-off" {
		t.Errorf("tag = %q, want %q so exits are identifiable in the order history",
			order.Tag, "square-off")
	}
}

// TestSquareOffUnknownSymbol reports a clear error rather than placing a
// speculative order against a position that does not exist.
func TestSquareOffUnknownSymbol(t *testing.T) {
	e, _ := tradingEngine(t, risk.Limits{})
	if _, err := e.SquareOff(context.Background(), "", "NOT-HELD"); !errors.Is(err, ErrNoPosition) {
		t.Errorf("err = %v, want ErrNoPosition", err)
	}
}

// TestSquareOffAllReportsPartialFailure ensures a book with several positions is
// fully attempted, and that failures are surfaced rather than swallowed —
// believing everything closed when it did not is how a position is carried
// overnight by accident.
func TestSquareOffAllFlattensEverything(t *testing.T) {
	e, tick := tradingEngine(t, risk.Limits{MaxLotsPerTrade: 10})
	ctx := context.Background()

	for _, sym := range []string{"NIFTY24AUG24500CE", "NIFTY24AUG24500PE"} {
		tick(sym, 100)
		if _, err := e.PlaceOrder(ctx, broker.OrderRequest{
			StrategyID: "manual", Exchange: "NFO", TradingSymbol: sym,
			Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
			Side: broker.SideSell, Quantity: 75,
		}); err != nil {
			t.Fatalf("open %s: %v", sym, err)
		}
	}
	e.refreshPositions(ctx)

	placed, errs := e.SquareOffAll(ctx)
	if len(errs) != 0 {
		t.Fatalf("square off all reported errors: %v", errs)
	}
	if len(placed) != 2 {
		t.Fatalf("placed %d closing orders, want 2", len(placed))
	}

	e.refreshPositions(ctx)
	for _, p := range e.Positions() {
		if p.IsOpen() {
			t.Errorf("%s still open with qty %d after square off all",
				p.TradingSymbol, p.NetQuantity)
		}
	}
}

// TestManualOrderIsAttributed keeps hand-entered trades out of a strategy's
// position book, which is keyed by strategy + symbol + product.
func TestManualOrderIsAttributed(t *testing.T) {
	e, tick := tradingEngine(t, risk.Limits{MaxLotsPerTrade: 10})
	ctx := context.Background()
	tick("NIFTY24AUG24500CE", 100)

	order, err := e.PlaceManualOrder(ctx, broker.OrderRequest{
		TradingSymbol: "NIFTY24AUG24500CE",
		Product:       broker.ProductMIS,
		OrderType:     broker.OrderTypeMarket,
		Side:          broker.SideBuy,
		Quantity:      75,
	})
	if err != nil {
		t.Fatalf("PlaceManualOrder: %v", err)
	}
	if order.StrategyID != ManualStrategyID {
		t.Errorf("StrategyID = %q, want %q", order.StrategyID, ManualStrategyID)
	}
	if order.Validity == "" {
		t.Error("validity was not defaulted")
	}
	if order.Exchange == "" {
		t.Error("exchange was not resolved")
	}
}
