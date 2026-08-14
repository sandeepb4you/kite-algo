package backtest

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"kite-algo/internal/analytics"
	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/history"
	"kite-algo/internal/kite"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
	"kite-algo/internal/strategy"
)

// Config describes one backtest run.
type Config struct {
	StrategyType   string
	InstanceID     string
	Params         map[string]any
	Symbols        []string // seed symbols; more arrive via Subscribe
	Interval       kite.Interval
	From           time.Time
	To             time.Time
	BarPath        BarPath
	Costs          CostModel
	Risk           risk.Limits
	InitialCapital float64
	Warmup         time.Duration
	RiskFreeRate   float64
}

// Result is everything a run produced.
type Result struct {
	Config      Config                  `json:"-"`
	Metrics     analytics.Metrics       `json:"metrics"`
	Trades      []analytics.Trade       `json:"trades"`
	Equity      []analytics.EquityPoint `json:"equity"`
	Signals     []strategy.Signal       `json:"signals"`
	Fills       []broker.Fill           `json:"-"`
	Symbols     []string                `json:"symbols"`
	Duration    time.Duration           `json:"duration"`
	Events      int                     `json:"events"`
	WarmupSkips int                     `json:"warmup_skips"`
	// ForcedExits counts positions still open at the end of the window, closed
	// by the runner so unrealized P&L is not silently reported as profit.
	ForcedExits int    `json:"forced_exits"`
	DataSource  string `json:"data_source"`
}

// Runner replays historical data through a strategy.
//
// It runs on a single goroutine with no concurrency at all. That is a
// correctness requirement, not a simplification: a backtest is a measurement,
// and a measurement whose result depends on goroutine scheduling cannot be
// reproduced or trusted.
type Runner struct {
	cfg      Config
	registry *strategy.Registry
	provider history.Provider
	store    storage.HistoryStore
	logger   *slog.Logger

	progress float64
}

// New builds a runner.
func New(cfg Config, registry *strategy.Registry, provider history.Provider, store storage.HistoryStore, logger *slog.Logger) (*Runner, error) {
	if cfg.StrategyType == "" {
		return nil, fmt.Errorf("backtest: no strategy type")
	}
	if !cfg.To.After(cfg.From) {
		return nil, fmt.Errorf("backtest: 'to' must be after 'from'")
	}
	if cfg.Interval == "" {
		cfg.Interval = kite.Interval5Minute
	}
	if cfg.BarPath == "" {
		cfg.BarPath = PathPessimist
	}
	if cfg.InitialCapital <= 0 {
		cfg.InitialCapital = 100000
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = cfg.StrategyType
	}
	if cfg.RiskFreeRate == 0 {
		cfg.RiskFreeRate = 0.06
	}
	return &Runner{
		cfg: cfg, registry: registry, provider: provider, store: store, logger: logger,
	}, nil
}

// Progress reports completion between 0 and 1.
func (r *Runner) Progress() float64 { return r.progress }

// Run replays the window and returns the result.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	started := time.Now() // wall clock, for the run's own duration only

	// Point-in-time instrument master. Without a snapshot from the period, the
	// contracts a strategy would have traded cannot be resolved at all — Kite
	// drops expired options from its live feed.
	instr, err := history.LoadAsOf(ctx, r.store, r.cfg.From)
	if err != nil {
		return nil, err
	}

	clock := NewSimClock(r.cfg.From)

	simBroker := broker.NewPaperBroker(nil, nil)
	simBroker.SetClock(clock.Now)
	simBroker.SetFillModel(SlippageFillModel{Model: r.cfg.Costs})

	feed, err := NewCandleFeed(ctx, FeedConfig{
		Provider: r.provider,
		Interval: r.cfg.Interval,
		From:     r.cfg.From,
		To:       r.cfg.To,
		Path:     r.cfg.BarPath,
		Clock:    clock,
		Symbols:  r.cfg.Symbols,
	})
	if err != nil {
		return nil, err
	}

	trader := &Trader{
		clock:       clock,
		broker:      simBroker,
		risk:        risk.NewManager(r.cfg.Risk),
		feed:        feed,
		instr:       instr,
		prices:      make(map[string]float64),
		warmupUntil: r.cfg.From.Add(r.cfg.Warmup),
	}
	simBroker.SetOnFill(trader.onFill)

	// Build and initialize the strategy exactly as the live engine would.
	inst, _, err := r.registry.New(r.cfg.StrategyType, r.cfg.InstanceID, r.logger)
	if err != nil {
		return nil, err
	}
	strategyCfg := config.StrategyCfg{
		Name: r.cfg.InstanceID, Enabled: true, Params: r.cfg.Params,
	}
	if err := inst.Init(ctx, trader, strategyCfg); err != nil {
		return nil, fmt.Errorf("initialize %s: %w", r.cfg.StrategyType, err)
	}

	events := 0
	for {
		ev, ok := feed.Next()
		if !ok {
			break
		}
		if err := ctx.Err(); err != nil {
			// A cancelled run still returns what it measured so far.
			return r.finish(ctx, trader, simBroker, feed, events, started, err)
		}

		events++
		// The clock moves BEFORE dispatch, so a strategy asking the time during
		// OnTick sees this event's moment, not the previous one's.
		clock.Set(ev.Time)
		trader.prices[ev.Tick.TradingSymbol] = ev.Tick.LastPrice

		// Price first, then the strategy: pending orders must fill against this
		// price before the strategy reacts to it, which is the ordering the live
		// engine uses too.
		simBroker.OnPrice(ev.Tick.TradingSymbol, ev.Tick.LastPrice)
		inst.OnTick(ctx, ev.Tick)

		r.progress = feed.Progress()
	}

	// Let the strategy unwind on its own terms before the runner forces it.
	if f, ok := inst.(strategy.Flattener); ok {
		_ = f.SquareOff(ctx, "backtest end")
		r.drainPendingFills(simBroker, trader)
	}
	_ = inst.Stop(ctx)

	return r.finish(ctx, trader, simBroker, feed, events, started, nil)
}

// drainPendingFills lets orders placed during teardown execute at the last
// known prices, since no further ticks are coming.
func (r *Runner) drainPendingFills(b *broker.PaperBroker, t *Trader) {
	for symbol, price := range t.prices {
		b.OnPrice(symbol, price)
	}
}

// finish force-closes anything still open and computes the result.
func (r *Runner) finish(ctx context.Context, t *Trader, b *broker.PaperBroker, feed Feed, events int, started time.Time, runErr error) (*Result, error) {
	forced := r.forceFlatten(ctx, t, b)

	trades := analytics.BuildTrades(t.fills, r.cfg.Costs.CostOf)
	metrics := analytics.Compute(trades, r.cfg.InitialCapital, r.cfg.RiskFreeRate)
	equity := analytics.BuildEquityCurve(trades, r.cfg.InitialCapital)

	var symbols []string
	if cf, ok := feed.(*CandleFeed); ok {
		symbols = cf.Symbols()
	}

	res := &Result{
		Config:      r.cfg,
		Metrics:     metrics,
		Trades:      trades,
		Equity:      equity,
		Signals:     t.signals,
		Fills:       t.fills,
		Symbols:     symbols,
		Duration:    time.Since(started),
		Events:      events,
		WarmupSkips: t.warmupSkips,
		ForcedExits: forced,
		DataSource:  r.provider.Name(),
	}
	return res, runErr
}

// forceFlatten closes positions still open when the window ends.
//
// Without this, an unclosed position contributes no trade at all and its
// unrealized loss simply vanishes from the report — the most flattering possible
// treatment of a strategy that never exits. Closing at the last known price is
// an assumption, but it is a conservative and visible one, counted in the result.
func (r *Runner) forceFlatten(ctx context.Context, t *Trader, b *broker.PaperBroker) int {
	positions, err := b.GetPositions(ctx)
	if err != nil {
		return 0
	}

	forced := 0
	for _, p := range positions {
		if p.NetQuantity == 0 {
			continue
		}
		price := t.prices[p.TradingSymbol]
		if price <= 0 {
			price = p.AveragePrice
		}

		side := broker.SideSell
		qty := p.NetQuantity
		if qty < 0 {
			side = broker.SideBuy
			qty = -qty
		}

		// Bypass the trader so neither warmup nor a tripped risk limit can
		// prevent the book being closed out for accounting.
		if _, err := b.PlaceOrder(ctx, broker.OrderRequest{
			StrategyID:    p.StrategyID,
			Intent:        broker.IntentClose,
			Exchange:      p.Exchange,
			TradingSymbol: p.TradingSymbol,
			Product:       p.Product,
			OrderType:     broker.OrderTypeMarket,
			Side:          side,
			Quantity:      qty,
			Tag:           "backtest-end",
		}); err != nil {
			continue
		}
		b.OnPrice(p.TradingSymbol, price)
		forced++
	}
	return forced
}
