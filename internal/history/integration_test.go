package history

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage"
	"kite-algo/internal/storage/sqlite"
)

// countingProvider records the windows it was asked for.
type countingProvider struct {
	name     string
	requests []storage.TimeRange
	perBar   time.Duration
}

func (c *countingProvider) Name() string { return c.name }

func (c *countingProvider) Candles(_ context.Context, req Request) ([]marketdata.Candle, error) {
	c.requests = append(c.requests, storage.TimeRange{From: req.From, To: req.To})

	step := c.perBar
	if step == 0 {
		step = req.Interval.Duration()
	}
	var out []marketdata.Candle
	for t := req.From; t.Before(req.To); t = t.Add(step) {
		out = append(out, marketdata.Candle{
			TradingSymbol: req.Symbol,
			Interval:      string(req.Interval),
			Open:          100, High: 101, Low: 99, Close: 100.5,
			Volume:   10,
			OpenTime: t, CloseTime: t.Add(step),
		})
	}
	return out, nil
}

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.New(context.Background(), filepath.Join(t.TempDir(), "hist.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestCacheDoesNotRefetch is the whole point of the caching layer. Kite meters
// historical requests and charges for the subscription, so a second backtest
// over the same window must hit storage rather than the API.
func TestCacheDoesNotRefetch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	upstream := &countingProvider{name: "fake"}
	cache := NewCacheProvider(store, upstream, nil)

	// Monday 8 Jan to Wednesday 10 Jan 2024 — three trading days.
	req := Request{
		Symbol:   "NIFTY24AUG24500CE",
		Interval: kite.Interval60Minute,
		From:     day(2024, time.January, 8),
		To:       day(2024, time.January, 11),
	}

	first, err := cache.Candles(ctx, req)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first fetch returned no candles")
	}
	firstCalls := len(upstream.requests)
	if firstCalls == 0 {
		t.Fatal("upstream was never called on a cold cache")
	}

	second, err := cache.Candles(ctx, req)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if len(upstream.requests) != firstCalls {
		t.Errorf("upstream called %d more times on a warm cache; caching is not working",
			len(upstream.requests)-firstCalls)
	}
	if len(second) != len(first) {
		t.Errorf("cached read returned %d candles, first fetch returned %d", len(second), len(first))
	}
}

// TestCacheFetchesOnlyTheGap covers an extended window: asking for more data
// must download only the new part.
func TestCacheFetchesOnlyTheGap(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	upstream := &countingProvider{name: "fake"}
	cache := NewCacheProvider(store, upstream, nil)

	base := Request{
		Symbol: "X", Interval: kite.Interval60Minute,
		From: day(2024, time.January, 8), To: day(2024, time.January, 10),
	}
	if _, err := cache.Candles(ctx, base); err != nil {
		t.Fatal(err)
	}
	upstream.requests = nil

	// Extend the window by two days.
	wider := base
	wider.To = day(2024, time.January, 12)
	if _, err := cache.Candles(ctx, wider); err != nil {
		t.Fatal(err)
	}

	if len(upstream.requests) == 0 {
		t.Fatal("extending the window fetched nothing")
	}
	// Nothing before the already-cached end should be requested again.
	cachedEnd := day(2024, time.January, 10)
	for _, r := range upstream.requests {
		if r.From.Before(cachedEnd) {
			t.Errorf("re-requested %s, which was already cached", fmtRange(r))
		}
	}
}

// TestCacheNeverRefetchesAClosedMarket is the reason the coverage table exists.
// A weekend legitimately has no candles; without recorded coverage, "no rows"
// is indistinguishable from "never asked" and the API gets hit forever.
func TestCacheNeverRefetchesAClosedMarket(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	upstream := &countingProvider{name: "fake"}
	cache := NewCacheProvider(store, upstream, nil)

	// Saturday 6 Jan and Sunday 7 Jan 2024 — the market is shut throughout.
	req := Request{
		Symbol: "X", Interval: kite.Interval60Minute,
		From: day(2024, time.January, 6), To: day(2024, time.January, 8),
	}

	if _, err := cache.Candles(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(upstream.requests) != 0 {
		t.Errorf("requested %d windows over a closed weekend: %v",
			len(upstream.requests), fmtRanges(upstream.requests))
	}

	// And a second pass must stay silent too.
	if _, err := cache.Candles(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(upstream.requests) != 0 {
		t.Error("a repeat request over a closed weekend reached the API")
	}
}

// TestCoverageMergesAdjacentWindows keeps the coverage table from fragmenting
// into thousands of touching rows over months of daily use.
func TestCoverageMergesAdjacentWindows(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	d := func(n int) time.Time { return day(2024, time.January, n) }
	for _, r := range []storage.TimeRange{
		{From: d(1), To: d(2)},
		{From: d(2), To: d(3)}, // adjacent
		{From: d(5), To: d(6)}, // separate
	} {
		if err := store.AddCoverage(ctx, "X", "minute", "test", r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.Coverage(ctx, "X", "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d coverage ranges %v, want 2 after merging adjacent windows",
			len(got), fmtRanges(got))
	}
	if !got[0].From.Equal(d(1)) || !got[0].To.Equal(d(3)) {
		t.Errorf("first range = %s, want Jan01..Jan03", fmtRange(got[0]))
	}
}

// TestSaveAndReadCandles round-trips through storage, including open interest,
// which the original schema lacked.
func TestSaveAndReadCandles(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	base := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)
	in := []marketdata.Candle{
		{TradingSymbol: "X", Interval: "minute", Open: 100, High: 105, Low: 99,
			Close: 103, Volume: 500, OpenInterest: 12345,
			OpenTime: base, CloseTime: base.Add(time.Minute)},
		{TradingSymbol: "X", Interval: "minute", Open: 103, High: 106, Low: 102,
			Close: 104, Volume: 600, OpenInterest: 12400,
			OpenTime: base.Add(time.Minute), CloseTime: base.Add(2 * time.Minute)},
	}
	if err := store.SaveCandles(ctx, in); err != nil {
		t.Fatalf("SaveCandles: %v", err)
	}

	got, err := store.GetCandles(ctx, "X", "minute", base, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("GetCandles: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candles, want 2", len(got))
	}
	if got[0].OpenInterest != 12345 {
		t.Errorf("open interest = %d, want 12345", got[0].OpenInterest)
	}
	if !got[0].OpenTime.Equal(base) {
		t.Errorf("open time round-tripped as %s, want %s", got[0].OpenTime, base)
	}
	// Ordering must be by open time so a backtest replays chronologically.
	if !got[0].OpenTime.Before(got[1].OpenTime) {
		t.Error("candles are not ordered by open time")
	}
}

// TestInstrumentSnapshotRoundTrip covers the data that becomes unrecoverable
// once contracts expire.
func TestInstrumentSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	asOf := day(2024, time.August, 1)
	expiry := day(2024, time.August, 8)
	rows := []storage.InstrumentRow{
		{InstrumentToken: 111, TradingSymbol: "NIFTY24AUG24500CE", Name: "NIFTY",
			Expiry: expiry, Strike: 24500, LotSize: 75, InstrumentType: "CE", Exchange: "NFO"},
		{InstrumentToken: 222, TradingSymbol: "NIFTY24AUG24500PE", Name: "NIFTY",
			Expiry: expiry, Strike: 24500, LotSize: 75, InstrumentType: "PE", Exchange: "NFO"},
	}

	if has, _ := store.HasInstrumentSnapshot(ctx, asOf); has {
		t.Fatal("a fresh database reports an existing snapshot")
	}
	if err := store.SaveInstrumentSnapshot(ctx, asOf, rows); err != nil {
		t.Fatalf("SaveInstrumentSnapshot: %v", err)
	}
	if has, _ := store.HasInstrumentSnapshot(ctx, asOf); !has {
		t.Error("snapshot not found after saving")
	}

	as, err := LoadAsOf(ctx, store, asOf)
	if err != nil {
		t.Fatalf("LoadAsOf: %v", err)
	}
	if as.Count() != 2 {
		t.Errorf("snapshot holds %d instruments, want 2", as.Count())
	}
	inst, ok := as.Lookup("NIFTY24AUG24500CE")
	if !ok {
		t.Fatal("expired contract could not be resolved from the snapshot")
	}
	if inst.LotSize != 75 || inst.Strike != 24500 {
		t.Errorf("resolved instrument = %+v", inst)
	}

	chain := as.Options("NIFTY", time.Time{})
	if len(chain) != 2 {
		t.Errorf("option chain has %d legs, want 2", len(chain))
	}
}

// TestLoadAsOfFallsBackToAnEarlierSnapshot means a backtest over a weekend or a
// day the server was down still resolves instruments, rather than looking like
// a strategy bug.
func TestLoadAsOfFallsBackToAnEarlierSnapshot(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if err := store.SaveInstrumentSnapshot(ctx, day(2024, time.August, 1),
		[]storage.InstrumentRow{{InstrumentToken: 1, TradingSymbol: "X"}}); err != nil {
		t.Fatal(err)
	}

	as, err := LoadAsOf(ctx, store, day(2024, time.August, 3)) // a Saturday
	if err != nil {
		t.Fatalf("LoadAsOf on a non-snapshot day: %v", err)
	}
	if as.Count() != 1 {
		t.Errorf("fallback returned %d instruments, want 1", as.Count())
	}
}

// TestLoadAsOfBeforeAnySnapshotFails is the honest failure: data that was never
// captured cannot be conjured, and the error should say so plainly.
func TestLoadAsOfBeforeAnySnapshotFails(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	if err := store.SaveInstrumentSnapshot(ctx, day(2024, time.August, 1),
		[]storage.InstrumentRow{{InstrumentToken: 1, TradingSymbol: "X"}}); err != nil {
		t.Fatal(err)
	}

	_, err := LoadAsOf(ctx, store, day(2023, time.January, 1))
	if err == nil {
		t.Fatal("expected an error for a date before any snapshot")
	}
}

// A multi-day gap must cost ONE upstream request, not one per trading day.
//
// Splitting per day multiplied every backfill by the number of days in it: a
// 30-day lookback became 22 sequential round trips per contract, which turned a
// few-hundred-request capture into ~14,000 and a twenty-minute job into six
// hours. Kite caps a request's span itself and returns nothing for the closed
// hours inside it, so the split bought nothing.
func TestCacheCoalescesAMultiDayGapIntoOneRequest(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	upstream := &countingProvider{name: "fake"}
	cache := NewCacheProvider(store, upstream, nil)

	// Six weeks spanning many weekends: 6 July → 14 August 2026.
	_, err := cache.Candles(ctx, Request{
		Symbol:   "NIFTY2681824350CE",
		Interval: kite.Interval5Minute,
		From:     day(2026, time.July, 6),
		To:       time.Date(2026, 8, 14, 15, 30, 0, 0, IST),
	})
	if err != nil {
		t.Fatalf("Candles: %v", err)
	}

	if n := len(upstream.requests); n != 1 {
		t.Fatalf("made %d upstream requests for one contiguous gap, want 1", n)
	}
	got := upstream.requests[0]
	if got.From.Hour() != 9 || got.From.Minute() != 15 {
		t.Errorf("request starts %s, want a 09:15 session open",
			got.From.Format("2006-01-02 15:04"))
	}
	if got.To.Hour() != 15 || got.To.Minute() != 30 {
		t.Errorf("request ends %s, want a 15:30 session close",
			got.To.Format("2006-01-02 15:04"))
	}
	// It must still cover the far end of the range, not just the first day.
	if got.To.Before(time.Date(2026, 8, 14, 0, 0, 0, 0, IST)) {
		t.Errorf("request ends %s, well short of the requested window", got.To)
	}
}

// The optimisation worth keeping: a span the exchange was shut for entirely is
// never requested, because the response is guaranteed empty.
func TestCacheSkipsAClosedOnlyGap(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	upstream := &countingProvider{name: "fake"}
	cache := NewCacheProvider(store, upstream, nil)

	// Saturday 15 and Sunday 16 August 2026.
	if _, err := cache.Candles(ctx, Request{
		Symbol:   "X",
		Interval: kite.Interval5Minute,
		From:     day(2026, time.August, 15),
		To:       day(2026, time.August, 17),
	}); err != nil {
		t.Fatalf("Candles: %v", err)
	}
	if n := len(upstream.requests); n != 0 {
		t.Errorf("requested %d windows for a closed weekend, want 0", n)
	}
}
