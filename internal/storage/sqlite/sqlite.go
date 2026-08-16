// Package sqlite is the default storage.Store implementation backed by a
// single-file SQLite database (pure-Go driver, no CGO required).
//
// We use database/sql with modernc.org/sqlite so the platform compiles on
// Windows without a C toolchain. The schema is embedded via go:embed and
// applied idempotently on Open.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed" // for the schema embed
	"fmt"
	"log/slog"
	"time"

	_ "modernc.org/sqlite"

	"kite-algo/internal/broker"
	"kite-algo/internal/marketdata"
)

//go:embed schema.sql
var schemaSQL string

// Store is a storage.Store backed by SQLite.
type Store struct {
	db     *sql.DB
	logger *slog.Logger
}

// New opens (or creates) the SQLite database at path, applies the schema, and
// returns a ready Store. The directory of path must already exist.
func New(ctx context.Context, path string, logger *slog.Logger) (*Store, error) {
	// _pragma strings tune SQLite for our workload:
	//   busy_timeout — wait instead of erroring on lock contention
	//   foreign_keys — enforce FK constraints
	//   journal_mode=WAL — concurrent readers + single writer, good for ticks
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// SQLite scales poorly with unlimited connections; one writer is plenty.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	if err := migrate(ctx, db, logger); err != nil {
		_ = db.Close()
		return nil, err
	}

	if logger != nil {
		logger.Info("sqlite store opened", "path", path)
	}
	return &Store{db: db, logger: logger}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// SaveOrder inserts or updates an order keyed by its internal id.
func (s *Store) SaveOrder(ctx context.Context, o *broker.Order) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orders (
			id, exchange_order_id, client_order_id, strategy_id, exchange,
			trading_symbol, product, order_type, side, quantity,
			filled_quantity, pending_quantity, price, trigger_price, validity,
			status, tag, mode, reject_reason, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			exchange_order_id=excluded.exchange_order_id,
			filled_quantity=excluded.filled_quantity,
			pending_quantity=excluded.pending_quantity,
			status=excluded.status,
			reject_reason=excluded.reject_reason,
			updated_at=excluded.updated_at`,
		o.ID, o.ExchangeOrderID, o.ClientOrderID, o.StrategyID, o.Exchange,
		o.TradingSymbol, string(o.Product), string(o.OrderType), string(o.Side), o.Quantity,
		o.FilledQuantity, o.PendingQuantity, o.Price, o.TriggerPrice, string(o.Validity),
		string(o.Status), o.Tag, o.Mode, o.RejectReason,
		o.CreatedAt.Format(time.RFC3339Nano), o.UpdatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save order: %w", err)
	}
	return nil
}

// GetOpenOrders returns orders that are still pending or open.
func (s *Store) GetOpenOrders(ctx context.Context) ([]broker.Order, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, exchange_order_id, client_order_id, strategy_id, exchange,
		       trading_symbol, product, order_type, side, quantity,
		       filled_quantity, pending_quantity, price, trigger_price, validity,
		       status, tag, mode, reject_reason, created_at, updated_at
		FROM orders
		WHERE status IN ('PENDING','OPEN')`)
	if err != nil {
		return nil, fmt.Errorf("query open orders: %w", err)
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

// SaveFill inserts a fill row.
func (s *Store) SaveFill(ctx context.Context, f *broker.Fill) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fills (
			id, order_id, exchange_order_id, strategy_id, exchange,
			trading_symbol, side, quantity, price, mode, timestamp
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		f.ID, f.OrderID, f.ExchangeOrderID, f.StrategyID, f.Exchange,
		f.TradingSymbol, string(f.Side), f.Quantity, f.Price, f.Mode,
		f.Timestamp.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save fill: %w", err)
	}
	return nil
}

// UpsertPosition inserts or updates a position keyed by
// strategy+symbol+product+book.
//
// The book is part of the key because the same instrument can be held in both
// at once — manual orders route to the exchange while strategies stay
// simulated. Keying without it lets a paper position overwrite a real one.
func (s *Store) UpsertPosition(ctx context.Context, p *broker.Position) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO positions (
			strategy_id, exchange, trading_symbol, product, book,
			net_quantity, average_price, last_price, pnl, updated
		) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(strategy_id, trading_symbol, product, book) DO UPDATE SET
			net_quantity=excluded.net_quantity,
			average_price=excluded.average_price,
			last_price=excluded.last_price,
			pnl=excluded.pnl,
			updated=excluded.updated`,
		p.StrategyID, p.Exchange, p.TradingSymbol, string(p.Product), p.Book.String(),
		p.NetQuantity, p.AveragePrice, p.LastPrice, p.PnL,
		p.Updated.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("upsert position: %w", err)
	}
	return nil
}

// GetOpenPositions returns all non-flat positions.
func (s *Store) GetOpenPositions(ctx context.Context) ([]broker.Position, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT strategy_id, exchange, trading_symbol, product, book,
		       net_quantity, average_price, last_price, pnl, updated
		FROM positions WHERE net_quantity != 0`)
	if err != nil {
		return nil, fmt.Errorf("query positions: %w", err)
	}
	defer rows.Close()

	var out []broker.Position
	for rows.Next() {
		var p broker.Position
		var updated string
		var product, book string
		if err := rows.Scan(
			&p.StrategyID, &p.Exchange, &p.TradingSymbol, &product, &book,
			&p.NetQuantity, &p.AveragePrice, &p.LastPrice, &p.PnL, &updated,
		); err != nil {
			return nil, fmt.Errorf("scan position: %w", err)
		}
		p.Product = broker.ProductType(product)
		p.Book = broker.Book(book)
		p.Updated, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveTick appends a tick row. Only called when tick recording is enabled.
func (s *Store) SaveTick(ctx context.Context, t *marketdata.Tick) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ticks (
			instrument_token, trading_symbol, exchange, last_price,
			last_quantity, volume, ohlc_open, ohlc_high, ohlc_low, ohlc_close,
			timestamp
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		t.InstrumentToken, t.TradingSymbol, t.Exchange, t.LastPrice,
		t.LastQuantity, t.Volume, t.OHLC.Open, t.OHLC.High, t.OHLC.Low, t.OHLC.Close,
		t.Timestamp.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save tick: %w", err)
	}
	return nil
}

// SaveCandle upserts a candle keyed by symbol+interval+open_time.
func (s *Store) SaveCandle(ctx context.Context, c *marketdata.Candle) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO candles (
			instrument_token, trading_symbol, interval, open, high, low, close,
			volume, open_time, close_time
		) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(trading_symbol, interval, open_time) DO UPDATE SET
			high=excluded.high, low=excluded.low, close=excluded.close,
			volume=excluded.volume, close_time=excluded.close_time`,
		c.InstrumentToken, c.TradingSymbol, c.Interval, c.Open, c.High, c.Low,
		c.Close, c.Volume, c.OpenTime.Format(time.RFC3339Nano),
		c.CloseTime.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("save candle: %w", err)
	}
	return nil
}

// GetDayPnL returns the sum of realized PnL (from closed positions) plus
// unrealized PnL on open positions, for the current calendar day. This is the
// figure the risk manager checks against the daily-loss limit.
//
// For v1 we use the stored positions.pnl as the source of truth (updated by the
// engine as prices move). Realized PnL is folded into positions.pnl on flat-out.
func (s *Store) GetDayPnL(ctx context.Context) (float64, error) {
	var sum sql.NullFloat64
	// Only count positions touched today.
	row := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(pnl), 0) FROM positions
		WHERE date(updated) = date('now')`)
	if err := row.Scan(&sum); err != nil {
		return 0, fmt.Errorf("query day pnl: %w", err)
	}
	if !sum.Valid {
		return 0, nil
	}
	return sum.Float64, nil
}

// GetDayPnLByBook is GetDayPnL for one book.
//
// The books are summed separately because they hold different kinds of money.
// A blended total would let a simulated loss trip the limit that guards real
// capital, and would report a figure that is neither one thing nor the other.
func (s *Store) GetDayPnLByBook(ctx context.Context, book broker.Book) (float64, error) {
	var sum sql.NullFloat64
	row := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(pnl), 0) FROM positions
		WHERE date(updated) = date('now') AND book = ?`, book.String())
	if err := row.Scan(&sum); err != nil {
		return 0, fmt.Errorf("query day pnl for %s book: %w", book, err)
	}
	if !sum.Valid {
		return 0, nil
	}
	return sum.Float64, nil
}

// scanOrder reads an order row from the current cursor position.
func scanOrder(rows *sql.Rows) (broker.Order, error) {
	var o broker.Order
	var product, orderType, side, validity, status string
	var created, updated string
	if err := rows.Scan(
		&o.ID, &o.ExchangeOrderID, &o.ClientOrderID, &o.StrategyID, &o.Exchange,
		&o.TradingSymbol, &product, &orderType, &side, &o.Quantity,
		&o.FilledQuantity, &o.PendingQuantity, &o.Price, &o.TriggerPrice, &validity,
		&status, &o.Tag, &o.Mode, &o.RejectReason, &created, &updated,
	); err != nil {
		return o, fmt.Errorf("scan order: %w", err)
	}
	o.Product = broker.ProductType(product)
	o.OrderType = broker.OrderType(orderType)
	o.Side = broker.Side(side)
	o.Validity = broker.Validity(validity)
	o.Status = broker.OrderStatus(status)
	o.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	o.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return o, nil
}
