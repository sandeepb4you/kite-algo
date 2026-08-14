// Package strategy defines the pluggable strategy contract. A strategy is any
// type that implements Strategy; the trading engine hands it a Trader (itself)
// at Init and then fans live ticks and fills into it.
//
// Strategies never call the broker directly — they go through Trader, which the
// engine wires to the risk manager, broker, ticker, and instrument master. This
// keeps risk checks and lookups centralized.
package strategy

import (
	"context"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/marketdata"
)

// Strategy is the contract every trading strategy implements.
type Strategy interface {
	// Name uniquely identifies the strategy instance (used as StrategyID on
	// orders, for audit and position tracking).
	Name() string

	// Init is called once at startup. The engine passes itself as the Trader the
	// strategy uses to place orders, plus its declarative config block.
	Init(ctx context.Context, trader Trader, cfg config.StrategyCfg) error

	// OnTick is called for every subscribed market-data tick.
	OnTick(ctx context.Context, tick marketdata.Tick)

	// OnFill is called when one of the strategy's orders fills (paper or live).
	OnFill(ctx context.Context, fill broker.Fill)

	// Stop is called when the strategy is being shut down. It should release
	// resources and stop reacting to the market.
	//
	// Stop must NOT place orders. Whether an outgoing strategy's open positions
	// are squared off is the operator's decision, made per-stop in the UI, and
	// the engine carries it out — see Flattener.
	Stop(ctx context.Context) error
}

// Flattener is implemented by strategies that know how to unwind their own
// position correctly — for example buying back short option legs before selling
// long ones, so the account is never briefly naked.
//
// The engine prefers this over its own generic square-off when stopping a
// strategy with "square off" requested.
type Flattener interface {
	SquareOff(ctx context.Context, reason string) error
}

// Instrument is the minimal instrument view a strategy needs. It's deliberately
// decoupled from the kite package so strategies don't import the broker SDK.
type Instrument struct {
	Token          uint32
	TradingSymbol  string
	Name           string // underlying
	Expiry         time.Time
	Strike         float64
	LotSize        int
	InstrumentType string // CE, PE, ...
	Exchange       string
}

// Trader is the trading capability the engine provides to strategies. It routes
// through the risk manager, persists every order, and exposes market-data lookups.
type Trader interface {
	// PlaceOrder submits an order after risk checks; returns the recorded Order.
	PlaceOrder(ctx context.Context, req broker.OrderRequest) (*broker.Order, error)

	// CancelOrder cancels a previously placed order.
	CancelOrder(ctx context.Context, orderID string) error

	// LTP returns the latest known price for a trading symbol (0 if unknown).
	LTP(symbol string) float64

	// LotSize returns the lot size for a trading symbol (0 if unknown).
	LotSize(symbol string) int

	// Lookup resolves a trading symbol to its Instrument.
	Lookup(symbol string) (Instrument, bool)

	// Options returns the option chain for an underlying's nearest expiry on or
	// after minExpiry (pass time.Time{} for the soonest expiry), sorted by strike.
	Options(underlying string, minExpiry time.Time) []Instrument

	// Subscribe requests market-data streaming for the given trading symbols.
	Subscribe(symbols []string) error

	// Now returns the current time in the strategy's frame of reference.
	//
	// Strategies MUST use this instead of time.Now(). In live and paper mode it
	// is wall-clock; in a backtest it is simulated time. A strategy that reads
	// the real clock while replaying last month's data will evaluate its
	// square-off window and its option greeks against today, which makes the
	// backtest quietly meaningless rather than obviously broken.
	Now() time.Time

	// Signal emits a human-readable decision or observation to the platform's
	// event stream, where it appears in the UI's activity feed and as the
	// strategy's last-signal line. It never blocks and never affects trading.
	Signal(s Signal)
}

// Signal is a strategy-authored event describing what it decided and why.
type Signal struct {
	StrategyID string         // filled in by the engine if empty
	Kind       string         // "enter" | "exit" | "skip" | free-form
	Symbol     string         // optional
	Level      string         // "info" | "warn" | "error"; empty means info
	Message    string         // one line, written for a human
	Data       map[string]any // supporting numbers, e.g. {"net_delta": -0.31}
	At         time.Time      // filled in by the engine if zero
}
