package storage

import (
	"context"
	"time"

	"kite-algo/internal/broker"
)

// TradeStore reads back the order and fill history the trading path writes.
//
// Separate from Store, and for the same reason HistoryStore is: the trading
// path only ever writes, and forcing every fake broker-side store in the tests
// to grow read methods it never uses buys nothing. *sqlite.Store implements
// both; callers type-assert.
//
// This exists because the platform was recording fills and had no way to read
// one back. Store's only order query is GetOpenOrders, so as soon as an order
// closed it left the UI for good — the data was on disk and unreachable, which
// is indistinguishable from not collecting it.
type TradeStore interface {
	// Fills returns fills in [from, to), oldest first. This is the raw material
	// for round-trip trades: analytics.BuildTrades pairs them into positions.
	Fills(ctx context.Context, from, to time.Time) ([]broker.Fill, error)

	// Orders returns orders created in [from, to), newest first — including
	// rejected and cancelled ones, which are exactly the rows an operator goes
	// looking for after something did not happen.
	Orders(ctx context.Context, from, to time.Time) ([]broker.Order, error)

	// ActivitySpan reports the oldest and newest timestamps across BOTH fills
	// and orders, so a history page can default its date range to where the
	// data actually is rather than to an empty last-30-days.
	//
	// It covers orders as well as fills because the two can legitimately
	// disagree: an account that has only ever had orders rejected has plenty of
	// history worth reading and not one fill. Defaulting off fills alone made
	// that account's page open blank on a database with 34 orders in it.
	ActivitySpan(ctx context.Context) (first, last time.Time, ok bool, err error)
}
