// Package analytics turns fills into round-trip trades and performance metrics.
//
// It is deliberately separate from the backtester so the same code produces the
// numbers for a backtest, a paper session, and live trading. "Does the backtest
// agree with paper?" is then a question about execution, not about two different
// implementations of what a trade is.
package analytics

import (
	"math"
	"sort"
	"time"

	"kite-algo/internal/broker"
)

// Trade is one completed round trip in a single instrument.
type Trade struct {
	Seq           int           `json:"seq"`
	StrategyID    string        `json:"strategy_id"`
	TradingSymbol string        `json:"trading_symbol"`
	Direction     broker.Side   `json:"direction"` // side of the OPENING leg
	Quantity      int           `json:"quantity"`
	EntryTime     time.Time     `json:"entry_time"`
	ExitTime      time.Time     `json:"exit_time"`
	EntryPrice    float64       `json:"entry_price"`
	ExitPrice     float64       `json:"exit_price"`
	GrossPnL      float64       `json:"gross_pnl"`
	Costs         float64       `json:"costs"`
	NetPnL        float64       `json:"net_pnl"`
	Holding       time.Duration `json:"holding"`
	ExitReason    string        `json:"exit_reason,omitempty"`
}

// IsWin reports whether the trade made money after costs.
func (t Trade) IsWin() bool { return t.NetPnL > 0 }

// CostFunc returns the charges attributable to one fill.
type CostFunc func(broker.Fill) float64

// BuildTrades matches fills into round trips, FIFO, per strategy and symbol.
//
// FIFO matters for correctness, not just convention: a strategy that scales into
// a position and then exits in pieces has no single "entry price", and matching
// the wrong lots would misreport both the P&L of individual trades and the
// holding periods. The total across all trades is unaffected, but the
// distribution — win rate, average win, drawdown attribution — is not.
func BuildTrades(fills []broker.Fill, cost CostFunc) []Trade {
	if len(fills) == 0 {
		return nil
	}

	// Chronological order is required for FIFO to mean anything.
	ordered := make([]broker.Fill, len(fills))
	copy(ordered, fills)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Timestamp.Before(ordered[j].Timestamp)
	})

	type key struct{ strategy, symbol string }
	type lot struct {
		qty   int
		price float64
		at    time.Time
		side  broker.Side
		cost  float64
	}

	open := make(map[key][]lot)
	var trades []Trade

	for _, f := range ordered {
		if f.Quantity <= 0 {
			continue
		}
		k := key{f.StrategyID, f.TradingSymbol}
		var fillCost float64
		if cost != nil {
			fillCost = cost(f)
		}
		// Charges are apportioned across the fill's units so a partial match
		// carries a proportional share.
		costPerUnit := fillCost / float64(f.Quantity)

		remaining := f.Quantity
		lots := open[k]

		// Close against opposing lots first.
		for remaining > 0 && len(lots) > 0 && lots[0].side != f.Side {
			head := &lots[0]
			matched := head.qty
			if remaining < matched {
				matched = remaining
			}

			entryPrice, exitPrice := head.price, f.Price
			gross := (exitPrice - entryPrice) * float64(matched)
			if head.side == broker.SideSell {
				gross = -gross // short: profit when the price falls
			}
			costs := head.cost*float64(matched) + costPerUnit*float64(matched)

			trades = append(trades, Trade{
				Seq:           len(trades) + 1,
				StrategyID:    f.StrategyID,
				TradingSymbol: f.TradingSymbol,
				Direction:     head.side,
				Quantity:      matched,
				EntryTime:     head.at,
				ExitTime:      f.Timestamp,
				EntryPrice:    entryPrice,
				ExitPrice:     exitPrice,
				GrossPnL:      gross,
				Costs:         costs,
				NetPnL:        gross - costs,
				Holding:       f.Timestamp.Sub(head.at),
			})

			head.qty -= matched
			remaining -= matched
			if head.qty == 0 {
				lots = lots[1:]
			}
		}

		// Anything left opens (or extends) a position.
		if remaining > 0 {
			lots = append(lots, lot{
				qty: remaining, price: f.Price, at: f.Timestamp,
				side: f.Side, cost: costPerUnit,
			})
		}
		open[k] = lots
	}

	return trades
}

// EquityPoint is one sample of the account's value over time.
type EquityPoint struct {
	Time          time.Time `json:"t"`
	Equity        float64   `json:"equity"`
	Drawdown      float64   `json:"drawdown"`
	OpenPositions int       `json:"open_positions"`
}

// BuildEquityCurve derives a curve from a trade ledger and starting capital.
//
// Equity is initial capital plus cumulative net P&L. Margin is not modelled, so
// for a strategy that sells options this is a P&L curve rather than a true
// account balance — the UI says so, because a return percentage computed against
// a notional capital figure is easy to mistake for something more rigorous.
func BuildEquityCurve(trades []Trade, initialCapital float64) []EquityPoint {
	if len(trades) == 0 {
		return nil
	}
	sorted := make([]Trade, len(trades))
	copy(sorted, trades)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ExitTime.Before(sorted[j].ExitTime) })

	out := make([]EquityPoint, 0, len(sorted))
	equity := initialCapital
	peak := initialCapital

	for _, t := range sorted {
		equity += t.NetPnL
		if equity > peak {
			peak = equity
		}
		out = append(out, EquityPoint{
			Time:     t.ExitTime,
			Equity:   equity,
			Drawdown: peak - equity,
		})
	}
	return out
}

func abs(f float64) float64 { return math.Abs(f) }
