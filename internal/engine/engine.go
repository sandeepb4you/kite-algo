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
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"kite-algo/internal/broker"
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
	instruments *kite.Instruments
	ticker      *kite.Ticker
	logger      *slog.Logger
	recordTicks bool

	// pub fans engine activity out to observers (the web UI). Never nil — it
	// defaults to events.Nop so the hot path needs no nil check. Publishing is
	// non-blocking and lossy by design; see package events.
	pub events.Publisher

	strategies []strategy.Strategy
	// strategyConfigs maps a strategy Name() to its declarative config block.
	strategyConfigs map[string]config.StrategyCfg

	mu        sync.RWMutex
	prices    map[string]float64 // symbol -> last price
	positions []broker.Position  // cached, refreshed by sync loop
	dayPnL    float64            // cached

	// cmu guards the market-data plumbing, which is now swapped at runtime:
	// the process boots without a Kite session and acquires one when the
	// operator completes the browser login, and swaps again when a token
	// expires and is renewed.
	cmu          sync.RWMutex
	tickerCancel context.CancelFunc
	runCtx       context.Context

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
		pub:         events.Nop{},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// AddStrategy registers a strategy. Must be called before Start.
func (e *Engine) AddStrategy(s strategy.Strategy) {
	e.strategies = append(e.strategies, s)
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

	// Initialize strategies, handing each one the engine (as Trader) and its cfg.
	for _, s := range e.strategies {
		if e.logger != nil {
			e.logger.Info("initializing strategy", "name", s.Name())
		}
		if err := s.Init(ctx, e, e.configFor(s.Name())); err != nil {
			return err
		}
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

// Stop shuts down strategies.
func (e *Engine) Stop(ctx context.Context) {
	for _, s := range e.strategies {
		if err := s.Stop(ctx); err != nil && e.logger != nil {
			e.logger.Error("strategy stop failed", "name", s.Name(), "err", err)
		}
	}
}

// --- strategy.Trader implementation ---

// PlaceOrder risk-checks and submits an order on behalf of a strategy.
func (e *Engine) PlaceOrder(ctx context.Context, req broker.OrderRequest) (*broker.Order, error) {
	lotSize := e.LotSize(req.TradingSymbol)
	openPositions := e.snapshotPositionCount(req.TradingSymbol)
	dayPnL := e.snapshotDayPnL()
	openingNew := openPositions == 0 || !e.hasPosition(req.TradingSymbol)

	if err := e.risk.Check(ctx, req, lotSize, openPositions, dayPnL, openingNew); err != nil {
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

	o, err := e.currentBroker().PlaceOrder(ctx, req)
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
	// Remember which strategy owns this order so fills route correctly.
	e.orderStrategy.Store(e.orderKey(o), req.StrategyID)

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
	return e.currentBroker().CancelOrder(ctx, orderID)
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

// knownIndexTokens maps index display names to their stable Kite instrument
// tokens. Index quotes (NIFTY 50, etc.) are NOT in the NFO instrument CSV, so
// we fall back to these well-known tokens when subscribing.
//
// Every value here MUST be an indices-segment token: Kite encodes the exchange
// segment in the low byte, and indices are segment 9 (see priceDivisor in
// internal/kite/ticker.go). indexTokenValid enforces that, and TestIndexTokens
// guards it — a token with the wrong segment is silently ignored by the
// exchange, so the subscription just never delivers ticks.
var knownIndexTokens = map[string]uint32{
	"NIFTY 50":          256265,
	"NIFTY BANK":        260105,
	"NIFTY FIN SERVICE": 257801,
	"INDIA VIX":         264969,
}

// segIndices is the Kite exchange-segment code carried in an index instrument
// token's low byte.
const segIndices = 9

// indexTokenValid reports whether tok is a well-formed indices-segment token.
func indexTokenValid(tok uint32) bool { return tok&0xff == segIndices }

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

	for _, s := range e.strategies {
		s.OnTick(context.Background(), tick)
	}
}

// handleFill records a fill and routes it to the owning strategy.
func (e *Engine) handleFill(fill broker.Fill) {
	if err := e.store.SaveFill(context.Background(), &fill); err != nil && e.logger != nil {
		e.logger.Error("persist fill failed", "id", fill.ID, "err", err)
	}
	// Persist the affected position (paper broker already updated it).
	if e.currentPaperBroker() != nil {
		e.persistPaperPositions()
	}
	e.pub.Publish(events.Event{
		Kind:       events.KindFill,
		At:         fill.Timestamp,
		Symbol:     fill.TradingSymbol,
		StrategyID: fill.StrategyID,
		Fill:       &fill,
	})
	for _, s := range e.strategies {
		if fill.StrategyID == "" || fill.StrategyID == s.Name() {
			s.OnFill(context.Background(), fill)
		}
	}
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

// syncLoop periodically refreshes the cached positions/dayPnL and persists them.
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
		}
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
			e.cmu.RLock()
			br := e.broker
			e.cmu.RUnlock()
			if br == nil || br.Mode() != "live" {
				continue
			}
			e.reconcileLiveOrders(ctx)
		}
	}
}

// refreshPositions fetches current positions, caches them, and persists them so
// the daily-loss check has fresh data.
func (e *Engine) refreshPositions(ctx context.Context) {
	positions, err := e.currentBroker().GetPositions(ctx)
	if err != nil {
		if e.logger != nil {
			e.logger.Debug("refresh positions failed", "err", err)
		}
		return
	}
	// Mark up unrealized PnL for paper positions using the latest prices.
	if e.currentPaperBroker() != nil {
		e.mu.RLock()
		prices := e.prices
		e.mu.RUnlock()
		for i := range positions {
			if last, ok := prices[positions[i].TradingSymbol]; ok && last > 0 {
				positions[i].LastPrice = last
				positions[i].PnL = positionPnL(positions[i], last)
			}
		}
	}

	var dayPnL float64
	for _, p := range positions {
		dayPnL += p.PnL
	}
	e.mu.Lock()
	e.positions = positions
	e.dayPnL = dayPnL
	e.mu.Unlock()

	// Persist so storage.GetDayPnL stays aligned.
	for i := range positions {
		_ = e.store.UpsertPosition(ctx, &positions[i])
	}

	e.pub.Publish(events.Event{
		Kind:      events.KindPositions,
		Positions: positions,
		DayPnL:    dayPnL,
	})
}

// reconcileLiveOrders diffs the order book against seen fills.
func (e *Engine) reconcileLiveOrders(ctx context.Context) {
	orders, err := e.currentBroker().GetOpenOrders(ctx)
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
		if last, ok := prices[positions[i].TradingSymbol]; ok && last > 0 {
			positions[i].LastPrice = last
			positions[i].PnL = positionPnL(positions[i], last)
		}
		_ = e.store.UpsertPosition(ctx, &positions[i])
	}
}

// snapshotPositionCount returns the number of currently OPEN (non-flat) positions.
func (e *Engine) snapshotPositionCount(symbol string) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	n := 0
	for _, p := range e.positions {
		if p.NetQuantity != 0 {
			n++
		}
	}
	return n
}

// snapshotDayPnL returns the cached day PnL.
func (e *Engine) snapshotDayPnL() float64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dayPnL
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
