package history

import (
	"context"
	"log/slog"
	"time"

	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage"
)

// TickProvider builds candles by aggregating ticks the platform recorded
// itself. It is the fallback for anyone without Zerodha's historical-data
// subscription, and it only covers periods when the server was running with
// recording.ticks enabled.
type TickProvider struct {
	store  storage.HistoryStore
	logger *slog.Logger
}

// NewTickProvider builds a provider over recorded ticks.
func NewTickProvider(store storage.HistoryStore, logger *slog.Logger) *TickProvider {
	return &TickProvider{store: store, logger: logger}
}

// Name identifies this provider. It is recorded as the source in coverage rows
// so a backtest can be labelled as running on tick-derived bars rather than
// exchange data — the two are not equivalent and should not be confused.
func (p *TickProvider) Name() string { return "ticks" }

// Candles aggregates recorded ticks into bars.
func (p *TickProvider) Candles(ctx context.Context, req Request) ([]marketdata.Candle, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	ticks, err := p.store.GetTicks(ctx, req.Symbol, req.From, req.To)
	if err != nil {
		return nil, err
	}
	return Aggregate(ticks, req.Interval), nil
}

// Aggregate folds ticks into OHLC bars at the given interval.
//
// Volume needs care: marketdata.Tick.Volume is the day's CUMULATIVE traded
// quantity, not the quantity of that trade. A bar's volume is therefore the
// difference between the last cumulative reading in the bar and the last one
// before it — summing the field instead would produce numbers larger than the
// day's entire turnover.
func Aggregate(ticks []marketdata.Tick, interval kite.Interval) []marketdata.Candle {
	if len(ticks) == 0 {
		return nil
	}
	dur := interval.Duration()
	if dur <= 0 {
		return nil
	}

	var (
		out       []marketdata.Candle
		current   *marketdata.Candle
		bucketEnd time.Time
		prevCum   int64 // cumulative volume at the end of the previous bar
		lastCum   int64
	)

	for _, t := range ticks {
		if t.Timestamp.IsZero() || t.LastPrice <= 0 {
			continue
		}
		start := t.Timestamp.In(IST).Truncate(dur)

		if current == nil || !start.Before(bucketEnd) {
			if current != nil {
				current.Volume = lastCum - prevCum
				if current.Volume < 0 {
					// A new trading day resets the cumulative counter; treat the
					// first bar after the reset as carrying its own reading.
					current.Volume = lastCum
				}
				prevCum = lastCum
				out = append(out, *current)
			}
			bucketEnd = start.Add(dur)
			current = &marketdata.Candle{
				InstrumentToken: t.InstrumentToken,
				TradingSymbol:   t.TradingSymbol,
				Interval:        string(interval),
				Open:            t.LastPrice,
				High:            t.LastPrice,
				Low:             t.LastPrice,
				Close:           t.LastPrice,
				OpenTime:        start,
				CloseTime:       bucketEnd,
			}
		}

		if t.LastPrice > current.High {
			current.High = t.LastPrice
		}
		if t.LastPrice < current.Low {
			current.Low = t.LastPrice
		}
		current.Close = t.LastPrice
		if t.Volume > 0 {
			lastCum = t.Volume
		}
	}

	if current != nil {
		current.Volume = lastCum - prevCum
		if current.Volume < 0 {
			current.Volume = lastCum
		}
		out = append(out, *current)
	}
	return out
}
