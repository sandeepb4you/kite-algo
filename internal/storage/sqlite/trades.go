package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"kite-algo/internal/broker"
)

// Fills returns fills in [from, to), oldest first.
//
// Ordered ascending because that is what pairing needs: analytics.BuildTrades
// walks fills in execution order to match an exit against the entry it closes,
// and a descending scan would pair every trade backwards.
func (s *Store) Fills(ctx context.Context, from, to time.Time) ([]broker.Fill, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, order_id, exchange_order_id, strategy_id, exchange,
		       trading_symbol, side, quantity, price, mode, timestamp
		FROM fills
		WHERE timestamp >= ? AND timestamp < ?
		ORDER BY timestamp, rowid`,
		from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("query fills: %w", err)
	}
	defer rows.Close()

	var out []broker.Fill
	for rows.Next() {
		f, err := scanFill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ActivitySpan reports the oldest and newest timestamps across fills and orders.
func (s *Store) ActivitySpan(ctx context.Context) (first, last time.Time, ok bool, err error) {
	// Both tables, because an account whose orders were all rejected has real
	// history and no fills at all. Timestamps are RFC3339Nano strings and sort
	// lexically in the same order they sort chronologically, so MIN/MAX over
	// the union is correct without parsing.
	var lo, hi sql.NullString
	if err := s.db.QueryRowContext(ctx, `
		SELECT MIN(ts), MAX(ts) FROM (
			SELECT timestamp  AS ts FROM fills
			UNION ALL
			SELECT created_at AS ts FROM orders
		)`).Scan(&lo, &hi); err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("query activity span: %w", err)
	}
	if !lo.Valid || !hi.Valid {
		return time.Time{}, time.Time{}, false, nil
	}
	f, err1 := time.Parse(time.RFC3339Nano, lo.String)
	l, err2 := time.Parse(time.RFC3339Nano, hi.String)
	if err1 != nil || err2 != nil {
		return time.Time{}, time.Time{}, false, nil
	}
	return f, l, true, nil
}

// Orders returns orders created in [from, to), newest first.
//
// Unlike GetOpenOrders this includes terminal states. A rejected or cancelled
// order is usually the row an operator is hunting for — "why did nothing
// happen?" is answered by the reject_reason on an order that never filled.
func (s *Store) Orders(ctx context.Context, from, to time.Time) ([]broker.Order, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, exchange_order_id, client_order_id, strategy_id, exchange,
		       trading_symbol, product, order_type, side, quantity,
		       filled_quantity, pending_quantity, price, trigger_price, validity,
		       status, tag, mode, reject_reason, created_at, updated_at
		FROM orders
		WHERE created_at >= ? AND created_at < ?
		ORDER BY created_at DESC, rowid DESC`,
		from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("query orders: %w", err)
	}
	defer rows.Close()

	var out []broker.Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// scanFill reads one fill row.
func scanFill(rows *sql.Rows) (broker.Fill, error) {
	var f broker.Fill
	var side, ts string
	if err := rows.Scan(
		&f.ID, &f.OrderID, &f.ExchangeOrderID, &f.StrategyID, &f.Exchange,
		&f.TradingSymbol, &side, &f.Quantity, &f.Price, &f.Mode, &ts,
	); err != nil {
		return f, fmt.Errorf("scan fill: %w", err)
	}
	f.Side = broker.Side(side)
	f.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
	return f, nil
}
