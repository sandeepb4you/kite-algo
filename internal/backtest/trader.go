package backtest

import (
	"context"
	"fmt"
	"sort"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/history"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
	"kite-algo/internal/strategy"
)

// Trader implements strategy.Trader against the simulated broker and clock.
//
// It runs the REAL risk manager, so a backtest reports the trades a strategy
// would actually have been allowed to make. A backtest that ignores position
// limits and daily-loss halts measures a strategy nobody could have run.
type Trader struct {
	clock  *SimClock
	broker *broker.PaperBroker
	risk   *risk.Manager
	feed   Feed
	instr  *history.AsOfInstruments

	prices map[string]float64
	fills  []broker.Fill
	orders []broker.Order

	signals []strategy.Signal

	warmupUntil time.Time
	warmupSkips int
}

// ErrWarmup rejects orders placed before the warmup period ends.
var ErrWarmup = fmt.Errorf("backtest: still in warmup")

// PlaceOrder risk-checks and submits to the simulated broker.
func (t *Trader) PlaceOrder(ctx context.Context, req broker.OrderRequest) (*broker.Order, error) {
	if t.clock.Now().Before(t.warmupUntil) {
		// Indicators and state need history before a strategy's decisions mean
		// anything. Counting these tells the operator when warmup was too short.
		t.warmupSkips++
		return nil, ErrWarmup
	}

	lotSize := t.LotSize(req.TradingSymbol)
	openPositions, hasPosition := t.positionState(req.TradingSymbol)
	dayPnL := t.dayPnL()

	if err := t.risk.Check(ctx, req, lotSize, openPositions, dayPnL, !hasPosition); err != nil {
		return nil, err
	}

	o, err := t.broker.PlaceOrder(ctx, req)
	if err != nil {
		return nil, err
	}
	t.orders = append(t.orders, *o)
	return o, nil
}

// CancelOrder cancels a pending simulated order.
func (t *Trader) CancelOrder(ctx context.Context, orderID string) error {
	return t.broker.CancelOrder(ctx, orderID)
}

// LTP returns the last simulated price for a symbol.
func (t *Trader) LTP(symbol string) float64 { return t.prices[symbol] }

// LotSize resolves the lot size from the point-in-time instrument master.
func (t *Trader) LotSize(symbol string) int {
	if t.instr == nil {
		return 0
	}
	if inst, ok := t.instr.Lookup(symbol); ok {
		return inst.LotSize
	}
	return 0
}

// Lookup resolves a symbol as it existed on the simulated date.
func (t *Trader) Lookup(symbol string) (strategy.Instrument, bool) {
	if t.instr == nil {
		return strategy.Instrument{}, false
	}
	row, ok := t.instr.Lookup(symbol)
	if !ok {
		return strategy.Instrument{}, false
	}
	return toStrategyInstrument(row), true
}

// Options returns the option chain as it existed on the simulated date.
//
// This is the reason instrument snapshots exist. Resolving the chain from the
// live instrument master would be lookahead at best and impossible at worst,
// since expired contracts are simply absent from it.
func (t *Trader) Options(underlying string, minExpiry time.Time) []strategy.Instrument {
	if t.instr == nil {
		return nil
	}
	rows := t.instr.Options(underlying, minExpiry)
	out := make([]strategy.Instrument, 0, len(rows))
	for _, r := range rows {
		out = append(out, toStrategyInstrument(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Strike < out[j].Strike })
	return out
}

// Subscribe attaches symbols to the feed mid-run.
func (t *Trader) Subscribe(symbols []string) error {
	if t.feed == nil {
		return nil
	}
	return t.feed.Add(context.Background(), symbols...)
}

// Now returns simulated time.
func (t *Trader) Now() time.Time { return t.clock.Now() }

// Signal records a strategy's decision for the run report.
func (t *Trader) Signal(s strategy.Signal) {
	if s.At.IsZero() {
		s.At = t.clock.Now()
	}
	t.signals = append(t.signals, s)
}

// onFill records a simulated execution.
func (t *Trader) onFill(f broker.Fill) { t.fills = append(t.fills, f) }

// positionState reports the open-position count and whether symbol is held.
func (t *Trader) positionState(symbol string) (open int, has bool) {
	positions, err := t.broker.GetPositions(context.Background())
	if err != nil {
		return 0, false
	}
	for _, p := range positions {
		if p.NetQuantity == 0 {
			continue
		}
		open++
		if p.TradingSymbol == symbol {
			has = true
		}
	}
	return open, has
}

// dayPnL marks the book to the latest simulated prices.
func (t *Trader) dayPnL() float64 {
	positions, err := t.broker.GetPositions(context.Background())
	if err != nil {
		return 0
	}
	var total float64
	for _, p := range positions {
		total += broker.MarkToMarket(p, t.prices[p.TradingSymbol])
	}
	return total
}

// toStrategyInstrument converts a snapshot row to the strategy-facing view.
func toStrategyInstrument(r storage.InstrumentRow) strategy.Instrument {
	return strategy.Instrument{
		Token:          r.InstrumentToken,
		TradingSymbol:  r.TradingSymbol,
		Name:           r.Name,
		Expiry:         r.Expiry,
		Strike:         r.Strike,
		LotSize:        r.LotSize,
		InstrumentType: r.InstrumentType,
		Exchange:       r.Exchange,
	}
}
