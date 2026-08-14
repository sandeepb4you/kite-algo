package storage

import (
	"context"
	"time"

	"kite-algo/internal/marketdata"
)

// TimeRange is a half-open interval [From, To).
type TimeRange struct {
	From time.Time
	To   time.Time
}

// Overlaps reports whether two ranges intersect.
func (r TimeRange) Overlaps(o TimeRange) bool {
	return r.From.Before(o.To) && o.From.Before(r.To)
}

// Contains reports whether r fully covers o.
func (r TimeRange) Contains(o TimeRange) bool {
	return !o.From.Before(r.From) && !o.To.After(r.To)
}

// Duration is the length of the range.
func (r TimeRange) Duration() time.Duration { return r.To.Sub(r.From) }

// InstrumentRow is one instrument as it existed on a given day.
type InstrumentRow struct {
	InstrumentToken uint32
	TradingSymbol   string
	Name            string
	Expiry          time.Time
	Strike          float64
	LotSize         int
	InstrumentType  string
	Segment         string
	Exchange        string
	TickSize        float64
}

// HistoryStore persists historical market data and point-in-time instrument
// masters. It is separate from Store so the trading path's interface stays
// small; *sqlite.Store implements both.
type HistoryStore interface {
	// GetCandles returns cached candles in [from, to), ordered by open time.
	GetCandles(ctx context.Context, symbol, interval string, from, to time.Time) ([]marketdata.Candle, error)

	// SaveCandles writes many candles in one transaction. A year of minute bars
	// is ~92k rows; one statement per row against a single-connection SQLite
	// handle takes minutes, batched it takes under a second.
	SaveCandles(ctx context.Context, candles []marketdata.Candle) error

	// Coverage returns the windows already fetched for a symbol and interval.
	Coverage(ctx context.Context, symbol, interval string) ([]TimeRange, error)

	// AddCoverage records that a window has been fetched, so an empty result
	// (a holiday, an untraded minute) is not mistaken for missing data and
	// re-requested forever.
	AddCoverage(ctx context.Context, symbol, interval, source string, r TimeRange) error

	// GetTicks returns recorded ticks for a symbol, used to build candles when
	// no historical subscription is available.
	GetTicks(ctx context.Context, symbol string, from, to time.Time) ([]marketdata.Tick, error)

	// SaveInstrumentSnapshot records the instrument master for one day.
	SaveInstrumentSnapshot(ctx context.Context, asOf time.Time, rows []InstrumentRow) error

	// HasInstrumentSnapshot reports whether a snapshot exists for a day.
	HasInstrumentSnapshot(ctx context.Context, asOf time.Time) (bool, error)

	// InstrumentsAsOf returns the instrument master as it stood on a day. This
	// is the only way a backtest can resolve an expired option contract, since
	// Kite's live feed lists current contracts only.
	InstrumentsAsOf(ctx context.Context, asOf time.Time) ([]InstrumentRow, error)
}
