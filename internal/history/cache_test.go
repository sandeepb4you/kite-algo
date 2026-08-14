package history

import (
	"context"
	"testing"
	"time"

	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, IST)
}

func rng(from, to time.Time) storage.TimeRange {
	return storage.TimeRange{From: from, To: to}
}

// TestSubtractFindsGaps is the heart of the cache: it decides what actually
// needs downloading. Getting it wrong either re-fetches everything (slow, and
// burns a metered quota) or silently skips data (a backtest with holes).
func TestSubtractFindsGaps(t *testing.T) {
	d := func(n int) time.Time { return day(2024, time.January, n) }

	tests := []struct {
		name string
		want storage.TimeRange
		have []storage.TimeRange
		gaps []storage.TimeRange
	}{
		{
			name: "nothing cached",
			want: rng(d(1), d(10)),
			gaps: []storage.TimeRange{rng(d(1), d(10))},
		},
		{
			name: "fully cached",
			want: rng(d(3), d(5)),
			have: []storage.TimeRange{rng(d(1), d(10))},
			gaps: nil,
		},
		{
			name: "cached prefix",
			want: rng(d(1), d(10)),
			have: []storage.TimeRange{rng(d(1), d(4))},
			gaps: []storage.TimeRange{rng(d(4), d(10))},
		},
		{
			name: "cached suffix",
			want: rng(d(1), d(10)),
			have: []storage.TimeRange{rng(d(6), d(12))},
			gaps: []storage.TimeRange{rng(d(1), d(6))},
		},
		{
			name: "hole in the middle",
			want: rng(d(1), d(10)),
			have: []storage.TimeRange{rng(d(1), d(3)), rng(d(7), d(10))},
			gaps: []storage.TimeRange{rng(d(3), d(7))},
		},
		{
			name: "two holes",
			want: rng(d(1), d(10)),
			have: []storage.TimeRange{rng(d(2), d(3)), rng(d(5), d(6))},
			gaps: []storage.TimeRange{rng(d(1), d(2)), rng(d(3), d(5)), rng(d(6), d(10))},
		},
		{
			name: "coverage entirely outside the window",
			want: rng(d(5), d(8)),
			have: []storage.TimeRange{rng(d(1), d(2)), rng(d(20), d(25))},
			gaps: []storage.TimeRange{rng(d(5), d(8))},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Subtract(tc.want, tc.have)
			if len(got) != len(tc.gaps) {
				t.Fatalf("got %d gaps %v, want %d %v", len(got), fmtRanges(got), len(tc.gaps), fmtRanges(tc.gaps))
			}
			for i := range got {
				if !got[i].From.Equal(tc.gaps[i].From) || !got[i].To.Equal(tc.gaps[i].To) {
					t.Errorf("gap %d = %s, want %s", i, fmtRange(got[i]), fmtRange(tc.gaps[i]))
				}
			}
		})
	}
}

func fmtRange(r storage.TimeRange) string {
	return r.From.Format("Jan02") + ".." + r.To.Format("Jan02")
}

func fmtRanges(rs []storage.TimeRange) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, fmtRange(r))
	}
	return out
}

// --- calendar -------------------------------------------------------------

func TestCalendarSkipsWeekends(t *testing.T) {
	c := NSE()
	// 2024-01-06 is a Saturday, 2024-01-07 a Sunday.
	if c.IsTradingDay(day(2024, time.January, 6)) {
		t.Error("Saturday reported as a trading day")
	}
	if c.IsTradingDay(day(2024, time.January, 7)) {
		t.Error("Sunday reported as a trading day")
	}
	if !c.IsTradingDay(day(2024, time.January, 8)) {
		t.Error("Monday should be a trading day")
	}
}

func TestCalendarHonoursHolidays(t *testing.T) {
	c := NSE()
	c.SetHolidays([]string{"2024-01-26"}) // Republic Day, a Friday
	if c.IsTradingDay(day(2024, time.January, 26)) {
		t.Error("configured holiday reported as a trading day")
	}
}

// TestTradingWindowsClipsToSessions ensures we never ask the API for overnight
// hours, which always return nothing but still cost rate-limit budget.
func TestTradingWindowsClipsToSessions(t *testing.T) {
	c := NSE()
	// Monday 8 Jan through Wednesday 10 Jan 2024.
	windows := c.TradingWindows(rng(day(2024, time.January, 8), day(2024, time.January, 11)))

	if len(windows) != 3 {
		t.Fatalf("got %d windows for 3 trading days: %v", len(windows), fmtRanges(windows))
	}
	for _, w := range windows {
		if w.From.Hour() != 9 || w.From.Minute() != 15 {
			t.Errorf("window starts at %s, want 09:15 IST", w.From.Format("15:04"))
		}
		if w.To.Hour() != 15 || w.To.Minute() != 30 {
			t.Errorf("window ends at %s, want 15:30 IST", w.To.Format("15:04"))
		}
	}
}

func TestTradingWindowsSkipsNonTradingDays(t *testing.T) {
	c := NSE()
	// Friday 5 Jan through Tuesday 9 Jan: the weekend must be excluded.
	windows := c.TradingWindows(rng(day(2024, time.January, 5), day(2024, time.January, 10)))
	if len(windows) != 3 { // Fri, Mon, Tue
		t.Fatalf("got %d windows, want 3 (weekend excluded): %v", len(windows), fmtRanges(windows))
	}
}

// --- tick aggregation -----------------------------------------------------

// TestAggregateVolumeIsADelta covers the subtle one. Tick.Volume is the day's
// CUMULATIVE traded quantity, so summing it would report a bar's volume as many
// times the day's entire turnover.
func TestAggregateVolumeIsADelta(t *testing.T) {
	base := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)
	ticks := []marketdata.Tick{
		{TradingSymbol: "X", LastPrice: 100, Volume: 1000, Timestamp: base},
		{TradingSymbol: "X", LastPrice: 102, Volume: 1500, Timestamp: base.Add(30 * time.Second)},
		// Next minute.
		{TradingSymbol: "X", LastPrice: 101, Volume: 1800, Timestamp: base.Add(70 * time.Second)},
		{TradingSymbol: "X", LastPrice: 103, Volume: 2200, Timestamp: base.Add(100 * time.Second)},
	}

	got := Aggregate(ticks, kite.IntervalMinute)
	if len(got) != 2 {
		t.Fatalf("got %d candles, want 2", len(got))
	}
	if got[0].Volume != 1500 {
		t.Errorf("first bar volume = %d, want 1500 (the cumulative reading at its close)", got[0].Volume)
	}
	if got[1].Volume != 700 {
		t.Errorf("second bar volume = %d, want 700 (2200-1500), not a sum of readings", got[1].Volume)
	}
}

func TestAggregateBuildsOHLC(t *testing.T) {
	base := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)
	ticks := []marketdata.Tick{
		{TradingSymbol: "X", LastPrice: 100, Timestamp: base},
		{TradingSymbol: "X", LastPrice: 105, Timestamp: base.Add(10 * time.Second)},
		{TradingSymbol: "X", LastPrice: 95, Timestamp: base.Add(20 * time.Second)},
		{TradingSymbol: "X", LastPrice: 102, Timestamp: base.Add(30 * time.Second)},
	}

	got := Aggregate(ticks, kite.IntervalMinute)
	if len(got) != 1 {
		t.Fatalf("got %d candles, want 1", len(got))
	}
	c := got[0]
	if c.Open != 100 || c.High != 105 || c.Low != 95 || c.Close != 102 {
		t.Errorf("OHLC = %v/%v/%v/%v, want 100/105/95/102", c.Open, c.High, c.Low, c.Close)
	}
	if !c.OpenTime.Equal(base) {
		t.Errorf("open time = %s, want %s", c.OpenTime, base)
	}
	if c.CloseTime.Sub(c.OpenTime) != time.Minute {
		t.Errorf("bar length = %s, want 1m", c.CloseTime.Sub(c.OpenTime))
	}
}

func TestAggregateIgnoresJunkTicks(t *testing.T) {
	base := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)
	ticks := []marketdata.Tick{
		{TradingSymbol: "X", LastPrice: 0, Timestamp: base},          // no price
		{TradingSymbol: "X", LastPrice: 100},                         // no timestamp
		{TradingSymbol: "X", LastPrice: 100, Timestamp: base.Add(1)}, // good
	}
	got := Aggregate(ticks, kite.IntervalMinute)
	if len(got) != 1 || got[0].Open != 100 {
		t.Errorf("got %+v, want one bar built from the single valid tick", got)
	}
}

func TestAggregateEmpty(t *testing.T) {
	if got := Aggregate(nil, kite.IntervalMinute); got != nil {
		t.Errorf("got %v, want nil for no ticks", got)
	}
}

// --- chain ----------------------------------------------------------------

type stubProvider struct {
	name    string
	candles []marketdata.Candle
	err     error
	calls   int
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Candles(context.Context, Request) ([]marketdata.Candle, error) {
	s.calls++
	return s.candles, s.err
}

// TestChainFallsBackToTicks covers the no-subscription path: if Kite refuses,
// candles built from recorded ticks are still better than nothing.
func TestChainFallsBackToTicks(t *testing.T) {
	primary := &stubProvider{name: "kite", err: context.DeadlineExceeded}
	fallback := &stubProvider{name: "ticks", candles: []marketdata.Candle{{Open: 1}}}

	chain := NewChain(primary, fallback)
	got, err := chain.Candles(context.Background(), Request{
		Symbol: "X", Interval: kite.IntervalMinute,
		From: day(2024, time.January, 1), To: day(2024, time.January, 2),
	})
	if err != nil {
		t.Fatalf("chain returned an error despite a working fallback: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d candles from the fallback, want 1", len(got))
	}
	if fallback.calls != 1 {
		t.Errorf("fallback called %d times, want 1", fallback.calls)
	}
}

func TestChainPrefersTheFirstProvider(t *testing.T) {
	primary := &stubProvider{name: "kite", candles: []marketdata.Candle{{Open: 1}}}
	fallback := &stubProvider{name: "ticks", candles: []marketdata.Candle{{Open: 2}}}

	chain := NewChain(primary, fallback)
	got, _ := chain.Candles(context.Background(), Request{
		Symbol: "X", Interval: kite.IntervalMinute,
		From: day(2024, time.January, 1), To: day(2024, time.January, 2),
	})
	if len(got) != 1 || got[0].Open != 1 {
		t.Errorf("got %+v, want the primary provider's data", got)
	}
	if fallback.calls != 0 {
		t.Error("fallback was consulted even though the primary succeeded")
	}
}

func TestRequestValidation(t *testing.T) {
	from := day(2024, time.January, 1)
	cases := map[string]Request{
		"no symbol":      {Interval: kite.IntervalMinute, From: from, To: from.AddDate(0, 0, 1)},
		"no interval":    {Symbol: "X", From: from, To: from.AddDate(0, 0, 1)},
		"to before from": {Symbol: "X", Interval: kite.IntervalMinute, From: from, To: from.AddDate(0, 0, -1)},
	}
	for name, req := range cases {
		if err := req.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
