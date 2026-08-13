// Package storage defines the persistence interface for the trading platform.
//
// The interface is small and synchronous: the trading engine calls it to record
// orders, fills, positions, and (optionally) market data. SQLite is the default
// implementation, but the interface lets us swap in another backend later.
package storage

import (
	"context"

	"kite-algo/internal/broker"
	"kite-algo/internal/marketdata"
)

// Store is the persistence boundary for everything the platform does.
type Store interface {
	// Close releases any underlying resources (e.g. the DB handle).
	Close() error

	// Orders
	SaveOrder(ctx context.Context, o *broker.Order) error
	GetOpenOrders(ctx context.Context) ([]broker.Order, error)

	// Fills
	SaveFill(ctx context.Context, f *broker.Fill) error

	// Positions (upsert: a position is keyed by strategy + symbol + product)
	UpsertPosition(ctx context.Context, p *broker.Position) error
	GetOpenPositions(ctx context.Context) ([]broker.Position, error)

	// Market data (only used when recording is enabled)
	SaveTick(ctx context.Context, t *marketdata.Tick) error
	SaveCandle(ctx context.Context, c *marketdata.Candle) error

	// Aggregate realized + unrealized PnL for the current trading day,
	// used by the risk manager to enforce the daily loss limit.
	GetDayPnL(ctx context.Context) (float64, error)
}
