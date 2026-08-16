package history

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage"
)

// Capturer downloads and stores option candles before the contracts expire.
//
// The whole point is the asymmetry in what Kite sells. Candles for a live
// contract can be re-fetched at any time; candles for an expired one cannot be
// fetched at all, because the historical endpoint is keyed by instrument_token
// and expired tokens leave the instrument master for good. So this runs daily,
// and each run is the last chance to record that day for every contract
// expiring before the next one.
//
// It writes through CacheProvider rather than calling Kite directly. That is
// what makes a missed day self-healing: coverage records what has already been
// stored, so a run asking for the last 30 days fetches only the parts still
// absent. A machine switched off for a week catches up on the contracts that
// are still alive, and loses only those that expired while it was down.
type Capturer struct {
	provider    Provider
	instruments *kite.Instruments
	calendar    *Calendar
	logger      *slog.Logger
	opts        CaptureOptions
}

// CaptureOptions configures a capture run.
type CaptureOptions struct {
	// Interval is the candle interval to store.
	Interval kite.Interval

	// Strikes is how many strikes to take on each side of the day's traded
	// range. The window is centred on the range rather than a single price
	// because a strategy entering at 09:15 uses a different ATM strike than one
	// entering at 15:00, and a backtest needs both.
	Strikes int

	// Expiries is how many expiries deep to capture, nearest first.
	Expiries int

	// Lookback is how far back to reach for a contract with no coverage yet.
	Lookback time.Duration

	// Underlyings are the chains to capture.
	Underlyings []CaptureTarget
}

// CaptureTarget names one option chain and the index that prices it.
type CaptureTarget struct {
	Underlying string // "NIFTY", "SENSEX"
	Index      string // "NIFTY 50", "SENSEX"
}

// CaptureReport summarises what a run did.
type CaptureReport struct {
	Day        time.Time
	Underlying []UnderlyingReport
	Contracts  int
	Candles    int
	Failures   int
	Skipped    string // non-empty when the whole run was skipped, with the reason
	Duration   time.Duration
}

// UnderlyingReport is the per-chain slice of a run.
type UnderlyingReport struct {
	Underlying string
	Spot       float64
	Low, High  float64
	Expiries   []time.Time
	Contracts  int
	Candles    int
	Failures   int
	Err        string
}

// NewCapturer builds a capturer over a provider and instrument master.
func NewCapturer(provider Provider, instruments *kite.Instruments, cal *Calendar, opts CaptureOptions, logger *slog.Logger) *Capturer {
	if opts.Interval == "" {
		opts.Interval = kite.Interval5Minute
	}
	if opts.Strikes <= 0 {
		opts.Strikes = 20
	}
	if opts.Expiries <= 0 {
		opts.Expiries = 4
	}
	if opts.Lookback <= 0 {
		opts.Lookback = 30 * 24 * time.Hour
	}
	if cal == nil {
		cal = NSE()
	}
	return &Capturer{
		provider:    provider,
		instruments: instruments,
		calendar:    cal,
		logger:      logger,
		opts:        opts,
	}
}

// CaptureDay captures every configured chain for one trading day.
//
// Weekends and configured holidays return immediately with Skipped set. That is
// checked here rather than only in the scheduler so that a manual trigger,
// a catch-up run, and the timer all agree on what a trading day is.
func (c *Capturer) CaptureDay(ctx context.Context, day time.Time) (CaptureReport, error) {
	started := time.Now()
	rep := CaptureReport{Day: day.In(IST)}

	if c.provider == nil {
		return rep, fmt.Errorf("capture: no history provider")
	}
	if c.instruments == nil {
		return rep, fmt.Errorf("capture: no instrument master; log in first")
	}
	if !c.calendar.IsTradingDay(day) {
		rep.Skipped = "not a trading day"
		rep.Duration = time.Since(started)
		return rep, nil
	}

	session, ok := c.calendar.SessionFor(day)
	if !ok {
		rep.Skipped = "no session"
		rep.Duration = time.Since(started)
		return rep, nil
	}

	for _, target := range c.opts.Underlyings {
		select {
		case <-ctx.Done():
			rep.Duration = time.Since(started)
			return rep, ctx.Err()
		default:
		}

		ur := c.captureUnderlying(ctx, target, session)
		rep.Underlying = append(rep.Underlying, ur)
		rep.Contracts += ur.Contracts
		rep.Candles += ur.Candles
		rep.Failures += ur.Failures
	}

	rep.Duration = time.Since(started)
	if c.logger != nil {
		c.logger.Info("option capture complete",
			"day", rep.Day.Format("2006-01-02"),
			"contracts", rep.Contracts, "candles", rep.Candles,
			"failures", rep.Failures, "took", rep.Duration.Round(time.Second))
	}
	return rep, nil
}

// captureUnderlying captures one chain: index first, then the strike window
// across the nearest expiries.
func (c *Capturer) captureUnderlying(ctx context.Context, target CaptureTarget, session storage.TimeRange) UnderlyingReport {
	ur := UnderlyingReport{Underlying: target.Underlying}

	// The index is captured first and for its own sake — a backtest of an
	// index-driven strategy needs the spot series, and it doubles as the
	// reference that positions the strike window.
	idx, err := c.captureSymbol(ctx, target.Index, 0, session)
	if err != nil {
		ur.Err = fmt.Sprintf("index %s: %v", target.Index, err)
		ur.Failures++
		if c.logger != nil {
			c.logger.Warn("capture: index fetch failed",
				"index", target.Index, "underlying", target.Underlying, "err", err)
		}
		return ur
	}
	ur.Contracts++
	ur.Candles += len(idx)

	// Position the window on THIS DAY's range, not the lookback's.
	//
	// captureSymbol returns everything back to the lookback horizon, because the
	// index series is worth storing in full. Measuring the strike window against
	// all of it silently widened the window to the index's 30-day range — a
	// ~900-point swing on NIFTY instead of a ~50-point one, so ~58 strikes per
	// expiry got captured where 42 were configured, at ~40% more requests than
	// the operator asked for.
	low, high, last, ok := rangeOf(candlesWithin(idx, session))
	if !ok {
		ur.Err = fmt.Sprintf("no %s candles on this day; cannot locate ATM", target.Index)
		if c.logger != nil {
			c.logger.Warn("capture: no index candles, skipping chain",
				"index", target.Index, "underlying", target.Underlying)
		}
		return ur
	}
	ur.Spot, ur.Low, ur.High = last, low, high

	expiries := c.instruments.Expiries(target.Underlying, session.From)
	if len(expiries) > c.opts.Expiries {
		expiries = expiries[:c.opts.Expiries]
	}
	if len(expiries) == 0 {
		ur.Err = fmt.Sprintf("no live expiries for %s in the instrument master", target.Underlying)
		if c.logger != nil {
			c.logger.Warn("capture: no expiries found; is the right exchange loaded?",
				"underlying", target.Underlying)
		}
		return ur
	}
	ur.Expiries = expiries

	for _, expiry := range expiries {
		select {
		case <-ctx.Done():
			return ur
		default:
		}

		chain := c.instruments.Chain(target.Underlying, expiry)
		for _, inst := range selectStrikes(chain, low, high, c.opts.Strikes) {
			candles, err := c.captureSymbol(ctx, inst.TradingSymbol, inst.InstrumentToken, session)
			if err != nil {
				ur.Failures++
				if c.logger != nil {
					c.logger.Warn("capture: contract fetch failed",
						"symbol", inst.TradingSymbol, "err", err)
				}
				continue
			}
			ur.Contracts++
			ur.Candles += len(candles)
		}
	}

	if c.logger != nil {
		c.logger.Info("captured chain",
			"underlying", target.Underlying, "spot", ur.Spot,
			"range", fmt.Sprintf("%.0f-%.0f", low, high),
			"expiries", len(expiries), "contracts", ur.Contracts,
			"candles", ur.Candles, "failures", ur.Failures)
	}
	return ur
}

// captureSymbol fetches one instrument through the cache, which stores whatever
// it had to download.
//
// The window starts at the lookback horizon rather than at the day being
// captured. Coverage makes the extra span nearly free after the first sighting,
// and on that first sighting it pulls down the contract's life so far — the one
// kind of backfill Kite still allows, since the contract has not expired yet.
func (c *Capturer) captureSymbol(ctx context.Context, symbol string, token uint32, session storage.TimeRange) ([]marketdata.Candle, error) {
	from := session.From.Add(-c.opts.Lookback)
	return c.provider.Candles(ctx, Request{
		Symbol:   symbol,
		Token:    token,
		Interval: c.opts.Interval,
		From:     from,
		To:       session.To,
	})
}

// candlesWithin returns the candles whose open time falls inside a window.
func candlesWithin(candles []marketdata.Candle, w storage.TimeRange) []marketdata.Candle {
	out := make([]marketdata.Candle, 0, len(candles))
	for _, c := range candles {
		if c.OpenTime.Before(w.From) || !c.OpenTime.Before(w.To) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// rangeOf returns the low, high and last traded price of a candle series.
func rangeOf(candles []marketdata.Candle) (low, high, last float64, ok bool) {
	for _, c := range candles {
		if c.Low <= 0 && c.High <= 0 {
			continue
		}
		if !ok {
			low, high = c.Low, c.High
			ok = true
		}
		if c.Low > 0 && c.Low < low {
			low = c.Low
		}
		if c.High > high {
			high = c.High
		}
		if c.Close > 0 {
			last = c.Close
		}
	}
	return low, high, last, ok
}

// selectStrikes picks the contracts within `n` strikes of the day's traded
// range, on both sides, both CE and PE.
//
// The grid is read off the instrument master rather than configured. NIFTY
// trades a 50-point grid and SENSEX a 100-point one, and both have been changed
// by the exchange before; deriving the ladder from the contracts that actually
// exist means a grid change is absorbed silently instead of quietly capturing
// the wrong strikes.
func selectStrikes(chain []kite.Instrument, low, high float64, n int) []kite.Instrument {
	if len(chain) == 0 || n <= 0 {
		return nil
	}

	// Distinct sorted strike ladder for this expiry.
	seen := make(map[float64]struct{})
	var ladder []float64
	for _, inst := range chain {
		if !inst.IsOption() || inst.Strike <= 0 {
			continue
		}
		if _, dup := seen[inst.Strike]; dup {
			continue
		}
		seen[inst.Strike] = struct{}{}
		ladder = append(ladder, inst.Strike)
	}
	if len(ladder) == 0 {
		return nil
	}
	sort.Float64s(ladder)

	// Widen from the strike nearest the low to the strike nearest the high, so
	// an intraday move does not push the morning's ATM out of the window.
	lo := nearestIndex(ladder, low) - n
	hi := nearestIndex(ladder, high) + n
	if lo < 0 {
		lo = 0
	}
	if hi > len(ladder)-1 {
		hi = len(ladder) - 1
	}

	wanted := make(map[float64]struct{}, hi-lo+1)
	for i := lo; i <= hi; i++ {
		wanted[ladder[i]] = struct{}{}
	}

	out := make([]kite.Instrument, 0, len(wanted)*2)
	for _, inst := range chain {
		if !inst.IsOption() {
			continue
		}
		if _, want := wanted[inst.Strike]; want {
			out = append(out, inst)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Strike != out[j].Strike {
			return out[i].Strike < out[j].Strike
		}
		return out[i].InstrumentType < out[j].InstrumentType
	})
	return out
}

// nearestIndex returns the index of the ladder entry closest to price.
func nearestIndex(ladder []float64, price float64) int {
	best, bestDist := 0, math.Abs(ladder[0]-price)
	for i := 1; i < len(ladder); i++ {
		if d := math.Abs(ladder[i] - price); d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}
