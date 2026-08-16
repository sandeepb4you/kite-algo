package history

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
)

// chainFor builds a synthetic option chain on a given strike grid.
func chainFor(underlying string, expiry time.Time, from, to, step float64) []kite.Instrument {
	var out []kite.Instrument
	for s := from; s <= to; s += step {
		for _, typ := range []string{"CE", "PE"} {
			out = append(out, kite.Instrument{
				TradingSymbol:  fmt.Sprintf("%s%.0f%s", underlying, s, typ),
				Name:           underlying,
				Expiry:         expiry,
				Strike:         s,
				InstrumentType: typ,
				Exchange:       "NFO",
			})
		}
	}
	return out
}

func TestSelectStrikesCoversRequestedDepthBothSides(t *testing.T) {
	chain := chainFor("NIFTY", time.Now(), 20000, 30000, 50)

	// A flat day: low == high, so the window is symmetric around one strike.
	got := selectStrikes(chain, 24350, 24350, 20)

	// 20 either side plus the centre = 41 strikes, CE and PE each.
	if want := 41 * 2; len(got) != want {
		t.Fatalf("got %d contracts, want %d", len(got), want)
	}
	lo, hi := got[0].Strike, got[len(got)-1].Strike
	if lo != 24350-20*50 {
		t.Errorf("lowest strike %v, want %v", lo, 24350-20*50)
	}
	if hi != 24350+20*50 {
		t.Errorf("highest strike %v, want %v", hi, 24350+20*50)
	}
}

// A strategy entering at 09:15 uses the morning's ATM, not the close's. If the
// window were centred on a single price, a trending day would leave the
// morning's at-the-money strike uncaptured and unbacktestable.
func TestSelectStrikesSpansTheDaysTradedRange(t *testing.T) {
	chain := chainFor("NIFTY", time.Now(), 20000, 30000, 50)

	got := selectStrikes(chain, 24000, 24600, 20)

	strikes := map[float64]bool{}
	for _, c := range got {
		strikes[c.Strike] = true
	}
	// Both ends of the move, widened by 20 strikes each way.
	for _, want := range []float64{24000 - 20*50, 24000, 24300, 24600, 24600 + 20*50} {
		if !strikes[want] {
			t.Errorf("strike %v not captured; window did not span the day's range", want)
		}
	}
}

// SENSEX trades a 100-point grid, NIFTY a 50-point one. Deriving the ladder
// from the contracts that exist means neither is hard-coded.
func TestSelectStrikesDerivesGridFromChain(t *testing.T) {
	chain := chainFor("SENSEX", time.Now(), 70000, 90000, 100)

	got := selectStrikes(chain, 80000, 80000, 10)

	if want := 21 * 2; len(got) != want {
		t.Fatalf("got %d contracts, want %d", len(got), want)
	}
	if lo := got[0].Strike; lo != 80000-10*100 {
		t.Errorf("lowest strike %v, want %v — grid not read from the chain", lo, 80000-10*100)
	}
}

func TestSelectStrikesClampsAtChainEdges(t *testing.T) {
	chain := chainFor("NIFTY", time.Now(), 24000, 24500, 50)

	// Spot sits at the very bottom of a short ladder.
	got := selectStrikes(chain, 24000, 24000, 20)

	if len(got) != 11*2 {
		t.Fatalf("got %d contracts, want %d (whole ladder, no panic)", len(got), 11*2)
	}
}

func TestSelectStrikesEmptyChain(t *testing.T) {
	if got := selectStrikes(nil, 100, 100, 20); got != nil {
		t.Errorf("got %v, want nil for an empty chain", got)
	}
}

// captureStub records what was asked for and returns canned candles.
type captureStub struct {
	asked   []Request
	candles map[string][]marketdata.Candle
	err     map[string]error
}

func (p *captureStub) Name() string { return "stub" }

func (p *captureStub) Candles(_ context.Context, req Request) ([]marketdata.Candle, error) {
	p.asked = append(p.asked, req)
	if err := p.err[req.Symbol]; err != nil {
		return nil, err
	}
	return p.candles[req.Symbol], nil
}

// indexCandles builds a bar series inside the trading session of day.
//
// Anchored to the 09:15 open rather than the caller's clock time: bars outside
// the session are filtered out when sizing the strike window, so a fixture
// stamped at, say, 16:00 would silently produce an empty range and test nothing.
func indexCandles(day time.Time, prices ...float64) []marketdata.Candle {
	local := day.In(IST)
	open := time.Date(local.Year(), local.Month(), local.Day(), 9, 15, 0, 0, IST)

	var out []marketdata.Candle
	for i, p := range prices {
		out = append(out, marketdata.Candle{
			Open: p, High: p + 10, Low: p - 10, Close: p,
			OpenTime: open.Add(time.Duration(i) * 5 * time.Minute),
		})
	}
	return out
}

func testInstruments(t *testing.T, insts []kite.Instrument) *kite.Instruments {
	t.Helper()
	csv := "instrument_token,tradingsymbol,name,expiry,strike,lot_size,instrument_type,segment,exchange,tick_size\n"
	for i, in := range insts {
		csv += fmt.Sprintf("%d,%s,%s,%s,%v,%d,%s,%s,%s,0.05\n",
			i+1000, in.TradingSymbol, in.Name, in.Expiry.Format("2006-01-02"),
			in.Strike, 75, in.InstrumentType, "NFO-OPT", in.Exchange)
	}
	m, err := kite.ParseInstruments(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse instruments: %v", err)
	}
	return m
}

func TestCaptureDaySkipsWeekends(t *testing.T) {
	// 2026-08-15 is a Saturday.
	saturday := time.Date(2026, 8, 15, 16, 0, 0, 0, IST)
	c := NewCapturer(&captureStub{}, testInstruments(t, nil), NSE(), CaptureOptions{}, nil)

	rep, err := c.CaptureDay(context.Background(), saturday)
	if err != nil {
		t.Fatalf("CaptureDay: %v", err)
	}
	if rep.Skipped == "" {
		t.Error("Saturday was captured; weekends must be skipped")
	}
	if rep.Contracts != 0 {
		t.Errorf("got %d contracts on a Saturday, want 0", rep.Contracts)
	}
}

func TestCaptureDaySkipsConfiguredHolidays(t *testing.T) {
	// 2026-08-14 is a Friday; declare it a holiday.
	friday := time.Date(2026, 8, 14, 16, 0, 0, 0, IST)
	cal := NSE()
	cal.SetHolidays([]string{"2026-08-14"})
	c := NewCapturer(&captureStub{}, testInstruments(t, nil), cal, CaptureOptions{}, nil)

	rep, err := c.CaptureDay(context.Background(), friday)
	if err != nil {
		t.Fatalf("CaptureDay: %v", err)
	}
	if rep.Skipped == "" {
		t.Error("a configured holiday was captured")
	}
}

func TestCaptureDayFetchesIndexAndBothSidesOfTheChain(t *testing.T) {
	friday := time.Date(2026, 8, 14, 16, 0, 0, 0, IST)
	expiry := time.Date(2026, 8, 18, 0, 0, 0, 0, IST)
	chain := chainFor("NIFTY", expiry, 24000, 24700, 50)

	prov := &captureStub{candles: map[string][]marketdata.Candle{
		"NIFTY 50": indexCandles(friday, 24350, 24400),
	}}
	c := NewCapturer(prov, testInstruments(t, chain), NSE(), CaptureOptions{
		Strikes:     2,
		Expiries:    1,
		Underlyings: []CaptureTarget{{Underlying: "NIFTY", Index: "NIFTY 50"}},
	}, nil)

	rep, err := c.CaptureDay(context.Background(), friday)
	if err != nil {
		t.Fatalf("CaptureDay: %v", err)
	}
	if rep.Skipped != "" {
		t.Fatalf("unexpectedly skipped: %s", rep.Skipped)
	}

	var ce, pe, idx int
	for _, req := range prov.asked {
		switch {
		case req.Symbol == "NIFTY 50":
			idx++
		case len(req.Symbol) > 2 && req.Symbol[len(req.Symbol)-2:] == "CE":
			ce++
		case len(req.Symbol) > 2 && req.Symbol[len(req.Symbol)-2:] == "PE":
			pe++
		}
	}
	if idx != 1 {
		t.Errorf("index fetched %d times, want 1", idx)
	}
	if ce == 0 || pe == 0 {
		t.Errorf("got %d CE and %d PE; both sides must be captured", ce, pe)
	}
	if ce != pe {
		t.Errorf("asymmetric capture: %d CE vs %d PE", ce, pe)
	}
}

// A contract that cannot be fetched must not abort the rest of the chain: one
// illiquid strike would otherwise cost the whole day's capture.
func TestCaptureDayContinuesPastAFailedContract(t *testing.T) {
	friday := time.Date(2026, 8, 14, 16, 0, 0, 0, IST)
	expiry := time.Date(2026, 8, 18, 0, 0, 0, 0, IST)
	chain := chainFor("NIFTY", expiry, 24300, 24400, 50)

	prov := &captureStub{
		candles: map[string][]marketdata.Candle{"NIFTY 50": indexCandles(friday, 24350)},
		err:     map[string]error{"NIFTY24350CE": fmt.Errorf("boom")},
	}
	c := NewCapturer(prov, testInstruments(t, chain), NSE(), CaptureOptions{
		Strikes:     5,
		Expiries:    1,
		Underlyings: []CaptureTarget{{Underlying: "NIFTY", Index: "NIFTY 50"}},
	}, nil)

	rep, err := c.CaptureDay(context.Background(), friday)
	if err != nil {
		t.Fatalf("CaptureDay: %v", err)
	}
	if rep.Failures != 1 {
		t.Errorf("got %d failures, want 1", rep.Failures)
	}
	if rep.Contracts < 5 {
		t.Errorf("only %d contracts captured; one failure aborted the chain", rep.Contracts)
	}
}

// Without index candles there is no way to know which strikes were near the
// money, and capturing an arbitrary slice of the chain would be worse than
// capturing none — it would look like data.
func TestCaptureDaySkipsChainWhenIndexHasNoCandles(t *testing.T) {
	friday := time.Date(2026, 8, 14, 16, 0, 0, 0, IST)
	expiry := time.Date(2026, 8, 18, 0, 0, 0, 0, IST)
	chain := chainFor("NIFTY", expiry, 24000, 24700, 50)

	prov := &captureStub{candles: map[string][]marketdata.Candle{}}
	c := NewCapturer(prov, testInstruments(t, chain), NSE(), CaptureOptions{
		Strikes:     2,
		Expiries:    1,
		Underlyings: []CaptureTarget{{Underlying: "NIFTY", Index: "NIFTY 50"}},
	}, nil)

	rep, err := c.CaptureDay(context.Background(), friday)
	if err != nil {
		t.Fatalf("CaptureDay: %v", err)
	}
	if len(prov.asked) != 1 {
		t.Errorf("fetched %d symbols with no spot reference, want 1 (the index only)", len(prov.asked))
	}
	if rep.Underlying[0].Err == "" {
		t.Error("no error recorded for a chain that could not be positioned")
	}
}

// The lookback is what recovers a still-live contract's past. Requesting only
// the current day would leave months of obtainable history on the table.
func TestCaptureReachesBackByLookback(t *testing.T) {
	friday := time.Date(2026, 8, 14, 16, 0, 0, 0, IST)
	prov := &captureStub{candles: map[string][]marketdata.Candle{
		"NIFTY 50": indexCandles(friday, 24350),
	}}
	c := NewCapturer(prov, testInstruments(t, nil), NSE(), CaptureOptions{
		Lookback:    30 * 24 * time.Hour,
		Underlyings: []CaptureTarget{{Underlying: "NIFTY", Index: "NIFTY 50"}},
	}, nil)

	if _, err := c.CaptureDay(context.Background(), friday); err != nil {
		t.Fatalf("CaptureDay: %v", err)
	}
	if len(prov.asked) == 0 {
		t.Fatal("nothing requested")
	}
	req := prov.asked[0]
	sessionOpen := time.Date(2026, 8, 14, 9, 15, 0, 0, IST)
	if want := sessionOpen.Add(-30 * 24 * time.Hour); !req.From.Equal(want) {
		t.Errorf("From = %v, want %v", req.From, want)
	}
}

// The strike window must follow the CAPTURE DAY's range, not the lookback's.
//
// captureSymbol deliberately returns everything back to the lookback horizon so
// the index series is stored in full. Measuring the window against all of it
// widened it to the index's 30-day swing — hundreds of points on NIFTY instead
// of tens — capturing far more strikes than configured and costing proportionally
// more requests, while appearing to work.
func TestStrikeWindowIgnoresLookbackRange(t *testing.T) {
	friday := time.Date(2026, 8, 14, 16, 0, 0, 0, IST)
	expiry := time.Date(2026, 8, 18, 0, 0, 0, 0, IST)
	chain := chainFor("NIFTY", expiry, 20000, 30000, 50)

	// A month of index history that ranged 23000–25000, then a quiet capture
	// day pinned at 24350.
	var idx []marketdata.Candle
	old := time.Date(2026, 7, 20, 9, 15, 0, 0, IST)
	for i, p := range []float64{23000, 25000, 24000} {
		at := old.Add(time.Duration(i) * 5 * time.Minute)
		idx = append(idx, marketdata.Candle{
			Open: p, High: p, Low: p, Close: p, OpenTime: at,
		})
	}
	dayOpen := time.Date(2026, 8, 14, 9, 15, 0, 0, IST)
	for i := 0; i < 3; i++ {
		at := dayOpen.Add(time.Duration(i) * 5 * time.Minute)
		idx = append(idx, marketdata.Candle{
			Open: 24350, High: 24350, Low: 24350, Close: 24350, OpenTime: at,
		})
	}

	prov := &captureStub{candles: map[string][]marketdata.Candle{"NIFTY 50": idx}}
	c := NewCapturer(prov, testInstruments(t, chain), NSE(), CaptureOptions{
		Strikes:     2, // ±2 strikes → 5 strikes → 10 contracts
		Expiries:    1,
		Lookback:    30 * 24 * time.Hour,
		Underlyings: []CaptureTarget{{Underlying: "NIFTY", Index: "NIFTY 50"}},
	}, nil)

	rep, err := c.CaptureDay(context.Background(), friday)
	if err != nil {
		t.Fatalf("CaptureDay: %v", err)
	}

	// 5 strikes × CE+PE = 10 contracts, plus the index itself.
	if want := 10 + 1; rep.Contracts != want {
		t.Errorf("captured %d contracts, want %d — the window was sized off the "+
			"lookback range rather than the capture day", rep.Contracts, want)
	}
	for _, req := range prov.asked {
		if req.Symbol == "NIFTY 50" {
			continue
		}
		var strike float64
		if _, err := fmt.Sscanf(req.Symbol, "NIFTY%fCE", &strike); err != nil {
			if _, err := fmt.Sscanf(req.Symbol, "NIFTY%fPE", &strike); err != nil {
				continue
			}
		}
		if strike < 24250 || strike > 24450 {
			t.Errorf("captured strike %v, outside ±2 strikes of the day's 24350", strike)
		}
	}
}
