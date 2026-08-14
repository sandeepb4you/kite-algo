package backtest

import (
	"context"
	"fmt"
	"sort"
	"time"

	"kite-algo/internal/history"
	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
)

// BarPath is how one candle is expanded into a sequence of prices.
//
// A candle records only four prices and no ordering between the high and the
// low, so any expansion is an assumption. The choice matters most for stops: a
// bar that touched both a stop and a target genuinely could have hit either
// first, and picking the favourable order is how a backtest flatters itself.
type BarPath string

const (
	// PathPessimist assumes the adverse extreme came first: for an up bar
	// O→L→H→C, for a down bar O→H→L→C. This is the default because when the
	// data cannot say, a backtest should not assume the lucky ordering.
	PathPessimist BarPath = "pessimist"
	// PathOHLC always walks O→H→L→C.
	PathOHLC BarPath = "ohlc"
	// PathCloseOnly emits one price per bar. Fastest, least faithful; intrabar
	// stops cannot trigger at all.
	PathCloseOnly BarPath = "close"
)

// Event is one simulated market-data update.
type Event struct {
	Time       time.Time
	Tick       marketdata.Tick
	IsBarClose bool
}

// Feed produces the event stream a backtest replays.
type Feed interface {
	// Add attaches symbols mid-run, loading their data from the current
	// simulated time onward.
	Add(ctx context.Context, symbols ...string) error
	// Next returns the chronologically next event.
	Next() (Event, bool)
	// Progress reports completion between 0 and 1.
	Progress() float64
}

// CandleFeed replays stored candles as synthetic ticks.
//
// Symbols can be attached at any point during the run, which is not a
// convenience — it is required. A strategy like the short straddle only learns
// which option legs it wants after seeing a spot price, so it calls Subscribe
// mid-run. A feed that loaded everything upfront could not backtest it at all.
type CandleFeed struct {
	provider history.Provider
	interval kite.Interval
	from, to time.Time
	path     BarPath
	clock    *SimClock

	// pending is the merged, time-ordered event queue.
	pending []Event
	cursor  int
	loaded  map[string]bool
	total   time.Duration
}

// FeedConfig configures a CandleFeed.
type FeedConfig struct {
	Provider history.Provider
	Interval kite.Interval
	From     time.Time
	To       time.Time
	Path     BarPath
	Clock    *SimClock
	Symbols  []string
}

// NewCandleFeed builds a feed and loads its seed symbols.
func NewCandleFeed(ctx context.Context, cfg FeedConfig) (*CandleFeed, error) {
	if cfg.Provider == nil {
		return nil, fmt.Errorf("backtest: feed needs a history provider")
	}
	if !cfg.To.After(cfg.From) {
		return nil, fmt.Errorf("backtest: 'to' must be after 'from'")
	}
	path := cfg.Path
	if path == "" {
		path = PathPessimist
	}

	f := &CandleFeed{
		provider: cfg.Provider,
		interval: cfg.Interval,
		from:     cfg.From,
		to:       cfg.To,
		path:     path,
		clock:    cfg.Clock,
		loaded:   make(map[string]bool),
		total:    cfg.To.Sub(cfg.From),
	}
	if err := f.Add(ctx, cfg.Symbols...); err != nil {
		return nil, err
	}
	return f, nil
}

// Add loads symbols and merges their events into the queue.
//
// Events already in the past are discarded: a symbol attached at 11:00 must not
// deliver the morning's bars, or the strategy would react to prices it could not
// have seen at the moment it subscribed. That is lookahead bias, and it is the
// single easiest way to build a backtest that cannot be reproduced live.
func (f *CandleFeed) Add(ctx context.Context, symbols ...string) error {
	var fresh []Event

	for _, sym := range symbols {
		if sym == "" || f.loaded[sym] {
			continue
		}
		f.loaded[sym] = true

		candles, err := f.provider.Candles(ctx, history.Request{
			Symbol:   sym,
			Interval: f.interval,
			From:     f.from,
			To:       f.to,
		})
		if err != nil {
			return fmt.Errorf("backtest: load %s: %w", sym, err)
		}

		cutoff := f.from
		if f.clock != nil {
			if now := f.clock.Now(); now.After(cutoff) {
				cutoff = now
			}
		}

		for _, c := range candles {
			if c.OpenTime.Before(cutoff) {
				continue
			}
			fresh = append(fresh, f.expand(c)...)
		}
	}

	if len(fresh) == 0 {
		return nil
	}

	// Merge the un-consumed remainder with the new events and re-sort. Re-sorting
	// is cheap relative to a run and keeps the ordering rule in exactly one place.
	remaining := append([]Event(nil), f.pending[f.cursor:]...)
	merged := append(remaining, fresh...)
	sortEvents(merged)

	f.pending = merged
	f.cursor = 0
	return nil
}

// sortEvents imposes a total order. Ties are broken by symbol and then by the
// position within the bar, so two instruments printing at the same instant are
// always delivered in the same order — the run must not depend on map iteration
// or slice arrival order.
func sortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].Time.Equal(events[j].Time) {
			return events[i].Time.Before(events[j].Time)
		}
		return events[i].Tick.TradingSymbol < events[j].Tick.TradingSymbol
	})
}

// expand turns one candle into its price path.
func (f *CandleFeed) expand(c marketdata.Candle) []Event {
	dur := c.CloseTime.Sub(c.OpenTime)
	if dur <= 0 {
		dur = f.interval.Duration()
	}

	prices := f.pathPrices(c)
	events := make([]Event, 0, len(prices))

	for i, p := range prices {
		// Space the points across the bar, keeping the close strictly inside it
		// so it can never collide with the next bar's open.
		offset := time.Duration(float64(dur) * float64(i) / float64(len(prices)))
		if i == len(prices)-1 {
			offset = dur - time.Nanosecond
		}
		last := i == len(prices)-1

		events = append(events, Event{
			Time:       c.OpenTime.Add(offset),
			IsBarClose: last,
			Tick: marketdata.Tick{
				InstrumentToken: c.InstrumentToken,
				TradingSymbol:   c.TradingSymbol,
				LastPrice:       p,
				// Volume is attributed entirely to the close: a bar reports one
				// figure, and splitting it across synthetic points would invent
				// detail the data does not contain.
				Volume:    volumeAt(c, last),
				OHLC:      marketdata.OHLC{Open: c.Open, High: c.High, Low: c.Low, Close: c.Close},
				Timestamp: c.OpenTime.Add(offset),
			},
		})
	}
	return events
}

func volumeAt(c marketdata.Candle, isClose bool) int64 {
	if isClose {
		return c.Volume
	}
	return 0
}

// pathPrices returns the price sequence for one bar.
func (f *CandleFeed) pathPrices(c marketdata.Candle) []float64 {
	switch f.path {
	case PathCloseOnly:
		return []float64{c.Close}
	case PathOHLC:
		return []float64{c.Open, c.High, c.Low, c.Close}
	default: // PathPessimist
		if c.Close >= c.Open {
			// An up bar: assume it dipped to the low before rallying.
			return []float64{c.Open, c.Low, c.High, c.Close}
		}
		// A down bar: assume it popped to the high before falling.
		return []float64{c.Open, c.High, c.Low, c.Close}
	}
}

// Next returns the next event in time order.
func (f *CandleFeed) Next() (Event, bool) {
	if f.cursor >= len(f.pending) {
		return Event{}, false
	}
	e := f.pending[f.cursor]
	f.cursor++
	return e, true
}

// Progress reports how far through the window the feed has advanced.
func (f *CandleFeed) Progress() float64 {
	if f.total <= 0 || f.cursor == 0 || f.cursor > len(f.pending) {
		return 0
	}
	elapsed := f.pending[f.cursor-1].Time.Sub(f.from)
	p := float64(elapsed) / float64(f.total)
	if p > 1 {
		return 1
	}
	if p < 0 {
		return 0
	}
	return p
}

// Symbols reports which instruments the feed has loaded.
func (f *CandleFeed) Symbols() []string {
	out := make([]string, 0, len(f.loaded))
	for s := range f.loaded {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
