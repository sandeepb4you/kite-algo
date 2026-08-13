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
	"log/slog"
	"math"
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
	indexSymbol    string  // spot index to track, e.g. "NIFTY 50"
	underlying     string  // option underlying, e.g. "NIFTY"
	strikeStep     float64 // strike grid, e.g. 50 (NIFTY), 100 (BANKNIFTY)
	lots           int
	exitDelta      float64 // square off when |net delta| exceeds this (per unit)
	riskFreeRate   float64 // annualized rate for Black-Scholes
	squareOffClock string  // "15:15" IST — flat by this time
	product        broker.ProductType

	mu      sync.Mutex
	armed   bool                 // options subscribed?
	entered bool                 // short straddle opened today?
	exited  bool                 // squared off today (no re-entry)?
	legs    map[string]leg       // symbol -> leg metadata
	spot    float64              // last underlying spot
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

	// Defaults.
	s.indexSymbol = cfg.ParamString("index_symbol")
	if s.indexSymbol == "" {
		s.indexSymbol = "NIFTY 50"
	}
	s.underlying = cfg.ParamString("underlying")
	if s.underlying == "" {
		s.underlying = "NIFTY"
	}
	s.strikeStep = cfg.ParamFloat("strike_step")
	if s.strikeStep == 0 {
		s.strikeStep = 50
	}
	s.lots = cfg.ParamInt("lots")
	if s.lots == 0 {
		s.lots = 1
	}
	s.exitDelta = cfg.ParamFloat("exit_delta")
	if s.exitDelta == 0 {
		s.exitDelta = 0.25
	}
	s.riskFreeRate = cfg.ParamFloat("risk_free_rate")
	if s.riskFreeRate == 0 {
		s.riskFreeRate = 0.06
	}
	s.squareOffClock = cfg.ParamString("square_off_time")
	if s.squareOffClock == "" {
		s.squareOffClock = "15:15"
	}
	prod := cfg.ParamString("product")
	if prod == "" {
		prod = "MIS"
	}
	s.product = broker.ProductType(prod)

	// Subscribe to the spot index so OnTick drives entry.
	if err := trader.Subscribe([]string{s.indexSymbol}); err != nil {
		return err
	}
	if s.logger != nil {
		s.logger.Info("short-straddle initialized",
			"index", s.indexSymbol, "underlying", s.underlying,
			"lots", s.lots, "exit_delta", s.exitDelta, "square_off", s.squareOffClock)
	}
	return nil
}

// OnTick drives entry (on the spot tick) and exit monitoring (on option ticks).
func (s *Strategy) OnTick(ctx context.Context, tick marketdata.Tick) {
	if tick.TradingSymbol == "" {
		return
	}
	// Spot index tick → maybe enter.
	if tick.TradingSymbol == s.indexSymbol {
		s.mu.Lock()
		s.spot = tick.LastPrice
		s.mu.Unlock()
		s.maybeEnter(ctx, tick.LastPrice)
		return
	}
	// Option tick → manage an open position.
	s.mu.Lock()
	hasOpen := false
	for _, l := range s.legs {
		if l.open {
			hasOpen = true
			break
		}
	}
	s.mu.Unlock()
	if hasOpen {
		s.maybeExit(ctx)
	}
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

// Stop squares off any open position on shutdown.
func (s *Strategy) Stop(ctx context.Context) error {
	s.squareOff(ctx, "shutdown")
	return nil
}

// maybeEnter opens the short straddle once, at the ATM strike for the nearest
// expiry, using MARKET sell orders for both legs.
func (s *Strategy) maybeEnter(ctx context.Context, spot float64) {
	s.mu.Lock()
	if s.entered || s.exited || spot <= 0 || s.trader == nil {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

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
	s.legs[ce.TradingSymbol] = leg{symbol: ce.TradingSymbol, side: broker.SideSell, strike: atm, typ: options.Call, qty: qty, open: true}
	s.legs[pe.TradingSymbol] = leg{symbol: pe.TradingSymbol, side: broker.SideSell, strike: atm, typ: options.Put, qty: qty, open: true}
	s.mu.Unlock()

	if s.logger != nil {
		s.logger.Info("ENTER short straddle", "spot", spot, "strike", atm,
			"ce", ce.TradingSymbol, "pe", pe.TradingSymbol, "qty", qty)
	}
	// Sell both legs at market.
	s.sell(ctx, ce.TradingSymbol, qty)
	s.sell(ctx, pe.TradingSymbol, qty)
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
	s.mu.Unlock()

	// Time-based square-off.
	if pastSquareOff(s.squareOffClock) {
		s.squareOff(ctx, "square-off time")
		return
	}

	// Delta-based square-off: compute net delta of the short straddle.
	netDelta := 0.0
	now := time.Now()
	for _, l := range legsCopy {
		if !l.open {
			continue
		}
		ins, ok := s.trader.Lookup(l.symbol)
		if !ok {
			continue
		}
		premium := s.trader.LTP(l.symbol)
		if premium <= 0 {
			continue
		}
		t := options.YearsToExpiry(now, ins.Expiry)
		iv, err := options.ImpliedVol(premium, spot, l.strike, t, s.riskFreeRate, l.typ)
		if err != nil {
			continue
		}
		g := options.BlackScholes(spot, l.strike, t, iv, s.riskFreeRate, l.typ)
		// We are SHORT the option, so the position delta is the negative of the
		// option's delta.
		netDelta += -g.Delta
	}

	if math.Abs(netDelta) > s.exitDelta {
		s.squareOff(ctx, "delta limit")
	}
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

// pastSquareOff reports whether the current IST time is past the square-off
// clock (e.g. "15:15"). Returns false if the clock string is unparseable.
func pastSquareOff(clock string) bool {
	t, err := time.Parse("15:04", clock)
	if err != nil {
		return false
	}
	ist := time.FixedZone("IST", 5*3600+30*60)
	now := time.Now().In(ist)
	target := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, ist)
	return now.After(target)
}
