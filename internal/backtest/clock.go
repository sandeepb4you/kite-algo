// Package backtest replays historical market data through the platform's real
// strategy, broker, and risk-management code.
//
// The design goal is that a strategy runs UNMODIFIED in backtest, paper, and
// live. That works because strategies only ever talk to strategy.Trader, and
// because the backtester drives the same broker.PaperBroker that paper trading
// uses — so simulated execution is not a second implementation that can quietly
// disagree with the first.
//
// Determinism is an invariant, not an aspiration. A backtest is a measurement,
// and a measurement that changes between runs is worthless. Concretely:
//
//   - The runner is single-goroutine. There are no `go` statements here.
//   - The event feed orders by (time, symbol, path index) — a total order, never
//     map iteration.
//   - Time comes only from SimClock. No code in this package calls time.Now().
package backtest

import (
	"sync/atomic"
	"time"
)

// SimClock is the simulated clock a backtest runs on.
//
// The replay loop advances it to each event's timestamp before dispatching, so
// a strategy asking the time sees the market's time, not the wall clock. It is
// atomic because the paper broker reads it from the same goroutine but through
// a function value, and cheap atomics cost nothing here.
type SimClock struct {
	nanos atomic.Int64
}

// NewSimClock returns a clock positioned at t.
func NewSimClock(t time.Time) *SimClock {
	c := &SimClock{}
	c.Set(t)
	return c
}

// Now returns the current simulated time.
func (c *SimClock) Now() time.Time {
	return time.Unix(0, c.nanos.Load()).In(IST)
}

// Set moves the clock to t. The replay loop only ever moves it forward.
func (c *SimClock) Set(t time.Time) { c.nanos.Store(t.UnixNano()) }

// Advance moves the clock on by d.
func (c *SimClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

// IST is the exchange timezone; every simulated timestamp is reported in it so
// logs and ledgers read the way an Indian trader expects.
var IST = time.FixedZone("IST", 5*3600+30*60)
