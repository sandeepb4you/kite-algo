package sqlite

import (
	"context"
	"fmt"
	"sort"
	"time"

	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage"
)

// dayFormat is how snapshot dates are keyed, in IST.
const dayFormat = "2006-01-02"

// ist is the exchange timezone; snapshot days are exchange days, not UTC days.
var ist = time.FixedZone("IST", 5*3600+30*60)

// GetCandles returns cached candles in [from, to), ordered by open time.
func (s *Store) GetCandles(ctx context.Context, symbol, interval string, from, to time.Time) ([]marketdata.Candle, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT instrument_token, trading_symbol, interval, open, high, low, close,
		       volume, COALESCE(open_interest, 0), open_time, close_time
		FROM candles
		WHERE trading_symbol = ? AND interval = ?
		  AND open_time >= ? AND open_time < ?
		ORDER BY open_time`,
		symbol, interval, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("query candles: %w", err)
	}
	defer rows.Close()

	var out []marketdata.Candle
	for rows.Next() {
		var (
			c                  marketdata.Candle
			oi                 int64
			openTime, closeTme string
		)
		if err := rows.Scan(&c.InstrumentToken, &c.TradingSymbol, &c.Interval,
			&c.Open, &c.High, &c.Low, &c.Close, &c.Volume, &oi,
			&openTime, &closeTme); err != nil {
			return nil, fmt.Errorf("scan candle: %w", err)
		}
		c.OpenTime, _ = time.Parse(time.RFC3339Nano, openTime)
		c.CloseTime, _ = time.Parse(time.RFC3339Nano, closeTme)
		c.OpenInterest = oi
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveCandles writes candles in a single transaction with a prepared statement.
func (s *Store) SaveCandles(ctx context.Context, candles []marketdata.Candle) error {
	if len(candles) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin candle write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO candles (
			instrument_token, trading_symbol, interval, open, high, low, close,
			volume, open_interest, open_time, close_time
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(trading_symbol, interval, open_time) DO UPDATE SET
			high=excluded.high, low=excluded.low, close=excluded.close,
			volume=excluded.volume, open_interest=excluded.open_interest,
			close_time=excluded.close_time`)
	if err != nil {
		return fmt.Errorf("prepare candle insert: %w", err)
	}
	defer stmt.Close()

	for i := range candles {
		c := &candles[i]
		if _, err := stmt.ExecContext(ctx,
			c.InstrumentToken, c.TradingSymbol, c.Interval, c.Open, c.High, c.Low,
			c.Close, c.Volume, c.OpenInterest,
			c.OpenTime.Format(time.RFC3339Nano), c.CloseTime.Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("save candle %s @ %s: %w", c.TradingSymbol, c.OpenTime, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit candles: %w", err)
	}
	return nil
}

// Coverage returns the fetched windows for a symbol and interval, merged where
// they touch so the caller sees the largest contiguous ranges.
func (s *Store) Coverage(ctx context.Context, symbol, interval string) ([]storage.TimeRange, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT from_time, to_time FROM candle_coverage
		WHERE trading_symbol = ? AND interval = ?
		ORDER BY from_time`, symbol, interval)
	if err != nil {
		return nil, fmt.Errorf("query coverage: %w", err)
	}
	defer rows.Close()

	var ranges []storage.TimeRange
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, fmt.Errorf("scan coverage: %w", err)
		}
		f, err1 := time.Parse(time.RFC3339Nano, from)
		t, err2 := time.Parse(time.RFC3339Nano, to)
		if err1 != nil || err2 != nil {
			continue
		}
		ranges = append(ranges, storage.TimeRange{From: f, To: t})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return MergeRanges(ranges), nil
}

// AddCoverage records a fetched window.
func (s *Store) AddCoverage(ctx context.Context, symbol, interval, source string, r storage.TimeRange) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO candle_coverage (trading_symbol, interval, from_time, to_time, source, fetched_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(trading_symbol, interval, from_time) DO UPDATE SET
			to_time = MAX(to_time, excluded.to_time),
			source = excluded.source,
			fetched_at = excluded.fetched_at`,
		symbol, interval,
		r.From.Format(time.RFC3339Nano), r.To.Format(time.RFC3339Nano),
		source, time.Now().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("record coverage: %w", err)
	}
	return nil
}

// MergeRanges sorts and coalesces overlapping or adjacent ranges.
func MergeRanges(in []storage.TimeRange) []storage.TimeRange {
	if len(in) < 2 {
		return in
	}
	sort.Slice(in, func(i, j int) bool { return in[i].From.Before(in[j].From) })

	out := []storage.TimeRange{in[0]}
	for _, r := range in[1:] {
		last := &out[len(out)-1]
		// Adjacent counts as overlapping: two windows that meet exactly leave
		// no gap between them, and treating them separately would make the
		// caller re-request a zero-length hole forever.
		if !r.From.After(last.To) {
			if r.To.After(last.To) {
				last.To = r.To
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// GetTicks returns recorded ticks for a symbol in [from, to).
func (s *Store) GetTicks(ctx context.Context, symbol string, from, to time.Time) ([]marketdata.Tick, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT instrument_token, trading_symbol, exchange, last_price,
		       last_quantity, volume, ohlc_open, ohlc_high, ohlc_low, ohlc_close, timestamp
		FROM ticks
		WHERE trading_symbol = ? AND timestamp >= ? AND timestamp < ?
		ORDER BY timestamp`,
		symbol, from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("query ticks: %w", err)
	}
	defer rows.Close()

	var out []marketdata.Tick
	for rows.Next() {
		var (
			t  marketdata.Tick
			ts string
		)
		if err := rows.Scan(&t.InstrumentToken, &t.TradingSymbol, &t.Exchange,
			&t.LastPrice, &t.LastQuantity, &t.Volume,
			&t.OHLC.Open, &t.OHLC.High, &t.OHLC.Low, &t.OHLC.Close, &ts); err != nil {
			return nil, fmt.Errorf("scan tick: %w", err)
		}
		t.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveInstrumentSnapshot records the instrument master for one exchange day.
func (s *Store) SaveInstrumentSnapshot(ctx context.Context, asOf time.Time, rows []storage.InstrumentRow) error {
	if len(rows) == 0 {
		return nil
	}
	day := asOf.In(ist).Format(dayFormat)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO instrument_snapshots (
			as_of, instrument_token, trading_symbol, name, expiry, strike,
			lot_size, instrument_type, segment, exchange, tick_size
		) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(as_of, instrument_token) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("prepare snapshot insert: %w", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		expiry := ""
		if !r.Expiry.IsZero() {
			expiry = r.Expiry.Format(dayFormat)
		}
		if _, err := stmt.ExecContext(ctx, day, r.InstrumentToken, r.TradingSymbol,
			r.Name, expiry, r.Strike, r.LotSize, r.InstrumentType,
			r.Segment, r.Exchange, r.TickSize); err != nil {
			return fmt.Errorf("save instrument %s: %w", r.TradingSymbol, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit snapshot: %w", err)
	}
	return nil
}

// HasInstrumentSnapshot reports whether a snapshot exists for an exchange day.
func (s *Store) HasInstrumentSnapshot(ctx context.Context, asOf time.Time) (bool, error) {
	day := asOf.In(ist).Format(dayFormat)
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM instrument_snapshots WHERE as_of = ? LIMIT 1`, day).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check snapshot: %w", err)
	}
	return n > 0, nil
}

// InstrumentsAsOf returns the instrument master recorded for a day, falling
// back to the most recent earlier snapshot.
//
// The fallback matters: a backtest over a Saturday, a holiday, or a day the
// server happened to be down would otherwise resolve no instruments at all and
// look like a strategy bug rather than a data gap.
func (s *Store) InstrumentsAsOf(ctx context.Context, asOf time.Time) ([]storage.InstrumentRow, error) {
	day := asOf.In(ist).Format(dayFormat)

	var actual string
	err := s.db.QueryRowContext(ctx, `
		SELECT as_of FROM instrument_snapshots
		WHERE as_of <= ? ORDER BY as_of DESC LIMIT 1`, day).Scan(&actual)
	if err != nil {
		// No snapshot at or before this date: the caller gets an empty set and
		// must decide whether that is fatal.
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT instrument_token, trading_symbol, name, expiry, strike, lot_size,
		       instrument_type, segment, exchange, tick_size
		FROM instrument_snapshots WHERE as_of = ?`, actual)
	if err != nil {
		return nil, fmt.Errorf("query snapshot: %w", err)
	}
	defer rows.Close()

	var out []storage.InstrumentRow
	for rows.Next() {
		var (
			r      storage.InstrumentRow
			expiry string
		)
		if err := rows.Scan(&r.InstrumentToken, &r.TradingSymbol, &r.Name, &expiry,
			&r.Strike, &r.LotSize, &r.InstrumentType, &r.Segment, &r.Exchange,
			&r.TickSize); err != nil {
			return nil, fmt.Errorf("scan instrument: %w", err)
		}
		if expiry != "" {
			r.Expiry, _ = time.ParseInLocation(dayFormat, expiry, ist)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
