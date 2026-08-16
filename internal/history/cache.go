package history

import (
	"context"
	"log/slog"
	"time"

	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage"
)

// CacheProvider serves candles from storage, fetching only what is missing.
//
// The coverage table is what makes this correct rather than merely fast.
// Without it there is no way to distinguish "we have no candles because we never
// asked" from "we have no candles because the market was closed" — so every
// backtest spanning a weekend or a holiday would re-hit the rate-limited,
// paid-for API for data that does not exist and never will.
type CacheProvider struct {
	store    storage.HistoryStore
	upstream Provider
	calendar *Calendar
	logger   *slog.Logger
}

// NewCacheProvider wraps an upstream provider with persistent caching.
func NewCacheProvider(store storage.HistoryStore, upstream Provider, logger *slog.Logger) *CacheProvider {
	return &CacheProvider{
		store:    store,
		upstream: upstream,
		calendar: NSE(),
		logger:   logger,
	}
}

// SetCalendar replaces the trading calendar used to skip closed windows.
//
// The default calendar knows weekends but no holidays, which is safe (a holiday
// costs one empty request) but wasteful once the operator has actually listed
// them in config. Capture passes its configured calendar through so the two
// agree on what a trading day is.
func (p *CacheProvider) SetCalendar(cal *Calendar) {
	if cal != nil {
		p.calendar = cal
	}
}

// Name identifies this provider.
func (p *CacheProvider) Name() string {
	if p.upstream == nil {
		return "cache"
	}
	return "cache(" + p.upstream.Name() + ")"
}

// Candles returns the requested window, fetching any uncovered sub-ranges.
func (p *CacheProvider) Candles(ctx context.Context, req Request) ([]marketdata.Candle, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	covered, err := p.store.Coverage(ctx, req.Symbol, string(req.Interval))
	if err != nil {
		return nil, err
	}

	gaps := Subtract(storage.TimeRange{From: req.From, To: req.To}, covered)

	// Skip gaps the exchange was shut for entirely — a weekend-only gap is a
	// guaranteed-empty response and pure waste of the rate-limit budget.
	//
	// But fetch each remaining gap as ONE request spanning its first open to its
	// last close, rather than one request per trading day. Splitting was the
	// original behaviour and it multiplies a backfill by the number of days in
	// it: a 30-day gap became 22 sequential round trips per contract, turning a
	// few hundred requests into ~14,000 and a twenty-minute job into six hours.
	// The API already caps a request's span itself (100 days at 5-minute
	// resolution) and simply returns nothing for the closed hours inside it, so
	// the per-day split bought nothing the whole-gap skip does not.
	var toFetch []storage.TimeRange
	for _, gap := range gaps {
		windows := p.calendar.TradingWindows(gap)
		if len(windows) == 0 {
			continue
		}
		toFetch = append(toFetch, storage.TimeRange{
			From: windows[0].From,
			To:   windows[len(windows)-1].To,
		})
	}

	if len(toFetch) > 0 && p.upstream != nil {
		p.fetchMissing(ctx, req, gaps, toFetch)
	}

	return p.store.GetCandles(ctx, req.Symbol, string(req.Interval), req.From, req.To)
}

// fetchMissing downloads and stores the uncovered windows.
func (p *CacheProvider) fetchMissing(ctx context.Context, req Request, gaps, toFetch []storage.TimeRange) {
	if p.logger != nil {
		p.logger.Info("fetching missing candles",
			"symbol", req.Symbol, "interval", req.Interval,
			"windows", len(toFetch), "provider", p.upstream.Name())
	}

	for _, window := range toFetch {
		sub := req
		sub.From, sub.To = window.From, window.To

		candles, err := p.upstream.Candles(ctx, sub)
		if err != nil {
			if p.logger != nil {
				p.logger.Warn("historical fetch failed",
					"symbol", req.Symbol, "from", window.From, "to", window.To, "err", err)
			}
			// Do not record coverage for a window we failed to fetch, or the
			// gap would be remembered as filled and never retried.
			continue
		}
		if len(candles) > 0 {
			if err := p.store.SaveCandles(ctx, candles); err != nil {
				if p.logger != nil {
					p.logger.Error("persist candles failed", "symbol", req.Symbol, "err", err)
				}
				continue
			}
		}
		// Record coverage even when the window returned nothing: an untraded
		// contract legitimately has no bars, and without this we would ask
		// again on every run forever.
		if err := p.store.AddCoverage(ctx, req.Symbol, string(req.Interval),
			p.upstream.Name(), window); err != nil && p.logger != nil {
			p.logger.Warn("record coverage failed", "symbol", req.Symbol, "err", err)
		}
	}

	// Mark the non-trading remainder of each gap as covered too, so weekends
	// and holidays are never reconsidered.
	for _, gap := range gaps {
		for _, closed := range p.calendar.ClosedWindows(gap) {
			if err := p.store.AddCoverage(ctx, req.Symbol, string(req.Interval),
				"closed", closed); err != nil && p.logger != nil {
				p.logger.Debug("record closed coverage failed", "err", err)
			}
		}
	}
}

// Subtract returns the parts of want not already covered by have.
//
// have must be sorted and merged, which storage.Coverage guarantees.
func Subtract(want storage.TimeRange, have []storage.TimeRange) []storage.TimeRange {
	var gaps []storage.TimeRange
	cursor := want.From

	for _, h := range have {
		if !h.To.After(cursor) {
			continue // entirely before the cursor
		}
		if !h.From.Before(want.To) {
			break // beyond the window
		}
		if h.From.After(cursor) {
			gaps = append(gaps, storage.TimeRange{From: cursor, To: min(h.From, want.To)})
		}
		if h.To.After(cursor) {
			cursor = h.To
		}
		if !cursor.Before(want.To) {
			return gaps
		}
	}

	if cursor.Before(want.To) {
		gaps = append(gaps, storage.TimeRange{From: cursor, To: want.To})
	}
	return gaps
}

func min(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
