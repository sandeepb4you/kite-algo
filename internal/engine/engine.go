// Package engine is the heart of the platform. The Engine owns the broker,
// risk manager, storage, ticker, and registered strategies. It:
//   - streams ticks from the ticker, records them, and fans them to strategies;
//   - implements strategy.Trader: every order a strategy places is risk-checked,
//     submitted to the broker, and persisted;
//   - reconciles fills (paper via callback, live via order polling) and fans
//     them back to strategies;
//   - keeps a fresh view of positions/PnL so the risk manager never decides on
//     stale data.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/charges"
	"kite-algo/internal/config"
	"kite-algo/internal/events"
	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
	"kite-algo/internal/strategy"
)

// Engine wires the trading platform together and implements strategy.Trader.
type Engine struct {
	broker      broker.Broker
	paperBroker *broker.PaperBroker // non-nil only in paper mode
	store       storage.Store
	risk        *risk.Manager
	// paperRisk applies to the simulated book. Separate limits, because a
	// simulated blow-up should halt the strategies and leave real manual
	// trading alone. Nil falls back to the real manager's limits.
	paperRisk   *risk.Manager
	instruments *kite.Instruments
	ticker      *kite.Ticker
	logger      *slog.Logger
	recordTicks bool

	// pub fans engine activity out to observers (the web UI). Never nil — it
	// defaults to events.Nop so the hot path needs no nil check. Publishing is
	// non-blocking and lossy by design; see package events.
	pub events.Publisher

	// strategyConfigs maps a strategy Name() to its declarative config block.
	strategyConfigs map[string]config.StrategyCfg

	// registry builds strategy instances by type name, so the UI can start one
	// at runtime without the binary knowing every strategy at compile time.
	registry *strategy.Registry

	// Strategy instances are mutated at runtime by HTTP handlers and read on
	// every tick by the market-data goroutine. smu serializes lifecycle
	// transitions; active is a copy-on-write snapshot the hot path reads with no
	// lock at all. Never mutate the slice inside active in place.
	smu     sync.Mutex
	handles map[string]*strategyHandle
	active  atomic.Pointer[[]*strategyHandle]

	// halt is the kill switch, checked before every order.
	halt haltGuard

	mu     sync.RWMutex
	prices map[string]float64 // symbol -> last price
	// rawPositions is the broker's view, whose PnL is REALIZED only. Kept
	// separate from the marked view so re-pricing on every tick stays
	// idempotent instead of compounding unrealized P&L into itself.
	rawPositions []broker.Position
	positions    []broker.Position // rawPositions marked to the latest prices
	dayPnL       float64           // sum of the marked positions, both books
	// realPnL and paperPnL split that sum, so the daily-loss limit that guards
	// real capital is never tripped by a simulated loss.
	realPnL  float64
	paperPnL float64
	// lastPnLPublish throttles tick-driven P&L updates.
	lastPnLPublish time.Time

	// refreshNow lets a fill ask the sync loop for an immediate position
	// refresh. Buffered depth 1 so it coalesces: several fills in quick
	// succession produce one refresh, not one each.
	refreshNow chan struct{}

	// dayCharges accumulates estimated transaction costs for the session, and
	// chargeDay is the IST date they belong to so the total resets overnight
	// rather than growing for the life of the process.
	costModel  charges.Model
	dayCharges charges.Breakdown
	chargeDay  string

	// cmu guards the market-data plumbing, which is now swapped at runtime:
	// the process boots without a Kite session and acquires one when the
	// operator completes the browser login, and swaps again when a token
	// expires and is renewed.
	cmu          sync.RWMutex
	tickerCancel context.CancelFunc
	runCtx       context.Context
	// liveBroker routes MANUAL orders to the exchange while strategies stay on
	// the paper broker. Nil until an operator explicitly confirms live routing.
	liveBroker broker.Broker
	// liveGate is consulted before every real-money entry; see SetLiveGate.
	liveGate func() (bool, string)
	// orderBooks remembers which broker each order went to, so a cancel reaches
	// the one that actually holds it.
	orderBooks *orderBooks

	// wanted is every symbol anyone has asked to stream, whether or not a
	// ticker existed at the time. Subscribe requests made before login (by
	// strategies at Init, or by the UI) are replayed in AttachMarketData —
	// without this they would be silently dropped and the subscriber would
	// simply never receive data.
	//
	// pinned is the subset that a strategy depends on. Browser subscriptions
	// come and go as tabs open and close, but unsubscribing a symbol a strategy
	// is trading would blind it mid-position — so pinned symbols are never
	// released by Unsubscribe.
	wantedMu sync.Mutex
	wanted   map[string]struct{}
	pinned   map[string]struct{}

	// lastTickAt is the arrival time of the most recent tick, in Unix nanos.
	// Atomic rather than mutex-guarded: it is written on the ticker's read
	// goroutine for every tick and read by health checks, so it must not
	// contend with anything on the hot path.
	lastTickAt atomic.Int64

	// orderStrategy maps an order id (internal for paper, exchange for live) to
	// the strategy that placed it, so fills can be routed back.
	orderStrategy sync.Map

	// liveSeen tracks per-order last-seen filled quantity, used to emit
	// incremental fills when polling live order updates. It is written from two
	// goroutines — handleOrderUpdate runs on the ticker's read loop, while
	// reconcileLiveOrders runs on the reconcile loop — so it MUST be held under
	// liveSeenMu. Without the lock this is a concurrent map write and the
	// process dies the first time a live order fills while reconcile is polling.
	liveSeenMu sync.Mutex
	liveSeen   map[string]int
}

// Option configures an Engine at construction.
type Option func(*Engine)

// WithTicker attaches a Kite ticker; the engine will run it on Start.
func WithTicker(t *kite.Ticker) Option {
	return func(e *Engine) { e.ticker = t }
}

// WithPaperBroker attaches the paper broker so the engine can feed it prices.
func WithPaperBroker(p *broker.PaperBroker) Option {
	return func(e *Engine) { e.paperBroker = p }
}

// WithInstruments attaches the instrument master (for lot sizes / symbols).
func WithInstruments(m *kite.Instruments) Option {
	return func(e *Engine) { e.instruments = m }
}

// WithStrategyConfigs attaches the declarative per-strategy config blocks so
// each strategy's Init receives its params from the YAML config.
func WithStrategyConfigs(cfgs map[string]config.StrategyCfg) Option {
	return func(e *Engine) { e.strategyConfigs = cfgs }
}

// WithEventPublisher attaches an observer for engine activity — ticks, orders,
// fills, position refreshes. Used by the web UI to push updates to the browser.
// Publishing is best-effort and never blocks the trading path.
func WithEventPublisher(p events.Publisher) Option {
	return func(e *Engine) {
		if p != nil {
			e.pub = p
		}
	}
}

// New constructs an Engine. Strategies are added via AddStrategy before Start.
func New(b broker.Broker, store storage.Store, r *risk.Manager, recordTicks bool, logger *slog.Logger, opts ...Option) *Engine {
	e := &Engine{
		broker:      b,
		store:       store,
		risk:        r,
		logger:      logger,
		recordTicks: recordTicks,
		prices:      make(map[string]float64),
		liveSeen:    make(map[string]int),
		wanted:      make(map[string]struct{}),
		pinned:      make(map[string]struct{}),
		handles:     make(map[string]*strategyHandle),
		refreshNow:  make(chan struct{}, 1),
		costModel:   charges.DefaultNSEOptions(),
		pub:         events.Nop{},
		orderBooks:  newOrderBooks(),
	}
	for _, opt := range opts {
		opt(e)
	}

	// Detect a paper broker automatically rather than relying on the caller to
	// also pass WithPaperBroker.
	//
	// Forgetting it is silent and total: handleTick only feeds prices to
	// e.paperBroker, so a nil one means the simulated broker never learns a
	// price, no order is ever marketable, and every paper order sits PENDING
	// for ever. Nothing logs an error — it simply does not trade. Deriving it
	// from the broker that was actually supplied removes the failure mode.
	if e.paperBroker == nil {
		if p, ok := b.(*broker.PaperBroker); ok {
			e.paperBroker = p
		}
	}

	// Route fills at construction, not at Start. Between the two there is a
	// window in which an order can be placed and filled, and a nil callback
	// drops that fill on the floor — no position, no persistence, no error.
	if e.paperBroker != nil {
		e.paperBroker.SetOnFill(e.handleFill)
	}
	return e
}

// WithRegistry attaches the strategy registry, enabling runtime start/stop.
func WithRegistry(r *strategy.Registry) Option {
	return func(e *Engine) { e.registry = r }
}

// AddStrategy registers an already-constructed strategy instance.
//
// Prefer StartStrategy, which builds the instance from the registry and
// validates its parameters. This exists for tests and for callers holding a
// concrete strategy value.
func (e *Engine) AddStrategy(s strategy.Strategy) {
	e.smu.Lock()
	defer e.smu.Unlock()

	id := s.Name()
	e.handles[id] = &strategyHandle{
		id:        id,
		typ:       id,
		inst:      s,
		params:    map[string]any{},
		cancel:    func() {},
		state:     StateRunning,
		startedAt: time.Now(),
	}
	e.rebuildActiveLocked()
}

// Start initializes strategies and background loops, then blocks until ctx is
// done.
//
// Start deliberately does NOT require market data. The process boots before the
// operator has completed the Zerodha browser login, so there may be no ticker
// and no instrument master yet; both arrive later via AttachMarketData, which
// runs the ticker under its own cancellable context. Making Start independent
// of the Kite session is what lets the web UI come up and serve the login page
// instead of the process exiting.
func (e *Engine) Start(ctx context.Context) error {
	e.cmu.Lock()
	e.runCtx = ctx
	ticker := e.ticker
	e.cmu.Unlock()

	// Route paper-broker fills back through the engine so they're persisted and
	// fanned out to strategies. Done here (not at construction) because the
	// engine owns handleFill and is built after the broker.
	if pb := e.currentPaperBroker(); pb != nil {
		pb.SetOnFill(e.handleFill)
	}

	// Initialize any strategies added before Start (the AddStrategy path).
	//
	// Only those never initialized. Instances created by StartStrategy are
	// initialized on creation, and Init is NOT idempotent for a strategy holding
	// a position — shortstraddle's resets its leg map, so re-running it on an
	// instance that had adopted open legs made it believe it was flat and enter
	// a second time on top of the first.
	for _, h := range e.activeStrategies() {
		e.smu.Lock()
		done := h.initialized
		e.smu.Unlock()
		if done {
			continue
		}
		if e.logger != nil {
			e.logger.Info("initializing strategy", "name", h.id)
		}
		if err := h.inst.Init(ctx, e, e.configFor(h.id)); err != nil {
			return err
		}
		e.smu.Lock()
		h.initialized = true
		e.smu.Unlock()
	}

	// Position/PnL sync loop: keeps the risk view fresh and persists positions.
	go e.syncLoop(ctx)

	// Live fill reconciliation. Always run the supervisor and re-check the mode
	// on each pass: the broker can be swapped from paper to live at runtime once
	// the operator confirms, so a one-shot decision here would leave live fills
	// unreconciled for the rest of the session.
	go e.reconcileLoop(ctx)

	// A ticker supplied at construction (the classic CLI path) is started here;
	// otherwise AttachMarketData starts one when the Kite session is ready.
	if ticker != nil {
		e.AttachMarketData(e.instruments, ticker)
	}

	<-ctx.Done()
	return ctx.Err()
}

// AttachMarketData installs a Kite session's instrument master and ticker and
// starts streaming. It is safe to call repeatedly: any previous ticker is
// cancelled and closed first, so a renewed session after token expiry cleanly
// replaces the old one.
//
// Subscriptions requested before market data existed are replayed here.
func (e *Engine) AttachMarketData(ins *kite.Instruments, tk *kite.Ticker) {
	if tk == nil {
		return
	}

	e.cmu.Lock()
	if e.tickerCancel != nil {
		e.tickerCancel()
		e.tickerCancel = nil
	}
	if e.ticker != nil && e.ticker != tk {
		e.ticker.Close()
	}
	if ins != nil {
		e.instruments = ins
	}
	e.ticker = tk
	parent := e.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	e.tickerCancel = cancel
	e.cmu.Unlock()

	tk.OnTick = e.handleTick
	tk.OnOrder = e.handleOrderUpdate
	tk.OnError = func(err error) {
		if e.logger != nil {
			e.logger.Warn("ticker error", "err", err)
		}
	}

	go func() {
		err := tk.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && e.logger != nil {
			e.logger.Warn("ticker stopped", "err", err)
		}
	}()

	// Replay everything anyone asked for while we had no ticker. streamSymbols
	// rather than Subscribe, so replaying does not promote transient UI
	// subscriptions into pinned ones.
	if pending := e.wantedSymbols(); len(pending) > 0 {
		if e.logger != nil {
			e.logger.Info("replaying pending subscriptions", "count", len(pending))
		}
		_ = e.streamSymbols(pending)
	}

	e.pub.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelInfo,
		Message: "market data connected",
	})
}

// DetachMarketData stops streaming and drops the ticker, leaving the engine
// running. Used when a Zerodha token expires: positions and PnL stay on screen
// while the operator logs in again.
func (e *Engine) DetachMarketData() {
	e.cmu.Lock()
	if e.tickerCancel != nil {
		e.tickerCancel()
		e.tickerCancel = nil
	}
	if e.ticker != nil {
		e.ticker.Close()
		e.ticker = nil
	}
	e.cmu.Unlock()

	e.pub.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelWarn,
		Message: "market data disconnected",
	})
}

// SwapBroker replaces the order-routing broker at runtime. Used to install the
// live broker after the operator confirms live trading, and to fall back to
// paper on disarm. Open positions are unaffected; only subsequent orders route
// differently.
func (e *Engine) SwapBroker(b broker.Broker) {
	if b == nil {
		return
	}
	e.cmu.Lock()
	e.broker = b
	// The paper broker's price feed is only meaningful while it is the active
	// broker; keep the reference only when it still is.
	if p, ok := b.(*broker.PaperBroker); ok {
		e.paperBroker = p
		p.SetOnFill(e.handleFill)
	} else {
		e.paperBroker = nil
	}
	e.cmu.Unlock()

	e.pub.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelWarn,
		Message: "order routing switched to " + b.Mode(),
		Fields:  map[string]any{"broker_mode": b.Mode()},
	})
}

// currentBroker returns the active order-routing broker. Always use this rather
// than reading e.broker directly: the field is swapped at runtime when the
// operator arms or disarms live trading.
func (e *Engine) currentBroker() broker.Broker {
	e.cmu.RLock()
	defer e.cmu.RUnlock()
	return e.broker
}

// currentPaperBroker returns the paper broker if it is the active one.
func (e *Engine) currentPaperBroker() *broker.PaperBroker {
	e.cmu.RLock()
	defer e.cmu.RUnlock()
	return e.paperBroker
}

// BrokerMode reports how orders are currently routed ("paper" or "live").
func (e *Engine) BrokerMode() string {
	e.cmu.RLock()
	defer e.cmu.RUnlock()
	if e.broker == nil {
		return ""
	}
	return e.broker.Mode()
}

// DayPnL returns the cached realized + unrealized PnL for the trading day.
func (e *Engine) DayPnL() float64 { return e.snapshotDayPnL() }

// RefreshPositions repopulates the position cache from the broker, synchronously.
//
// The cache is normally maintained by syncLoop, which only exists once Start
// has run. Anything that needs positions before then — strategy restore, above
// all — must ask for them rather than read an empty snapshot and conclude the
// book is flat. That conclusion, drawn by a resuming strategy, opens a second
// position on top of the one it already holds.
func (e *Engine) RefreshPositions(ctx context.Context) { e.refreshPositions(ctx) }

// Positions returns a copy of the cached position snapshot.
func (e *Engine) Positions() []broker.Position {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]broker.Position, len(e.positions))
	copy(out, e.positions)
	return out
}

// Prices returns a copy of the last known price for every known symbol.
func (e *Engine) Prices() map[string]float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]float64, len(e.prices))
	for k, v := range e.prices {
		out[k] = v
	}
	return out
}

// HasMarketData reports whether a ticker is currently attached.
func (e *Engine) HasMarketData() bool {
	e.cmu.RLock()
	defer e.cmu.RUnlock()
	return e.ticker != nil
}

// tickStaleAfter is how long without a tick counts as "not streaming".
//
// Generous relative to a live feed, which prints continuously on the indices
// during market hours, and deliberately shorter than the gap between sessions:
// outside trading hours this reports false, which is the truth.
const tickStaleAfter = 90 * time.Second

// Streaming reports whether market data is actually arriving.
//
// Distinct from HasMarketData, which only says a ticker object is attached. The
// two came apart on a live server: a duplicate session activation left the
// engine holding a ticker whose connection had been cancelled, so
// HasMarketData stayed true, /healthz reported streaming, and no tick had
// arrived for a quarter of an hour. A health signal that cannot distinguish
// "connected" from "receiving" is the one that lets a silent outage run.
func (e *Engine) Streaming() bool {
	last := e.lastTickAt.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) < tickStaleAfter
}

// LastTickAt reports when the most recent tick arrived (zero if none has).
func (e *Engine) LastTickAt() time.Time {
	last := e.lastTickAt.Load()
	if last == 0 {
		return time.Time{}
	}
	return time.Unix(0, last)
}

// wantedSymbols returns a snapshot of every symbol anyone has subscribed to.
func (e *Engine) wantedSymbols() []string {
	e.wantedMu.Lock()
	defer e.wantedMu.Unlock()
	out := make([]string, 0, len(e.wanted))
	for s := range e.wanted {
		out = append(out, s)
	}
	sort.Strings(out) // deterministic, so logs and tests are stable
	return out
}

// Stop shuts down every running strategy, squaring off their positions.
//
// The square-off is deliberate and preserves the previous Ctrl-C behaviour: a
// process going down should not leave naked short options open overnight.
// Strategy.Stop itself no longer trades, so the engine has to ask for this
// explicitly.
func (e *Engine) Stop(ctx context.Context) {
	errs := e.StopAllStrategies(ctx, StopOptions{SquareOff: true, Reason: "shutdown"})
	for _, err := range errs {
		if e.logger != nil {
			e.logger.Error("strategy shutdown failed", "err", err)
		}
	}
}

// --- strategy.Trader implementation ---

// PlaceOrder risk-checks and submits an order on behalf of a strategy.
//
// The kill switch is checked first, before the risk manager and before the
// broker. Trader is the only route a strategy has to the market, so this single
// check is what makes "halt" mean halt.
func (e *Engine) PlaceOrder(ctx context.Context, req broker.OrderRequest) (*broker.Order, error) {
	if e.IsHalted() {
		err := e.haltError()
		if e.logger != nil {
			e.logger.Warn("order blocked by kill switch",
				"symbol", req.TradingSymbol, "strategy", req.StrategyID)
		}
		e.pub.Publish(events.Event{
			Kind:       events.KindOrderRejected,
			Symbol:     req.TradingSymbol,
			StrategyID: req.StrategyID,
			Level:      events.LevelError,
			Message:    err.Error(),
			Fields:     map[string]any{"rule": "kill-switch"},
		})
		return nil, err
	}
	return e.placeOrderInternal(ctx, req)
}

// placeOrderInternal is the order path without the kill-switch check.
//
// Engine-initiated square-offs use it: the whole point of halting is to stop
// opening risk, and a halt that also blocked the flatten would leave the
// operator holding a position they explicitly asked to close.
func (e *Engine) placeOrderInternal(ctx context.Context, req broker.OrderRequest) (*broker.Order, error) {
	lotSize := e.LotSize(req.TradingSymbol)
	// Risk is evaluated per book. A strategy losing simulated money must not
	// consume the exposure allowance or trip the daily-loss halt that exists to
	// protect real capital, and vice versa — the two are different money.
	book := e.bookFor(req)
	openPositions := e.snapshotBookPositionCount(book)
	dayPnL := e.snapshotBookPnL(book)

	// The real book can be closed for the day independently of any single
	// order's merits. Exits are exempt: a lockout must never trap the operator
	// in the position that caused it.
	if book.IsReal() && req.Intent != broker.IntentClose {
		if ok, why := e.liveGateAllows(); !ok {
			err := &risk.RiskError{Rule: "live-lockout", Message: why}
			e.pub.Publish(events.Event{
				Kind:       events.KindOrderRejected,
				Symbol:     req.TradingSymbol,
				StrategyID: req.StrategyID,
				Level:      events.LevelWarn,
				Message:    why,
				Fields:     map[string]any{"rule": "live-lockout"},
			})
			return nil, err
		}
	}
	openingNew := openPositions == 0 || !e.hasPosition(req.TradingSymbol)

	if err := e.riskFor(book).Check(ctx, req, lotSize, openPositions, dayPnL, openingNew); err != nil {
		if e.logger != nil {
			e.logger.Warn("order rejected by risk",
				"symbol", req.TradingSymbol, "side", req.Side, "err", err)
		}
		rule := ""
		var re *risk.RiskError
		if errors.As(err, &re) {
			rule = re.Rule
		}
		e.pub.Publish(events.Event{
			Kind:       events.KindOrderRejected,
			Symbol:     req.TradingSymbol,
			StrategyID: req.StrategyID,
			Level:      events.LevelWarn,
			Message:    err.Error(),
			Fields: map[string]any{
				"rule": rule, "side": string(req.Side), "quantity": req.Quantity,
			},
		})
		return nil, err
	}

	router, book := e.brokerFor(req)
	o, err := router.PlaceOrder(ctx, req)
	if err != nil {
		e.pub.Publish(events.Event{
			Kind:       events.KindOrderRejected,
			Symbol:     req.TradingSymbol,
			StrategyID: req.StrategyID,
			Level:      events.LevelError,
			Message:    err.Error(),
			Fields:     map[string]any{"rule": "broker", "side": string(req.Side)},
		})
		return nil, err
	}
	// Remember which strategy owns this order so fills route correctly, and
	// which book it went to so a later cancel reaches the right broker.
	e.orderStrategy.Store(e.orderKey(o), req.StrategyID)
	e.orderBooks.set(o.ID, book)
	if o.ExchangeOrderID != "" {
		e.orderBooks.set(o.ExchangeOrderID, book)
	}
	e.countOrder(req.StrategyID)

	if err := e.store.SaveOrder(ctx, o); err != nil && e.logger != nil {
		e.logger.Error("persist order failed", "id", o.ID, "err", err)
	}
	e.pub.Publish(events.Event{
		Kind:       events.KindOrder,
		Symbol:     o.TradingSymbol,
		StrategyID: o.StrategyID,
		Order:      o,
	})
	return o, nil
}

// CancelOrder cancels a previously placed order.
func (e *Engine) CancelOrder(ctx context.Context, orderID string) error {
	return e.brokerForOrder(orderID).CancelOrder(ctx, orderID)
}

// Now returns wall-clock time. Strategies call this instead of time.Now() so a
// backtest can substitute simulated time; see strategy.Trader.
func (e *Engine) Now() time.Time { return time.Now() }

// Signal records a strategy-authored event and forwards it to the UI feed.
func (e *Engine) Signal(s strategy.Signal) {
	if s.At.IsZero() {
		s.At = time.Now()
	}
	if s.Level == "" {
		s.Level = "info"
	}

	if s.StrategyID != "" {
		e.smu.Lock()
		if h, ok := e.handles[s.StrategyID]; ok {
			sig := s
			h.lastSignal.Store(&sig)
		}
		e.smu.Unlock()
	}

	if e.logger != nil {
		e.logger.Info("strategy signal",
			"strategy", s.StrategyID, "kind", s.Kind, "msg", s.Message)
	}
	e.pub.Publish(events.Event{
		Kind:       events.KindSignal,
		At:         s.At,
		Symbol:     s.Symbol,
		StrategyID: s.StrategyID,
		Level:      events.Level(s.Level),
		Message:    s.Message,
		Fields:     s.Data,
	})
}

// countOrder increments a strategy's order counter for its status card.
func (e *Engine) countOrder(strategyID string) {
	if strategyID == "" {
		return
	}
	e.smu.Lock()
	if h, ok := e.handles[strategyID]; ok {
		h.orders.Add(1)
	}
	e.smu.Unlock()
}

// LTP returns the latest known price for a symbol (0 if unknown).
func (e *Engine) LTP(symbol string) float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.prices[symbol]
}

// LotSize returns the lot size for a symbol from the instrument master (0 unknown).
func (e *Engine) LotSize(symbol string) int {
	if e.instruments == nil {
		return 0
	}
	if inst, ok := e.instruments.Lookup(symbol); ok {
		return inst.LotSize
	}
	return 0
}

// Lookup resolves a trading symbol to the strategy-facing Instrument view.
func (e *Engine) Lookup(symbol string) (strategy.Instrument, bool) {
	if e.instruments == nil {
		return strategy.Instrument{}, false
	}
	k, ok := e.instruments.Lookup(symbol)
	if !ok {
		return strategy.Instrument{}, false
	}
	return toStrategyInstrument(k), true
}

// Options returns the option chain for an underlying's nearest expiry.
func (e *Engine) Options(underlying string, minExpiry time.Time) []strategy.Instrument {
	if e.instruments == nil {
		return nil
	}
	chain := e.instruments.Options(underlying, minExpiry)
	out := make([]strategy.Instrument, 0, len(chain))
	for i := range chain {
		out = append(out, toStrategyInstrument(&chain[i]))
	}
	return out
}

// knownIndexTokens resolves index names to Kite instrument tokens.
//
// Deliberately the SAME table the ticker uses to resolve tokens back to names.
// Two copies would let subscription and tick-decoding disagree, and that
// disagreement is invisible: ticks arrive for a token nothing can name, get
// dropped for having no trading symbol, and the price simply never appears.
var knownIndexTokens = kite.IndexTokens

// indexTokenValid reports whether tok is a well-formed indices-segment token.
func indexTokenValid(tok uint32) bool { return kite.IsIndexToken(tok) }

// segIndices is the Kite exchange-segment code carried in an index token's low
// byte, kept here for the tests that assert the invariant.
const segIndices = 9

// Subscribe resolves trading symbols to tokens and streams them.
//
// Symbols are always recorded as wanted, even when no ticker is attached yet,
// and replayed once one is. Strategies call this from Init — which now runs
// before the Zerodha session exists — so dropping the request here would mean a
// strategy silently never receives the data it was built around.
//
// Symbols requested through this method are PINNED: it is the strategy-facing
// path (strategy.Trader), so the symbols belong to something holding a position
// and must survive a browser tab closing. Use SubscribeTransient for UI-driven
// subscriptions that should be released again.
func (e *Engine) Subscribe(symbols []string) error {
	e.wantedMu.Lock()
	for _, s := range symbols {
		if s != "" {
			e.wanted[s] = struct{}{}
			e.pinned[s] = struct{}{}
		}
	}
	e.wantedMu.Unlock()
	return e.streamSymbols(symbols)
}

// SubscribeTransient streams symbols on behalf of a UI client. Unlike
// Subscribe, these can later be released by Unsubscribe.
func (e *Engine) SubscribeTransient(symbols []string) error {
	e.wantedMu.Lock()
	for _, s := range symbols {
		if s != "" {
			e.wanted[s] = struct{}{}
		}
	}
	e.wantedMu.Unlock()
	return e.streamSymbols(symbols)
}

// Unsubscribe stops streaming symbols that no longer have a UI watcher. Symbols
// pinned by a strategy are ignored — silently cutting market data to a strategy
// that is managing an open position is never acceptable.
func (e *Engine) Unsubscribe(symbols []string) error {
	e.wantedMu.Lock()
	var release []string
	for _, s := range symbols {
		if _, isPinned := e.pinned[s]; isPinned {
			continue
		}
		if _, known := e.wanted[s]; known {
			delete(e.wanted, s)
			release = append(release, s)
		}
	}
	e.wantedMu.Unlock()

	if len(release) == 0 {
		return nil
	}
	e.cmu.RLock()
	ticker, instruments := e.ticker, e.instruments
	e.cmu.RUnlock()
	if ticker == nil {
		return nil
	}

	tokens := make([]uint32, 0, len(release))
	for _, s := range release {
		if instruments != nil {
			if inst, ok := instruments.Lookup(s); ok {
				tokens = append(tokens, inst.InstrumentToken)
				continue
			}
		}
		if tok, ok := knownIndexTokens[s]; ok {
			tokens = append(tokens, tok)
		}
	}
	if len(tokens) > 0 {
		ticker.Unsubscribe(tokens)
	}
	return nil
}

// streamSymbols resolves symbols to tokens and subscribes them on the ticker.
func (e *Engine) streamSymbols(symbols []string) error {
	e.cmu.RLock()
	ticker := e.ticker
	e.cmu.RUnlock()
	if ticker == nil {
		return nil // recorded; AttachMarketData will replay it
	}

	tokens := make([]uint32, 0, len(symbols))
	var unresolved []string
	for _, s := range symbols {
		if e.instruments != nil {
			if inst, ok := e.instruments.Lookup(s); ok {
				tokens = append(tokens, inst.InstrumentToken)
				continue
			}
		}
		// Fallback for index symbols absent from the instrument master.
		if tok, ok := knownIndexTokens[s]; ok {
			tokens = append(tokens, tok)
			continue
		}
		unresolved = append(unresolved, s)
	}
	// A symbol we can't resolve yields no ticks, which for a strategy driven by
	// that symbol means it silently never trades. Never swallow this.
	if len(unresolved) > 0 && e.logger != nil {
		e.logger.Warn("subscribe: symbols could not be resolved to instrument tokens; they will produce NO ticks",
			"symbols", unresolved)
	}
	if len(tokens) > 0 {
		ticker.Subscribe(tokens)
	}
	return nil
}

// toStrategyInstrument converts a kite instrument to the strategy-facing view.
func toStrategyInstrument(k *kite.Instrument) strategy.Instrument {
	return strategy.Instrument{
		Token:          k.InstrumentToken,
		TradingSymbol:  k.TradingSymbol,
		Name:           k.Name,
		Expiry:         k.Expiry,
		Strike:         k.Strike,
		LotSize:        k.LotSize,
		InstrumentType: k.InstrumentType,
		Exchange:       k.Exchange,
	}
}

// --- internal wiring ---

// handleTick is the ticker's OnTick callback: record, update prices, feed the
// paper broker, and fan out to strategies.
func (e *Engine) handleTick(tick marketdata.Tick) {
	// Stamped before the symbol check: an unlabelled tick still proves the feed
	// is alive, and treating it as silence would report a healthy connection as
	// down while pointing at the wrong problem.
	e.lastTickAt.Store(time.Now().UnixNano())

	if tick.TradingSymbol == "" {
		return // can't act without a symbol (instrument master missing)
	}
	if e.recordTicks {
		if err := e.store.SaveTick(context.Background(), &tick); err != nil && e.logger != nil {
			e.logger.Debug("save tick failed", "symbol", tick.TradingSymbol, "err", err)
		}
	}

	e.mu.Lock()
	e.prices[tick.TradingSymbol] = tick.LastPrice
	e.mu.Unlock()

	e.pub.Publish(events.Event{
		Kind:   events.KindTick,
		At:     tick.Timestamp,
		Symbol: tick.TradingSymbol,
		Tick:   &tick,
	})

	// Paper broker evaluates pending orders against the new price.
	if pb := e.currentPaperBroker(); pb != nil {
		pb.OnPrice(tick.TradingSymbol, tick.LastPrice)
	}

	// Re-price open positions against this tick.
	//
	// The authoritative refresh runs every few seconds and asks the BROKER for
	// positions — which in live mode is a rate-limited REST call, so it cannot
	// simply run faster. This marks the cached positions to market instead: no
	// network, no broker call, just arithmetic on data already in hand. Without
	// it P&L visibly lags the price by seconds, which is exactly when you are
	// watching it most closely.
	e.markPositionsToMarket(false)

	for _, h := range e.activeStrategies() {
		h.ticks.Add(1)
		e.deliverTick(h, tick)
	}
}

// HandleTickForTest feeds a tick through the engine's normal market-data path.
//
// Exported solely so the backtest package can drive the production engine with a
// known price series and assert that paper trading and backtesting execute
// identically. Nothing in the running platform should call it.
func (e *Engine) HandleTickForTest(tick marketdata.Tick) { e.handleTick(tick) }

// deliverTick calls one strategy's OnTick, containing any panic.
//
// The fan-out runs on the ticker's read goroutine, so an unrecovered panic in
// one strategy would kill the process — taking down market data, every other
// strategy, and the web UI with it, while positions stayed open in the market.
// A misbehaving strategy is quarantined instead.
func (e *Engine) deliverTick(h *strategyHandle, tick marketdata.Tick) {
	defer func() {
		if rec := recover(); rec != nil {
			e.quarantine(h, fmt.Sprintf("panic in OnTick: %v", rec))
		}
	}()
	h.inst.OnTick(context.Background(), tick)
}

// quarantine marks a strategy errored and removes it from the fan-out.
func (e *Engine) quarantine(h *strategyHandle, reason string) {
	if e.logger != nil {
		e.logger.Error("strategy quarantined after failure",
			"id", h.id, "reason", reason)
	}
	e.smu.Lock()
	h.state = StateErrored
	h.lastErr = reason
	e.rebuildActiveLocked()
	e.smu.Unlock()

	e.pub.Publish(events.Event{
		Kind:       events.KindStatus,
		StrategyID: h.id,
		Level:      events.LevelError,
		Message:    "strategy " + h.id + " stopped after a failure: " + reason,
		Fields:     map[string]any{"strategy_state": string(StateErrored)},
	})
}

// ist is the exchange timezone; the charge total is a per-trading-day figure.
var ist = time.FixedZone("IST", 5*3600+30*60)

// accrueCharges adds a fill's estimated transaction costs to the day's total.
//
// The figures are estimates from the published rate card, not the broker's. Kite
// exposes no real-time charges API — the authoritative numbers arrive on the
// contract note after the close — so anything shown before then is necessarily
// a model, and is labelled as such wherever it appears.
func (e *Engine) accrueCharges(fill broker.Fill) {
	when := fill.Timestamp
	if when.IsZero() {
		when = time.Now()
	}
	day := when.In(ist).Format("2006-01-02")

	e.mu.Lock()
	if e.chargeDay != day {
		// New session: start from zero rather than carrying yesterday forward.
		e.chargeDay = day
		e.dayCharges = charges.Breakdown{}
	}
	e.dayCharges.Add(e.costModel.Charge(fill))
	e.mu.Unlock()
}

// DayCharges returns the session's estimated transaction costs.
func (e *Engine) DayCharges() charges.Breakdown {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dayCharges
}

// pnlPublishInterval bounds how often a re-priced P&L is pushed to observers.
//
// Marking to market is cheap enough to do on every tick, but publishing at tick
// rate would flood the event bus and the browser with figures nobody can read.
// Four updates a second looks continuous to a human and costs nothing.
const pnlPublishInterval = 250 * time.Millisecond

// markPositionsToMarket re-prices the broker's positions against the latest
// prices and publishes the result.
//
// It always derives from rawPositions — the broker's realized figures — so the
// calculation is idempotent no matter how often ticks arrive. force bypasses the
// publish throttle, for the authoritative refresh.
func (e *Engine) markPositionsToMarket(force bool) {
	e.mu.Lock()
	if len(e.rawPositions) == 0 {
		e.positions = nil
		e.dayPnL, e.realPnL, e.paperPnL = 0, 0, 0
		e.mu.Unlock()
		return
	}

	marked := make([]broker.Position, len(e.rawPositions))
	copy(marked, e.rawPositions)

	// P&L is accumulated per book as well as in total. The books hold different
	// kinds of money: a simulated loss must never trip the limit that guards
	// real capital, and a blended figure is neither number.
	var dayPnL, realPnL, paperPnL float64
	for i := range marked {
		if last, ok := e.prices[marked[i].TradingSymbol]; ok && last > 0 {
			marked[i].LastPrice = last
			marked[i].PnL = positionPnL(marked[i], last)
		}
		dayPnL += marked[i].PnL
		if marked[i].Book.IsReal() {
			realPnL += marked[i].PnL
		} else {
			paperPnL += marked[i].PnL
		}
	}
	e.positions = marked
	e.dayPnL = dayPnL
	e.realPnL = realPnL
	e.paperPnL = paperPnL

	due := force || time.Since(e.lastPnLPublish) >= pnlPublishInterval
	if due {
		e.lastPnLPublish = time.Now()
	}
	e.mu.Unlock()

	if due {
		e.pub.Publish(events.Event{
			Kind:      events.KindPositions,
			Positions: marked,
			DayPnL:    dayPnL,
		})
	}
}

// handleFill records a fill and routes it to the owning strategy.
func (e *Engine) handleFill(fill broker.Fill) {
	ctx := context.Background()

	// Persist the parent order before the fill.
	//
	// The paper broker fills synchronously from inside PlaceOrder, so this runs
	// BEFORE PlaceOrder has returned and before the engine has saved the order.
	// fills.order_id is a foreign key, so inserting the fill first violated the
	// constraint, the error was logged and swallowed, and every paper fill was
	// silently discarded — 34 completed orders had produced 0 fill rows. Nothing
	// downstream noticed, because positions are written separately.
	//
	// SaveOrder is an upsert, so the write PlaceOrder does moments later simply
	// updates the same row. Live fills arrive long after their order was saved
	// and take the cheap not-found path.
	if pb := e.currentPaperBroker(); pb != nil {
		if o, ok := pb.GetOrder(fill.OrderID); ok {
			if err := e.store.SaveOrder(ctx, &o); err != nil && e.logger != nil {
				e.logger.Error("persist order before fill failed", "id", o.ID, "err", err)
			}
		}
	}
	if err := e.store.SaveFill(ctx, &fill); err != nil && e.logger != nil {
		e.logger.Error("persist fill failed", "id", fill.ID, "err", err)
	}
	// Persist the affected position (paper broker already updated it).
	if e.currentPaperBroker() != nil {
		e.persistPaperPositions()
	}
	e.accrueCharges(fill)

	// A fill changes the book, so refresh it now rather than waiting for the
	// next timer tick — this is exactly when the operator is watching.
	e.requestRefresh()

	e.pub.Publish(events.Event{
		Kind:       events.KindFill,
		At:         fill.Timestamp,
		Symbol:     fill.TradingSymbol,
		StrategyID: fill.StrategyID,
		Fill:       &fill,
	})
	for _, h := range e.activeStrategies() {
		if fill.StrategyID != "" && fill.StrategyID != h.id {
			continue
		}
		h.fills.Add(1)
		e.deliverFill(h, fill)
	}
}

// deliverFill calls one strategy's OnFill, containing any panic.
func (e *Engine) deliverFill(h *strategyHandle, fill broker.Fill) {
	defer func() {
		if rec := recover(); rec != nil {
			e.quarantine(h, fmt.Sprintf("panic in OnFill: %v", rec))
		}
	}()
	h.inst.OnFill(context.Background(), fill)
}

// handleOrderUpdate turns a live ticker order-update into a fill (best-effort).
// Live fills are also reconciled by the polling loop; this makes paper-style
// latency possible when the order stream is healthy.
func (e *Engine) handleOrderUpdate(ou kite.OrderUpdate) {
	if ou.Status != "COMPLETE" {
		return
	}
	cur := int(ou.FilledQuantity)
	e.liveSeenMu.Lock()
	delta := cur - e.liveSeen[ou.ExchangeOrderID]
	if delta > 0 {
		e.liveSeen[ou.ExchangeOrderID] = cur
	}
	e.liveSeenMu.Unlock()
	if delta <= 0 {
		return
	}

	strategyID, _ := e.orderStrategy.Load(ou.ExchangeOrderID)
	fill := broker.Fill{
		ID:              ou.ExchangeOrderID + "-" + strconv.Itoa(cur),
		ExchangeOrderID: ou.ExchangeOrderID,
		StrategyID:      toStr(strategyID),
		Exchange:        ou.Exchange,
		TradingSymbol:   ou.Tradingsymbol,
		Side:            broker.Side(ou.TransactionType),
		Quantity:        delta,
		Price:           ou.AveragePrice,
		Mode:            "live",
		Timestamp:       time.Now(),
	}
	e.handleFill(fill)
}

// configFor returns the declarative config block for a strategy by name.
func (e *Engine) configFor(name string) config.StrategyCfg {
	if e.strategyConfigs == nil {
		return config.StrategyCfg{Name: name, Params: map[string]any{}}
	}
	if c, ok := e.strategyConfigs[name]; ok {
		return c
	}
	return config.StrategyCfg{Name: name, Params: map[string]any{}}
}

// syncLoop refreshes the cached positions and day P&L.
//
// It runs on a timer AND on demand. The timer alone left the position book
// stale for up to three seconds after a fill — the one moment an operator is
// looking straight at it. A fill nudges this loop instead of calling the broker
// inline, so the refresh still happens on a single goroutine and a burst of
// fills coalesces into one request rather than one per fill. That matters in
// live mode, where fetching positions is a rate-limited REST call.
func (e *Engine) syncLoop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	e.refreshPositions(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.refreshPositions(ctx)
		case <-e.refreshNow:
			e.refreshPositions(ctx)
		}
	}
}

// requestRefresh asks the sync loop to refresh positions as soon as it can.
// Non-blocking: if a refresh is already queued, this one is redundant.
func (e *Engine) requestRefresh() {
	select {
	case e.refreshNow <- struct{}{}:
	default:
	}
}

// reconcileLoop polls the live order book and emits any newly-filled quantity,
// as a fallback to the order-update stream.
//
// The live-mode check happens on every pass rather than once at startup: the
// broker can be swapped from paper to live at runtime when the operator
// confirms live trading, and a decision made at boot would leave real fills
// unreconciled for the rest of the session.
func (e *Engine) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Poll whichever broker actually holds real orders. Under mixed
			// routing e.broker is still the PAPER broker while manual orders go
			// live, so checking e.broker.Mode() would skip reconciliation
			// entirely and leave real fills unseen.
			br := e.liveBrokerOrNil()
			if br == nil {
				e.cmu.RLock()
				br = e.broker
				e.cmu.RUnlock()
				if br == nil || br.Mode() != "live" {
					continue
				}
			}
			e.reconcileLiveOrders(ctx, br)
		}
	}
}

// refreshPositions fetches current positions, caches them, and persists them so
// the daily-loss check has fresh data.
func (e *Engine) refreshPositions(ctx context.Context) {
	// Both books, tagged. With manual orders live and strategies simulated,
	// positions exist in two brokers at once and neither list is the whole
	// picture. A failure on one book must not discard the other's positions,
	// so each is collected independently.
	var positions []broker.Position
	var failed int
	for _, src := range e.booksInUse() {
		got, err := src.Broker.GetPositions(ctx)
		if err != nil {
			failed++
			if e.logger != nil {
				e.logger.Debug("refresh positions failed", "book", src.Book, "err", err)
			}
			continue
		}
		for i := range got {
			got[i].Book = src.Book
		}
		positions = append(positions, got...)
	}
	if failed > 0 && len(positions) == 0 {
		return
	}
	// Keep the broker's figures as the REALIZED baseline, untouched.
	//
	// Marking to market adds unrealized P&L on top of it, and that happens on
	// every tick. Overwriting PnL in place would make the next mark add
	// unrealized to a number that already contained it, compounding the error
	// on every tick until the reported P&L was nonsense.
	e.mu.Lock()
	e.rawPositions = positions
	e.mu.Unlock()

	e.markPositionsToMarket(true)

	// Persist so storage.GetDayPnL stays aligned. Uses the marked view.
	for _, p := range e.Positions() {
		_ = e.store.UpsertPosition(ctx, &p)
	}
}

// reconcileLiveOrders diffs the order book against seen fills.
func (e *Engine) reconcileLiveOrders(ctx context.Context, br broker.Broker) {
	if br == nil {
		return
	}
	orders, err := br.GetOpenOrders(ctx)
	if err != nil || len(orders) == 0 {
		return
	}
	// Note: open orders only carry pending fills; fully-filled orders leave the
	// book. The order-update stream is the primary fill source; this is a backup.
	e.liveSeenMu.Lock()
	defer e.liveSeenMu.Unlock()
	for _, o := range orders {
		if o.FilledQuantity == 0 {
			continue
		}
		if o.FilledQuantity > e.liveSeen[o.ExchangeOrderID] {
			e.liveSeen[o.ExchangeOrderID] = o.FilledQuantity
		}
	}
}

// persistPaperPositions writes the paper broker's current positions to storage.
func (e *Engine) persistPaperPositions() {
	pb := e.currentPaperBroker()
	if pb == nil {
		return
	}
	ctx := context.Background()
	positions, err := pb.GetPositions(ctx)
	if err != nil {
		return
	}
	e.mu.RLock()
	prices := e.prices
	e.mu.RUnlock()
	for i := range positions {
		// Tag the book before persisting, or a simulated position lands in
		// storage indistinguishable from a real one and every downstream total
		// blends the two.
		positions[i].Book = broker.BookPaper
		if last, ok := prices[positions[i].TradingSymbol]; ok && last > 0 {
			positions[i].LastPrice = last
			positions[i].PnL = positionPnL(positions[i], last)
		}
		_ = e.store.UpsertPosition(ctx, &positions[i])
	}
}

// snapshotDayPnL returns the cached day PnL.
func (e *Engine) snapshotDayPnL() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dayPnL
}

// snapshotBookPnL returns the day's P&L for one book.
func (e *Engine) snapshotBookPnL(b broker.Book) float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if b.IsReal() {
		return e.realPnL
	}
	return e.paperPnL
}

// BookPnL reports the day's P&L for one book, for the UI.
func (e *Engine) BookPnL(b broker.Book) float64 { return e.snapshotBookPnL(b) }

// snapshotBookPositionCount counts open positions in one book, so the
// max-open-positions limit is applied per book rather than letting simulated
// positions consume the allowance that guards real exposure.
func (e *Engine) snapshotBookPositionCount(b broker.Book) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	n := 0
	for _, p := range e.positions {
		if p.NetQuantity != 0 && p.Book.IsReal() == b.IsReal() {
			n++
		}
	}
	return n
}

// hasPosition reports whether there's an open (non-flat) position in symbol.
func (e *Engine) hasPosition(symbol string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, p := range e.positions {
		if p.TradingSymbol == symbol && p.NetQuantity != 0 {
			return true
		}
	}
	return false
}

// orderKey returns the id used to key orderStrategy for a given order.
func (e *Engine) orderKey(o *broker.Order) string {
	if o.ExchangeOrderID != "" {
		return o.ExchangeOrderID
	}
	return o.ID
}

// positionPnL computes total PnL (realized + unrealized) for a position at last.
func positionPnL(p broker.Position, last float64) float64 {
	if p.NetQuantity == 0 {
		return p.PnL // realized only
	}
	sign := 1.0
	if p.NetQuantity < 0 {
		sign = -1.0
	}
	return p.PnL + sign*(last-p.AveragePrice)*float64(abs(p.NetQuantity))
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// toStr stringifies a sync.Map Load result (interface{}).
func toStr(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}
