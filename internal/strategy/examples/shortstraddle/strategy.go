// Package shortstraddle is a reference strategy. It sells 1 lot each of the
// ATM call and put (a short straddle), then squares off when the position's
// net delta drifts past a threshold OR near the close.
//
// It demonstrates the full strategy lifecycle: reading config, subscribing to
// instruments, computing Greeks from live premiums, and placing orders through
// the Trader (which risk-checks and persists them).
//
// This is a TEACHING EXAMPLE, not a money-maker. Short straddles have
// unlimited risk; run it in paper mode first and never size beyond what you
// can afford to lose.
package shortstraddle

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/options"
	"kite-algo/internal/strategy"
)

// Strategy implements strategy.Strategy for the delta-managed short straddle.
type Strategy struct {
	name   string
	trader strategy.Trader
	logger *slog.Logger

	// Config (read from the YAML strategy params block).
	indexSymbol     string  // spot index to track, e.g. "NIFTY 50"
	underlying      string  // option underlying, e.g. "NIFTY"
	strikeStep      float64 // strike grid, e.g. 50 (NIFTY), 100 (BANKNIFTY)
	lots            int
	exitDelta       float64 // square off when |net delta| exceeds this (per unit)
	riskFreeRate    float64 // annualized rate for Black-Scholes
	entryStartClock string  // "09:20" IST — no entry before this
	squareOffClock  string  // "15:15" IST — flat by this time
	product         broker.ProductType

	mu      sync.Mutex
	armed   bool           // options subscribed?
	entered bool           // short straddle opened today?
	exited  bool           // squared off today (no re-entry)?
	legs    map[string]leg // symbol -> leg metadata
	spot    float64        // last underlying spot

	// baseQty is the quantity of one leg at entry (lots × lot size). Net delta is
	// summed quantity-weighted and then divided by this, so exit_delta stays the
	// per-unit number the descriptor declares (range 0.01–2) and the exit trigger
	// does not move when the operator changes `lots`.
	baseQty int

	// session is the IST trading date the entered/exited flags belong to.
	// Without it those flags are set once and never cleared, so the strategy
	// trades exactly once for the lifetime of the process. That was invisible
	// when the platform was a CLI restarted every morning; as a long-running
	// server it would mean the strategy silently stops trading after day one.
	session string
}

// leg records what we did with one option symbol.
type leg struct {
	symbol string
	side   broker.Side // the side we SOLD (always SELL for entry; BUY for exit)
	strike float64
	typ    options.OptionType
	qty    int
	open   bool // still short?
}

// New constructs the strategy with a display name and logger.
func New(name string, logger *slog.Logger) *Strategy {
	return &Strategy{name: name, logger: logger}
}

// Name satisfies strategy.Strategy.
func (s *Strategy) Name() string { return s.name }

// Init reads its config block and subscribes to the spot index.
func (s *Strategy) Init(ctx context.Context, trader strategy.Trader, cfg config.StrategyCfg) error {
	s.trader = trader
	s.legs = make(map[string]leg)

	// Parameters arrive already defaulted, type-coerced, and range-checked by
	// the registry (see register.go). Applying fallbacks a second time here
	// would create two sets of defaults that can silently drift apart.
	//
	// The registry defaults are still applied defensively for callers that
	// construct a config by hand, such as tests.
	params, err := Descriptor().Normalize(cfg.Params)
	if err != nil {
		return err
	}
	get := config.StrategyCfg{Params: params}

	s.indexSymbol = get.ParamString("index_symbol")
	s.underlying = get.ParamString("underlying")
	s.strikeStep = get.ParamFloat("strike_step")
	s.lots = get.ParamInt("lots")
	s.exitDelta = get.ParamFloat("exit_delta")
	s.riskFreeRate = get.ParamFloat("risk_free_rate")
	s.entryStartClock = get.ParamString("entry_start_time")
	s.squareOffClock = get.ParamString("square_off_time")
	s.product = broker.ProductType(get.ParamString("product"))

	// An entry window that closes before it opens would leave the strategy
	// running all day and never trading, with nothing in the logs to say why.
	// Caught at Init so it surfaces when the operator clicks start, not at
	// 15:15 when they wonder where the position went.
	if !clockBefore(s.entryStartClock, s.squareOffClock) {
		return fmt.Errorf(
			"entry_start_time %s is not before square_off_time %s, so the strategy could never enter",
			s.entryStartClock, s.squareOffClock)
	}

	// Subscribe to the spot index so OnTick drives entry.
	if err := trader.Subscribe([]string{s.indexSymbol}); err != nil {
		return err
	}
	if s.logger != nil {
		s.logger.Info("short-straddle initialized",
			"index", s.indexSymbol, "underlying", s.underlying,
			"lots", s.lots, "exit_delta", s.exitDelta,
			"entry_start", s.entryStartClock, "square_off", s.squareOffClock)
	}
	return nil
}

// OnTick drives entry (on the spot tick) and exit monitoring (on option ticks).
func (s *Strategy) OnTick(ctx context.Context, tick marketdata.Tick) {
	if tick.TradingSymbol == "" {
		return
	}
	s.rollSession(s.trader.Now())

	// Spot index tick → refresh spot, maybe enter.
	if tick.TradingSymbol == s.indexSymbol {
		s.mu.Lock()
		s.spot = tick.LastPrice
		s.mu.Unlock()
		s.maybeEnter(ctx, tick.LastPrice)
	}

	// Every tick — spot included — manages an open position. Keying exit
	// management off option ticks alone meant an illiquid leg that stopped
	// printing could hold the position past both the delta limit and the
	// square-off clock, precisely when the underlying was moving fastest. The
	// index prints continuously, so it is the reliable heartbeat.
	if s.hasOpenLeg() {
		s.maybeExit(ctx)
	}
}

// hasOpenLeg reports whether any leg is still short.
func (s *Strategy) hasOpenLeg() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.legs {
		if l.open {
			return true
		}
	}
	return false
}

// OnFill logs fills so you can watch the strategy trade.
func (s *Strategy) OnFill(ctx context.Context, fill broker.Fill) {
	if s.logger != nil {
		s.logger.Info("fill",
			"strategy", s.name, "symbol", fill.TradingSymbol,
			"side", fill.Side, "qty", fill.Quantity, "price", fill.Price)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.legs[fill.TradingSymbol]; ok && fill.Side == broker.SideBuy {
		// A buy fill closes the short leg.
		l.open = false
		s.legs[fill.TradingSymbol] = l
	}
}

// Stop releases the strategy. It deliberately does NOT trade: whether an
// outgoing strategy's positions are squared off is the operator's decision,
// taken per-stop in the UI, and the engine carries it out via SquareOff.
func (s *Strategy) Stop(ctx context.Context) error { return nil }

// SquareOff buys back every open leg, satisfying strategy.Flattener. The engine
// calls this when the operator stops the strategy with square-off requested, or
// at shutdown.
func (s *Strategy) SquareOff(ctx context.Context, reason string) error {
	s.squareOff(ctx, reason)
	return nil
}

// maybeEnter opens the short straddle once, at the ATM strike for the nearest
// expiry, using MARKET sell orders for both legs. It does nothing once the
// square-off clock has passed.
func (s *Strategy) maybeEnter(ctx context.Context, spot float64) {
	s.mu.Lock()
	if s.entered || s.exited || spot <= 0 || s.trader == nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	now := s.trader.Now()

	// Hold off until the entry window opens. The first minutes after the open
	// carry the widest spreads of the day and the underlying is still finding a
	// level, so an ATM straddle sold at 09:15 is frequently not the straddle
	// that was intended by 09:20 — the spot has moved a strike and both legs
	// were crossed at their worst price of the session.
	if !atOrAfter(s.entryStartClock, now) {
		return
	}

	// Never open a position the exit path would close on this very tick. A first
	// tick arriving after the square-off clock — a late start, a reconnect, a
	// backtest whose window opens mid-afternoon — would otherwise sell the
	// straddle and buy it straight back, paying both spreads for no exposure.
	if pastSquareOff(s.squareOffClock, now) {
		return
	}

	chain := s.trader.Options(s.underlying, time.Time{})
	if len(chain) == 0 {
		// No instrument master yet (e.g. dry-run). Nothing to do.
		return
	}
	atm := roundTo(spot, s.strikeStep)
	var ce, pe strategy.Instrument
	for _, ins := range chain {
		if ins.Strike != atm {
			continue
		}
		switch ins.InstrumentType {
		case "CE":
			ce = ins
		case "PE":
			pe = ins
		}
	}
	if ce.TradingSymbol == "" || pe.TradingSymbol == "" {
		if s.logger != nil {
			s.logger.Warn("ATM CE/PE not found", "underlying", s.underlying, "strike", atm)
		}
		return
	}

	lotSize := ce.LotSize
	if lotSize == 0 {
		lotSize = pe.LotSize
	}
	qty := s.lots * lotSize

	// Subscribe to the legs so we get ticks to manage the position.
	_ = s.trader.Subscribe([]string{ce.TradingSymbol, pe.TradingSymbol})

	s.mu.Lock()
	if s.entered { // double-check under lock
		s.mu.Unlock()
		return
	}
	s.entered = true
	s.baseQty = qty
	s.legs[ce.TradingSymbol] = leg{symbol: ce.TradingSymbol, side: broker.SideSell, strike: atm, typ: options.Call, qty: qty, open: true}
	s.legs[pe.TradingSymbol] = leg{symbol: pe.TradingSymbol, side: broker.SideSell, strike: atm, typ: options.Put, qty: qty, open: true}
	s.mu.Unlock()

	if s.logger != nil {
		s.logger.Info("ENTER short straddle", "spot", spot, "strike", atm,
			"ce", ce.TradingSymbol, "pe", pe.TradingSymbol, "qty", qty)
	}
	s.trader.Signal(strategy.Signal{
		StrategyID: s.name,
		Kind:       "enter",
		Message: fmt.Sprintf("Sold %d× straddle at %.0f strike (spot %.2f)",
			s.lots, atm, spot),
		Data: map[string]any{
			"spot": spot, "strike": atm, "qty": qty,
			"ce": ce.TradingSymbol, "pe": pe.TradingSymbol,
		},
	})
	// Sell both legs at market.
	s.sell(ctx, ce.TradingSymbol, qty)
	s.sell(ctx, pe.TradingSymbol, qty)
}

// Resume rebuilds the session state from legs this instance already holds,
// so a restart picks up managing an open straddle instead of selling another.
//
// Everything reconstructed here is what maybeEnter would have set: the legs
// map, entered, baseQty, and a subscription to each leg. What it deliberately
// does NOT reconstruct is the entry price or the original ATM strike — nothing
// downstream needs them. Exits key off net delta and the square-off clock, both
// computed from live quotes and the strike stored per leg.
//
// The session date is stamped to today so rollSession does not immediately
// treat these as stale. That is safe because rollSession already refuses to
// roll while any leg is open; the pair means a straddle carried across
// midnight stays managed rather than being forgotten at 00:00.
func (s *Strategy) Resume(ctx context.Context, positions []broker.Position) error {
	symbols := make([]string, 0, len(positions))

	s.mu.Lock()
	if s.legs == nil {
		s.legs = make(map[string]leg)
	}
	for _, p := range positions {
		if !p.IsOpen() {
			continue
		}

		// Only SHORT legs are ours to manage. This strategy is only ever short
		// its options, so a long position under this StrategyID is something
		// else — a partial manual unwind, most likely — and adopting it would
		// have the delta calculation working from a book that never existed.
		if p.NetQuantity > 0 {
			if s.logger != nil {
				s.logger.Warn("ignoring long position on resume",
					"symbol", p.TradingSymbol, "qty", p.NetQuantity, "strategy", s.name)
			}
			continue
		}
		qty := -p.NetQuantity

		strike, typ, ok := s.legShape(p.TradingSymbol)
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("cannot resolve strike/type for open position %s", p.TradingSymbol)
		}

		s.legs[p.TradingSymbol] = leg{
			symbol: p.TradingSymbol, side: broker.SideSell,
			strike: strike, typ: typ, qty: qty, open: true,
		}
		if qty > s.baseQty {
			s.baseQty = qty
		}
		symbols = append(symbols, p.TradingSymbol)
	}

	restored := len(symbols)
	if restored > 0 {
		s.entered = true
		s.exited = false
		s.session = s.trader.Now().In(ist).Format("2006-01-02")
	}
	s.mu.Unlock()

	if restored == 0 {
		return nil
	}

	// Without ticks on the legs there is no delta and no exit — a resumed
	// strategy that cannot see its own position is worse than one that never
	// started, because the UI would show it running.
	if err := s.trader.Subscribe(symbols); err != nil {
		return fmt.Errorf("subscribe to resumed legs: %w", err)
	}

	if s.logger != nil {
		s.logger.Info("resumed with open legs",
			"strategy", s.name, "legs", restored, "base_qty", s.baseQty)
	}
	s.trader.Signal(strategy.Signal{
		Kind: "resume", Level: "warn",
		Message: fmt.Sprintf("Resumed after restart with %d open leg(s); managing, not re-entering.", restored),
		Data:    map[string]any{"legs": restored},
	})
	return nil
}

// legShape resolves a symbol's strike and option type, preferring the
// instrument master and falling back to parsing the symbol.
//
// The master is authoritative — NSE has changed its symbol format several
// times — but it is not always loaded when a resume runs, and refusing to
// resume a live position because the chain has not downloaded yet would strand
// the position for the sake of tidiness.
func (s *Strategy) legShape(symbol string) (float64, options.OptionType, bool) {
	if s.trader != nil {
		if ins, ok := s.trader.Lookup(symbol); ok && ins.Strike > 0 {
			switch ins.InstrumentType {
			case "CE":
				return ins.Strike, options.Call, true
			case "PE":
				return ins.Strike, options.Put, true
			}
		}
	}
	if spec, ok := options.ParseSymbol(symbol); ok && spec.Strike > 0 {
		return spec.Strike, spec.Type, true
	}
	return 0, 0, false
}

// rollSession resets the once-per-day entry flags when the IST trading date
// changes, so the strategy trades again the next morning.
func (s *Strategy) rollSession(now time.Time) {
	today := now.In(ist).Format("2006-01-02")

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == today {
		return
	}
	// Carrying legs across a rollover would mean losing track of a live
	// position, so only reset once the book is clear.
	for _, l := range s.legs {
		if l.open {
			return
		}
	}
	if s.session != "" && s.logger != nil {
		s.logger.Info("new trading session; re-arming", "date", today, "strategy", s.name)
	}
	s.session = today
	s.entered = false
	s.exited = false
	s.baseQty = 0
	s.legs = make(map[string]leg)
}

// maybeExit squares off if delta drifts past exitDelta or the square-off clock hits.
func (s *Strategy) maybeExit(ctx context.Context) {
	s.mu.Lock()
	if s.exited || !s.entered || s.spot <= 0 {
		s.mu.Unlock()
		return
	}
	legsCopy := make(map[string]leg, len(s.legs))
	for k, v := range s.legs {
		legsCopy[k] = v
	}
	spot := s.spot
	baseQty := s.baseQty
	s.mu.Unlock()

	// All time comes from the trader, never time.Now(): in a backtest replaying
	// past data the real clock would put the square-off window and every greek
	// on the wrong day.
	now := s.trader.Now()

	// Time-based square-off.
	if pastSquareOff(s.squareOffClock, now) {
		s.squareOff(ctx, "square-off time")
		return
	}

	// Delta-based square-off: compute the net delta of the short straddle,
	// weighted by each leg's quantity. Summing raw per-unit deltas only happens
	// to be right while every leg is the same size; weighting by qty keeps the
	// number a true position delta once the legs diverge (a partial cover, or a
	// leg closed on its own).
	netDelta := 0.0
	openQty := 0
	for _, l := range legsCopy {
		if !l.open {
			continue
		}
		ins, ok := s.trader.Lookup(l.symbol)
		if !ok {
			return // see below: a partial sum is worse than no sum
		}
		premium := s.trader.LTP(l.symbol)
		if premium <= 0 {
			return
		}
		t := options.YearsToExpiry(now, ins.Expiry)
		iv, err := options.ImpliedVol(premium, spot, l.strike, t, s.riskFreeRate, l.typ)
		if err != nil {
			return
		}
		g := options.BlackScholes(spot, l.strike, t, iv, s.riskFreeRate, l.typ)
		// We are SHORT the option, so the position delta is the negative of the
		// option's delta.
		netDelta += -g.Delta * float64(l.qty)
		openQty += l.qty
	}
	// Any leg we could not price aborts the check rather than contributing zero.
	// Half a straddle looks like a runaway delta — an unquoted put would have
	// squared the position off on the call's delta alone.
	if openQty == 0 {
		return
	}

	// Normalize back to per-unit, so exit_delta means the same thing at 1 lot and
	// at 10 and stays inside the 0.01–2 range the descriptor validates against.
	if baseQty <= 0 {
		baseQty = maxLegQty(legsCopy) // legs restored without an entry this session
	}
	if baseQty <= 0 {
		return
	}
	netDelta /= float64(baseQty)

	if math.Abs(netDelta) > s.exitDelta {
		s.squareOff(ctx, "delta limit")
	}
}

// maxLegQty returns the largest open-leg quantity, the fallback denominator when
// the entry quantity is unknown.
func maxLegQty(legs map[string]leg) int {
	most := 0
	for _, l := range legs {
		if l.open && l.qty > most {
			most = l.qty
		}
	}
	return most
}

// squareOff buys back every open leg (once).
func (s *Strategy) squareOff(ctx context.Context, reason string) {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return
	}
	s.exited = true
	legsCopy := make([]leg, 0, len(s.legs))
	for _, l := range s.legs {
		if l.open {
			legsCopy = append(legsCopy, l)
		}
	}
	s.mu.Unlock()

	if s.logger != nil {
		s.logger.Info("EXIT short straddle", "reason", reason, "legs", len(legsCopy))
	}
	if s.trader != nil {
		s.trader.Signal(strategy.Signal{
			StrategyID: s.name,
			Kind:       "exit",
			Level:      "warn",
			Message:    fmt.Sprintf("Squaring off %d leg(s): %s", len(legsCopy), reason),
			Data:       map[string]any{"reason": reason, "legs": len(legsCopy)},
		})
	}
	for _, l := range legsCopy {
		s.buy(ctx, l.symbol, l.qty)
	}
}

// sell places a market sell order.
func (s *Strategy) sell(ctx context.Context, symbol string, qty int) {
	s.place(ctx, broker.OrderRequest{
		StrategyID:    s.name,
		Exchange:      s.exchangeFor(symbol),
		TradingSymbol: symbol,
		Product:       s.product,
		OrderType:     broker.OrderTypeMarket,
		Side:          broker.SideSell,
		Quantity:      qty,
		Validity:      broker.ValidityDay,
		Tag:           "short-straddle/entry",
	})
}

// buy places a market buy order (to close a short leg).
func (s *Strategy) buy(ctx context.Context, symbol string, qty int) {
	s.place(ctx, broker.OrderRequest{
		StrategyID:    s.name,
		Exchange:      s.exchangeFor(symbol),
		TradingSymbol: symbol,
		Product:       s.product,
		OrderType:     broker.OrderTypeMarket,
		Side:          broker.SideBuy,
		Quantity:      qty,
		Validity:      broker.ValidityDay,
		Tag:           "short-straddle/exit",
	})
}

// place submits an order and logs the outcome.
func (s *Strategy) place(ctx context.Context, req broker.OrderRequest) {
	o, err := s.trader.PlaceOrder(ctx, req)
	if s.logger != nil {
		if err != nil {
			s.logger.Warn("order failed", "symbol", req.TradingSymbol, "side", req.Side, "err", err)
		} else {
			s.logger.Info("order placed", "symbol", o.TradingSymbol, "side", o.Side,
				"qty", o.Quantity, "id", o.ID, "status", o.Status)
		}
	}
}

// exchangeFor resolves the exchange for a symbol via the instrument master.
func (s *Strategy) exchangeFor(symbol string) string {
	if ins, ok := s.trader.Lookup(symbol); ok {
		return ins.Exchange
	}
	return "NFO"
}

// roundTo rounds spot to the nearest strike step.
func roundTo(spot, step float64) float64 {
	return math.Round(spot/step) * step
}

// ist is the exchange timezone. Declared once rather than rebuilt per call.
var ist = time.FixedZone("IST", 5*3600+30*60)

// pastSquareOff reports whether `now` is past the square-off clock (e.g.
// "15:15") in IST. Returns false if the clock string is unparseable.
//
// It takes `now` as a parameter rather than reading the clock itself, so the
// same code is correct under live trading and under a backtest's simulated time.
func pastSquareOff(clock string, now time.Time) bool {
	target, ok := clockOn(clock, now)
	if !ok {
		return false
	}
	return now.In(ist).After(target)
}

// atOrAfter reports whether `now` has reached the clock time in IST.
//
// Returns TRUE for an unparseable clock, the opposite of pastSquareOff. Each
// defaults to the harmless answer for its own question: an unreadable
// square-off time must not trigger a surprise exit, and an unreadable entry
// time must not silently prevent the strategy from ever trading.
func atOrAfter(clock string, now time.Time) bool {
	target, ok := clockOn(clock, now)
	if !ok {
		return true
	}
	return !now.In(ist).Before(target)
}

// clockOn resolves "HH:MM" against the IST calendar day of `now`.
func clockOn(clock string, now time.Time) (time.Time, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(clock))
	if err != nil {
		return time.Time{}, false
	}
	local := now.In(ist)
	return time.Date(local.Year(), local.Month(), local.Day(),
		t.Hour(), t.Minute(), 0, 0, ist), true
}

// clockBefore reports whether clock a is strictly earlier in the day than b.
// An unparseable clock is treated as ordered, so validation rejects only a
// genuine inversion rather than a typo the parser already handles elsewhere.
func clockBefore(a, b string) bool {
	ta, err1 := time.Parse("15:04", strings.TrimSpace(a))
	tb, err2 := time.Parse("15:04", strings.TrimSpace(b))
	if err1 != nil || err2 != nil {
		return true
	}
	return ta.Before(tb)
}
