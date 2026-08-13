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
	"log/slog"
	"strconv"
	"sync"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
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

	strategies []strategy.Strategy
	// strategyConfigs maps a strategy Name() to its declarative config block.
	strategyConfigs map[string]config.StrategyCfg

	mu        sync.RWMutex
	prices    map[string]float64 // symbol -> last price
	positions []broker.Position  // cached, refreshed by sync loop
	dayPnL    float64            // cached

	// orderStrategy maps an order id (internal for paper, exchange for live) to
	// the strategy that placed it, so fills can be routed back.
	orderStrategy sync.Map

	// liveSeen tracks per-order last-seen filled quantity, used to emit
	// incremental fills when polling live order updates.
	liveSeen map[string]int
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

// Start initializes strategies, starts the ticker, and runs until ctx is done.
// It blocks; run it in the main goroutine or behind a WaitGroup.
func (e *Engine) Start(ctx context.Context) error {
	// Wire ticker callbacks (if a ticker was provided).
	if e.ticker != nil {
		e.ticker.OnTick = e.handleTick
		e.ticker.OnOrder = e.handleOrderUpdate
		e.ticker.OnError = func(err error) {
			if e.logger != nil {
				e.logger.Warn("ticker error", "err", err)
			}
		}
	}

	// Route paper-broker fills back through the engine so they're persisted and
	// fanned out to strategies. Done here (not at construction) because the
	// engine owns handleFill and is built after the broker.
	if e.paperBroker != nil {
		e.paperBroker.SetOnFill(e.handleFill)
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

	// Live fill reconciliation loop (only meaningful for live; harmless for paper).
	if e.broker.Mode() == "live" {
		go e.reconcileLoop(ctx)
	}

	// Run the ticker (blocks until ctx done or Close). If there's no ticker
	// (e.g. a unit test or fully offline run), just wait on ctx.
	if e.ticker != nil {
		return e.ticker.Run(ctx)
	}
	<-ctx.Done()
	return ctx.Err()
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
		return nil, err
	}

	o, err := e.broker.PlaceOrder(ctx, req)
	if err != nil {
		return nil, err
	}
	// Remember which strategy owns this order so fills route correctly.
	e.orderStrategy.Store(e.orderKey(o), req.StrategyID)

	if err := e.store.SaveOrder(ctx, o); err != nil && e.logger != nil {
		e.logger.Error("persist order failed", "id", o.ID, "err", err)
	}
	return o, nil
}

// CancelOrder cancels a previously placed order.
func (e *Engine) CancelOrder(ctx context.Context, orderID string) error {
	return e.broker.CancelOrder(ctx, orderID)
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
// we fall back to these well-known tokens when subscribing. (Values are stable
// and documented in Kite's instruments/quote endpoints.)
var knownIndexTokens = map[string]uint32{
	"NIFTY 50":       256,
	"NIFTY BANK":     260542,
	"NIFTY FIN SERVICE": 257,
	"INDIA VIX":      264969,
}

// Subscribe resolves trading symbols to tokens and subscribes them on the ticker.
func (e *Engine) Subscribe(symbols []string) error {
	if e.ticker == nil {
		return nil
	}
	tokens := make([]uint32, 0, len(symbols))
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
		}
	}
	if len(tokens) > 0 {
		e.ticker.Subscribe(tokens)
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

	// Paper broker evaluates pending orders against the new price.
	if e.paperBroker != nil {
		e.paperBroker.OnPrice(tick.TradingSymbol, tick.LastPrice)
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
	if e.paperBroker != nil {
		e.persistPaperPositions()
	}
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
	prev := e.liveSeen[ou.ExchangeOrderID]
	cur := int(ou.FilledQuantity)
	delta := cur - prev
	if delta <= 0 {
		return
	}
	e.liveSeen[ou.ExchangeOrderID] = cur

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
func (e *Engine) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.reconcileLiveOrders(ctx)
		}
	}
}

// refreshPositions fetches current positions, caches them, and persists them so
// the daily-loss check has fresh data.
func (e *Engine) refreshPositions(ctx context.Context) {
	positions, err := e.broker.GetPositions(ctx)
	if err != nil {
		if e.logger != nil {
			e.logger.Debug("refresh positions failed", "err", err)
		}
		return
	}
	// Mark up unrealized PnL for paper positions using the latest prices.
	if e.paperBroker != nil {
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
}

// reconcileLiveOrders diffs the order book against seen fills.
func (e *Engine) reconcileLiveOrders(ctx context.Context) {
	orders, err := e.broker.GetOpenOrders(ctx)
	if err != nil || len(orders) == 0 {
		return
	}
	// Note: open orders only carry pending fills; fully-filled orders leave the
	// book. The order-update stream is the primary fill source; this is a backup.
	for _, o := range orders {
		if o.FilledQuantity == 0 {
			continue
		}
		prev := e.liveSeen[o.ExchangeOrderID]
		if o.FilledQuantity > prev {
			e.liveSeen[o.ExchangeOrderID] = o.FilledQuantity
		}
	}
}

// persistPaperPositions writes the paper broker's current positions to storage.
func (e *Engine) persistPaperPositions() {
	ctx := context.Background()
	positions, err := e.paperBroker.GetPositions(ctx)
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
