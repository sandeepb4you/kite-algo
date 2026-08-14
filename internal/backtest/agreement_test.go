package backtest

import (
	"context"
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/engine"
	"kite-algo/internal/events"
	"kite-algo/internal/history"
	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
	"kite-algo/internal/strategy"
)

// nullStore satisfies storage.Store for the paper-side engine.
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
func (nullStore) DeleteExpiredWebSessions(context.Context, time.Time) error   { return nil }

func (nullStore) GetKiteSession(context.Context) (storage.KiteSession, bool, error) {
	return storage.KiteSession{}, false, nil
}
func (nullStore) GetWebSession(context.Context, string) (storage.WebSession, bool, error) {
	return storage.WebSession{}, false, nil
}

// TestPaperAndBacktestAgree is the strongest correctness guarantee available
// here: the same strategy, over the same prices, must produce byte-identical
// executions whether it runs through the live engine's paper broker or through
// the backtester.
//
// It is achievable only because both paths drive the same broker.PaperBroker and
// the same broker.ApplyFill arithmetic. If this test ever fails, a backtest has
// stopped predicting what paper trading would do — which makes every number it
// reports untrustworthy, however plausible it looks.
func TestPaperAndBacktestAgree(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)
	const bars = 30

	// One price per bar, so the two paths see an identical sequence.
	candles := rampCandles(testSymbol, from, bars, kite.Interval5Minute)

	// --- A: the production engine on its paper broker ---
	paperFills := runThroughEngine(t, candles)

	// --- B: the backtester, with friction disabled so only execution differs ---
	store := testStore(t)
	seedSnapshot(t, store, from, testSymbol)
	provider := &staticProvider{candles: map[string][]marketdata.Candle{testSymbol: candles}}

	cfg := baseConfig(from)
	cfg.BarPath = PathCloseOnly // one price per bar, matching the engine feed
	cfg.Costs = CostModel{}     // no slippage, no charges
	cfg.To = from.Add(time.Duration(bars+1) * kite.Interval5Minute.Duration())

	runner, err := New(cfg, fixedRegistry(testSymbol, 3, 12), provider, store, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// --- compare ---
	if len(res.Fills) != len(paperFills) {
		t.Fatalf("backtest produced %d fills, paper produced %d",
			len(res.Fills), len(paperFills))
	}
	if len(paperFills) == 0 {
		t.Fatal("neither path traded; the comparison proves nothing")
	}

	for i := range paperFills {
		p, b := paperFills[i], res.Fills[i]
		if p.TradingSymbol != b.TradingSymbol {
			t.Errorf("fill %d symbol: paper %q, backtest %q", i, p.TradingSymbol, b.TradingSymbol)
		}
		if p.Side != b.Side {
			t.Errorf("fill %d side: paper %s, backtest %s", i, p.Side, b.Side)
		}
		if p.Quantity != b.Quantity {
			t.Errorf("fill %d quantity: paper %d, backtest %d", i, p.Quantity, b.Quantity)
		}
		if p.Price != b.Price {
			t.Errorf("fill %d PRICE: paper %.4f, backtest %.4f — execution has diverged",
				i, p.Price, b.Price)
		}
	}
	t.Logf("paper and backtest agreed on %d fills", len(paperFills))
}

// runThroughEngine replays candle closes through the production engine and
// returns the fills its paper broker produced.
func runThroughEngine(t *testing.T, candles []marketdata.Candle) []broker.Fill {
	t.Helper()
	ctx := context.Background()

	paper := broker.NewPaperBroker(nil, nil)
	eng := engine.New(paper, nullStore{}, risk.NewManager(risk.Limits{MaxLotsPerTrade: 10}),
		false, nil, engine.WithPaperBroker(paper), engine.WithEventPublisher(events.Nop{}))

	var fills []broker.Fill
	paper.SetOnFill(func(f broker.Fill) { fills = append(fills, f) })

	strat := &fixedStrategy{name: "fixed", symbol: testSymbol, buyAfter: 3, sellAfter: 12}
	if err := strat.Init(ctx, eng, config.StrategyCfg{Name: "fixed"}); err != nil {
		t.Fatalf("strategy init: %v", err)
	}
	eng.AddStrategy(strat)

	for _, c := range candles {
		eng.HandleTickForTest(marketdata.Tick{
			TradingSymbol: c.TradingSymbol,
			LastPrice:     c.Close,
			Timestamp:     c.OpenTime,
		})
	}
	return fills
}

// ensure the backtest Trader really satisfies the strategy contract, so a
// strategy cannot behave differently here purely by taking a different path.
var _ strategy.Trader = (*Trader)(nil)

// and that the history provider interface is what the feed consumes.
var _ history.Provider = (*staticProvider)(nil)
