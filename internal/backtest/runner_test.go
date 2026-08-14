package backtest

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/history"
	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
	"kite-algo/internal/storage/sqlite"
	"kite-algo/internal/strategy"
)

// --- fixtures -------------------------------------------------------------

// fixedStrategy buys on a given bar and sells on another, unconditionally.
// Being strategy-independent, it isolates harness regressions from strategy
// regressions.
type fixedStrategy struct {
	name      string
	trader    strategy.Trader
	symbol    string
	buyAfter  int
	sellAfter int
	ticks     int
	bought    bool
	sold      bool
}

func (s *fixedStrategy) Name() string { return s.name }

func (s *fixedStrategy) Init(_ context.Context, t strategy.Trader, _ config.StrategyCfg) error {
	s.trader = t
	return t.Subscribe([]string{s.symbol})
}

func (s *fixedStrategy) OnTick(ctx context.Context, tick marketdata.Tick) {
	if tick.TradingSymbol != s.symbol {
		return
	}
	s.ticks++

	if !s.bought && s.ticks >= s.buyAfter {
		s.bought = true
		_, _ = s.trader.PlaceOrder(ctx, broker.OrderRequest{
			StrategyID: s.name, Exchange: "NFO", TradingSymbol: s.symbol,
			Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
			Side: broker.SideBuy, Quantity: 75,
		})
		s.trader.Signal(strategy.Signal{StrategyID: s.name, Kind: "enter", Message: "bought"})
		return
	}
	if s.bought && !s.sold && s.ticks >= s.sellAfter {
		s.sold = true
		_, _ = s.trader.PlaceOrder(ctx, broker.OrderRequest{
			StrategyID: s.name, Intent: broker.IntentClose, Exchange: "NFO",
			TradingSymbol: s.symbol, Product: broker.ProductMIS,
			OrderType: broker.OrderTypeMarket, Side: broker.SideSell, Quantity: 75,
		})
	}
}

func (s *fixedStrategy) OnFill(context.Context, broker.Fill) {}
func (s *fixedStrategy) Stop(context.Context) error          { return nil }

// staticProvider serves a fixed candle series.
type staticProvider struct {
	candles map[string][]marketdata.Candle
}

func (p *staticProvider) Name() string { return "static" }

func (p *staticProvider) Candles(_ context.Context, req history.Request) ([]marketdata.Candle, error) {
	var out []marketdata.Candle
	for _, c := range p.candles[req.Symbol] {
		if !c.OpenTime.Before(req.From) && c.OpenTime.Before(req.To) {
			out = append(out, c)
		}
	}
	return out, nil
}

// rampCandles builds a rising series: open 100, +1 per bar.
func rampCandles(symbol string, start time.Time, n int, interval kite.Interval) []marketdata.Candle {
	dur := interval.Duration()
	out := make([]marketdata.Candle, 0, n)
	for i := 0; i < n; i++ {
		base := 100 + float64(i)
		out = append(out, marketdata.Candle{
			TradingSymbol: symbol,
			Interval:      string(interval),
			Open:          base,
			High:          base + 0.5,
			Low:           base - 0.5,
			Close:         base + 0.25,
			Volume:        1000,
			OpenTime:      start.Add(time.Duration(i) * dur),
			CloseTime:     start.Add(time.Duration(i+1) * dur),
		})
	}
	return out
}

func testStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.New(context.Background(), filepath.Join(t.TempDir(), "bt.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedSnapshot writes the instrument snapshot a backtest needs to resolve
// symbols, standing in for a day the live server would have captured.
func seedSnapshot(t *testing.T, st *sqlite.Store, asOf time.Time, symbol string) {
	t.Helper()
	err := st.SaveInstrumentSnapshot(context.Background(), asOf, []storage.InstrumentRow{{
		InstrumentToken: 12345, TradingSymbol: symbol, Name: "NIFTY",
		Expiry: asOf.AddDate(0, 0, 7), Strike: 24500, LotSize: 75,
		InstrumentType: "CE", Exchange: "NFO",
	}})
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func fixedRegistry(symbol string, buyAfter, sellAfter int) *strategy.Registry {
	reg := strategy.NewRegistry()
	reg.Register(strategy.Descriptor{
		Type:  "fixed",
		Title: "Fixed test strategy",
		Factory: func(id string, _ *slog.Logger) (strategy.Strategy, error) {
			return &fixedStrategy{name: id, symbol: symbol,
				buyAfter: buyAfter, sellAfter: sellAfter}, nil
		},
	})
	return reg
}

// --- tests ----------------------------------------------------------------

const testSymbol = "NIFTY24AUG24500CE"

func baseConfig(from time.Time) Config {
	return Config{
		StrategyType:   "fixed",
		Symbols:        []string{testSymbol},
		Interval:       kite.Interval5Minute,
		From:           from,
		To:             from.Add(4 * time.Hour),
		BarPath:        PathOHLC,
		Costs:          CostModel{}, // zero costs unless a test sets them
		Risk:           risk.Limits{MaxLotsPerTrade: 10},
		InitialCapital: 100000,
	}
}

// TestRunnerExecutesARoundTrip is the end-to-end smoke test: data in, a trade
// out, with prices from the replayed series.
func TestRunnerExecutesARoundTrip(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)

	store := testStore(t)
	seedSnapshot(t, store, from, testSymbol)

	provider := &staticProvider{candles: map[string][]marketdata.Candle{
		testSymbol: rampCandles(testSymbol, from, 20, kite.Interval5Minute),
	}}

	runner, err := New(baseConfig(from), fixedRegistry(testSymbol, 2, 10), provider, store, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Events == 0 {
		t.Fatal("no events were replayed")
	}
	if len(res.Trades) != 1 {
		t.Fatalf("got %d trades, want 1 round trip", len(res.Trades))
	}

	tr := res.Trades[0]
	if tr.Quantity != 75 {
		t.Errorf("quantity = %d, want 75", tr.Quantity)
	}
	if tr.Direction != broker.SideBuy {
		t.Errorf("direction = %s, want BUY", tr.Direction)
	}
	// The series rises, so a long round trip must profit.
	if tr.NetPnL <= 0 {
		t.Errorf("net P&L = %.2f on a rising series with zero costs, want > 0", tr.NetPnL)
	}
	if !tr.ExitTime.After(tr.EntryTime) {
		t.Error("exit is not after entry")
	}
	// Timestamps must come from the replayed period, not the wall clock.
	if tr.EntryTime.Year() != 2024 {
		t.Errorf("entry stamped %s — the simulated clock is not being used", tr.EntryTime)
	}
	if len(res.Signals) == 0 {
		t.Error("strategy signals were not captured")
	}
}

// TestRunnerIsDeterministic is the property that makes a backtest a measurement
// rather than an anecdote.
func TestRunnerIsDeterministic(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)

	run := func() *Result {
		store := testStore(t)
		seedSnapshot(t, store, from, testSymbol)
		provider := &staticProvider{candles: map[string][]marketdata.Candle{
			testSymbol: rampCandles(testSymbol, from, 30, kite.Interval5Minute),
		}}
		runner, err := New(baseConfig(from), fixedRegistry(testSymbol, 3, 12), provider, store, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		res, err := runner.Run(ctx)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res
	}

	a, b := run(), run()

	if a.Events != b.Events {
		t.Errorf("event counts differ: %d vs %d", a.Events, b.Events)
	}
	if len(a.Trades) != len(b.Trades) {
		t.Fatalf("trade counts differ: %d vs %d", len(a.Trades), len(b.Trades))
	}
	for i := range a.Trades {
		x, y := a.Trades[i], b.Trades[i]
		if x.EntryPrice != y.EntryPrice || x.ExitPrice != y.ExitPrice || x.NetPnL != y.NetPnL {
			t.Errorf("trade %d differs between runs: %+v vs %+v", i, x, y)
		}
		if !x.EntryTime.Equal(y.EntryTime) {
			t.Errorf("trade %d entry time differs: %s vs %s", i, x.EntryTime, y.EntryTime)
		}
	}
	if a.Metrics.NetPnL != b.Metrics.NetPnL {
		t.Errorf("net P&L differs between runs: %.4f vs %.4f", a.Metrics.NetPnL, b.Metrics.NetPnL)
	}
}

// TestOpenPositionsAreForceClosed covers the reporting trap: a strategy that
// never exits would otherwise contribute no trade at all, and its unrealized
// loss would simply disappear from the metrics.
func TestOpenPositionsAreForceClosed(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)

	store := testStore(t)
	seedSnapshot(t, store, from, testSymbol)
	provider := &staticProvider{candles: map[string][]marketdata.Candle{
		testSymbol: rampCandles(testSymbol, from, 20, kite.Interval5Minute),
	}}

	// sellAfter is beyond the series, so the position never closes on its own.
	runner, err := New(baseConfig(from), fixedRegistry(testSymbol, 2, 9999), provider, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if res.ForcedExits != 1 {
		t.Errorf("forced exits = %d, want 1", res.ForcedExits)
	}
	if len(res.Trades) != 1 {
		t.Errorf("got %d trades; an unclosed position must still be accounted for", len(res.Trades))
	}
}

// TestCostsReduceProfit checks the cost model is actually applied — a backtest
// with costs must report less profit than the same run without them.
func TestCostsReduceProfit(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)

	run := func(costs CostModel) *Result {
		store := testStore(t)
		seedSnapshot(t, store, from, testSymbol)
		provider := &staticProvider{candles: map[string][]marketdata.Candle{
			testSymbol: rampCandles(testSymbol, from, 20, kite.Interval5Minute),
		}}
		cfg := baseConfig(from)
		cfg.Costs = costs
		runner, err := New(cfg, fixedRegistry(testSymbol, 2, 10), provider, store, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := runner.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	free := run(CostModel{})
	charged := run(DefaultNSEOptionCosts())

	if charged.Metrics.TotalCosts <= 0 {
		t.Fatal("cost model produced no charges")
	}
	if charged.Metrics.NetPnL >= free.Metrics.NetPnL {
		t.Errorf("net P&L with costs (%.2f) is not below the cost-free run (%.2f)",
			charged.Metrics.NetPnL, free.Metrics.NetPnL)
	}
	// Slippage should also worsen the entry relative to the frictionless run.
	if charged.Trades[0].EntryPrice <= free.Trades[0].EntryPrice {
		t.Errorf("entry with slippage %.2f is not worse than %.2f",
			charged.Trades[0].EntryPrice, free.Trades[0].EntryPrice)
	}
}

// TestWarmupBlocksEarlyOrders confirms the warmup window is enforced and its
// rejections counted, so an operator can tell when it was set too long.
func TestWarmupBlocksEarlyOrders(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)

	store := testStore(t)
	seedSnapshot(t, store, from, testSymbol)
	provider := &staticProvider{candles: map[string][]marketdata.Candle{
		testSymbol: rampCandles(testSymbol, from, 20, kite.Interval5Minute),
	}}

	cfg := baseConfig(from)
	cfg.Warmup = 3 * time.Hour // covers most of the series

	runner, err := New(cfg, fixedRegistry(testSymbol, 2, 10), provider, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if res.WarmupSkips == 0 {
		t.Error("no orders were rejected during a warmup covering most of the run")
	}
}

// TestMissingSnapshotFailsClearly is the honest-failure case: a backtest over a
// period with no captured instrument master cannot resolve its contracts, and
// must say so rather than silently trading nothing.
func TestMissingSnapshotFailsClearly(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)

	store := testStore(t) // deliberately no snapshot
	provider := &staticProvider{}

	runner, err := New(baseConfig(from), fixedRegistry(testSymbol, 2, 10), provider, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx); err == nil {
		t.Fatal("a backtest with no instrument snapshot should fail loudly")
	}
}

// TestFeedAddMidRunHasNoLookahead is the bias check. A symbol attached at 11:00
// must not deliver the morning's bars: reacting to prices the strategy could not
// have seen is the easiest way to build a backtest that cannot be reproduced.
func TestFeedAddMidRunHasNoLookahead(t *testing.T) {
	ctx := context.Background()
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)

	provider := &staticProvider{candles: map[string][]marketdata.Candle{
		"LATE": rampCandles("LATE", from, 20, kite.Interval5Minute),
	}}

	clock := NewSimClock(from)
	feed, err := NewCandleFeed(ctx, FeedConfig{
		Provider: provider, Interval: kite.Interval5Minute,
		From: from, To: from.Add(4 * time.Hour), Path: PathCloseOnly, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Advance the clock an hour, then attach the symbol.
	midpoint := from.Add(time.Hour)
	clock.Set(midpoint)
	if err := feed.Add(ctx, "LATE"); err != nil {
		t.Fatal(err)
	}

	count := 0
	for {
		ev, ok := feed.Next()
		if !ok {
			break
		}
		count++
		if ev.Time.Before(midpoint) {
			t.Fatalf("event at %s predates the subscription at %s — lookahead bias",
				ev.Time.Format(time.RFC3339), midpoint.Format(time.RFC3339))
		}
	}
	if count == 0 {
		t.Error("no events delivered after a mid-run subscription")
	}
}

// TestBarPathPessimistOrdersAdversely documents the intrabar assumption.
func TestBarPathPessimistOrdersAdversely(t *testing.T) {
	f := &CandleFeed{path: PathPessimist, interval: kite.Interval5Minute}

	up := marketdata.Candle{Open: 100, High: 110, Low: 95, Close: 105}
	got := f.pathPrices(up)
	if got[1] != 95 {
		t.Errorf("up bar path = %v; the low should come before the high", got)
	}

	down := marketdata.Candle{Open: 100, High: 110, Low: 95, Close: 97}
	got = f.pathPrices(down)
	if got[1] != 110 {
		t.Errorf("down bar path = %v; the high should come before the low", got)
	}
}
