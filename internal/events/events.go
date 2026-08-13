// Package events is the platform's internal fan-out from the trading engine to
// observers — principally the web UI, which pushes them to the browser over a
// WebSocket.
//
// It exists to break an import cycle: the engine publishes events and the web
// layer subscribes to them, and neither package imports the other. This package
// depends only on the leaf domain packages (broker, marketdata).
//
// The cardinal rule here is that publishing MUST NOT block. Engine.handleTick
// runs synchronously on the Kite ticker's WebSocket read goroutine, so a
// publisher that waits on a slow subscriber would stall market data for every
// strategy in the process. Delivery is therefore best-effort: a subscriber that
// cannot keep up loses events rather than slowing the trading path.
package events

import (
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/marketdata"
)

// Kind identifies what an Event carries. The zero value is not a valid Kind.
type Kind string

const (
	// KindTick is a market-data tick. High rate — expect to coalesce these.
	KindTick Kind = "tick"
	// KindOrder is an order that was accepted by a broker.
	KindOrder Kind = "order"
	// KindOrderRejected is an order refused before it reached the broker,
	// typically by the risk manager or the kill switch.
	KindOrderRejected Kind = "order_rejected"
	// KindFill is a (possibly partial) execution.
	KindFill Kind = "fill"
	// KindPositions is a refreshed snapshot of all positions plus day PnL.
	KindPositions Kind = "positions"
	// KindSignal is a strategy-authored decision or observation.
	KindSignal Kind = "signal"
	// KindStatus is a change in engine/session lifecycle state.
	KindStatus Kind = "status"
)

// Level classifies an event for display. Empty means informational.
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Event is a single observation from the trading engine.
//
// Only the fields relevant to Kind are populated; the rest are zero. Pointer
// fields are owned by the subscriber once received and must not be mutated,
// because every subscriber receives the same pointer.
type Event struct {
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at"`

	// Symbol is the trading symbol this event concerns, when it concerns one.
	// Subscribers use it to filter per-client interest without decoding the
	// whole payload.
	Symbol string `json:"symbol,omitempty"`

	// StrategyID attributes the event to a strategy instance, when applicable.
	StrategyID string `json:"strategy_id,omitempty"`

	Tick      *marketdata.Tick  `json:"tick,omitempty"`
	Order     *broker.Order     `json:"order,omitempty"`
	Fill      *broker.Fill      `json:"fill,omitempty"`
	Positions []broker.Position `json:"positions,omitempty"`
	DayPnL    float64           `json:"day_pnl,omitempty"`

	Level   Level          `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// Publisher is the engine-facing side of the bus. The engine holds one of these
// rather than a *Bus so that tests and offline runs can drop events entirely.
type Publisher interface {
	// Publish delivers e to every interested subscriber. It never blocks and
	// never returns an error: events are advisory, and losing one must never
	// affect trading.
	Publish(e Event)
}

// Nop is a Publisher that discards everything. Use it instead of a nil
// Publisher so callers never need a nil check on the hot path.
type Nop struct{}

// Publish discards e.
func (Nop) Publish(Event) {}
