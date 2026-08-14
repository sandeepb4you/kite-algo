package shortstraddle

import (
	"context"
	"fmt"
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/strategy"
)

// stubTrader is a Trader whose clock the test controls.
type stubTrader struct {
	now     time.Time
	orders  []broker.OrderRequest
	signals []strategy.Signal
}

func (s *stubTrader) PlaceOrder(_ context.Context, req broker.OrderRequest) (*broker.Order, error) {
	s.orders = append(s.orders, req)
	return &broker.Order{ID: "o", TradingSymbol: req.TradingSymbol, Side: req.Side}, nil
}
func (s *stubTrader) CancelOrder(context.Context, string) error { return nil }
func (s *stubTrader) LTP(string) float64                        { return 100 }
func (s *stubTrader) LotSize(string) int                        { return 75 }
func (s *stubTrader) Subscribe([]string) error                  { return nil }
func (s *stubTrader) Now() time.Time                            { return s.now }
func (s *stubTrader) Signal(sig strategy.Signal)                { s.signals = append(s.signals, sig) }

func (s *stubTrader) Lookup(symbol string) (strategy.Instrument, bool) {
	return strategy.Instrument{TradingSymbol: symbol, Exchange: "NFO", LotSize: 75}, true
}

// Options returns a chain spanning several strikes around 24500, so a test can
// move the spot without accidentally landing on a strike the chain lacks —
// which the strategy correctly treats as "nothing to trade".
func (s *stubTrader) Options(underlying string, _ time.Time) []strategy.Instrument {
	expiry := s.now.AddDate(0, 0, 3)
	var out []strategy.Instrument
	for strike := 24300.0; strike <= 24700; strike += 50 {
		for _, typ := range []string{"CE", "PE"} {
			out = append(out, strategy.Instrument{
				TradingSymbol:  fmt.Sprintf("NIFTY24AUG%.0f%s", strike, typ),
				Name:           underlying,
				Strike:         strike,
				LotSize:        75,
				InstrumentType: typ,
				Exchange:       "NFO",
				Expiry:         expiry,
			})
		}
	}
	return out
}

func newStrategy(t *testing.T, now time.Time) (*Strategy, *stubTrader) {
	t.Helper()
	tr := &stubTrader{now: now}
	s := New("short-straddle", nil)
	if err := s.Init(context.Background(), tr, config.StrategyCfg{Params: map[string]any{}}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, tr
}

func spotTick(price float64) marketdata.Tick {
	return marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: price}
}

// TestTradesAgainOnANewDay covers the bug that made this strategy useless on a
// long-running server: entered/exited were set once and never reset, so it
// traded exactly once for the lifetime of the process. That was invisible while
// the platform was a CLI restarted every morning.
func TestTradesAgainOnANewDay(t *testing.T) {
	day1 := time.Date(2026, 8, 13, 10, 0, 0, 0, ist)
	s, tr := newStrategy(t, day1)
	ctx := context.Background()

	s.OnTick(ctx, spotTick(24510))
	if len(tr.orders) != 2 {
		t.Fatalf("day 1 placed %d orders, want 2 (one straddle)", len(tr.orders))
	}

	// Close both legs, as a real exit would.
	for _, sym := range []string{"NIFTY24AUG24500CE", "NIFTY24AUG24500PE"} {
		s.OnFill(ctx, broker.Fill{TradingSymbol: sym, Side: broker.SideBuy, Quantity: 75})
	}

	// Same day: it must not re-enter.
	tr.orders = nil
	tr.now = day1.Add(2 * time.Hour)
	s.OnTick(ctx, spotTick(24520))
	if len(tr.orders) != 0 {
		t.Errorf("re-entered on the same session: %d orders", len(tr.orders))
	}

	// Next trading day: it must arm again.
	tr.now = day1.AddDate(0, 0, 1)
	s.OnTick(ctx, spotTick(24530))
	if len(tr.orders) != 2 {
		t.Errorf("day 2 placed %d orders, want 2 — the strategy did not re-arm "+
			"for the new session and would never trade again", len(tr.orders))
	}
}

// TestRolloverWaitsForAFlatBook ensures the daily reset never abandons a live
// position. Clearing the leg map while still short would lose track of it.
func TestRolloverWaitsForAFlatBook(t *testing.T) {
	day1 := time.Date(2026, 8, 13, 10, 0, 0, 0, ist)
	s, tr := newStrategy(t, day1)
	ctx := context.Background()

	s.OnTick(ctx, spotTick(24510)) // enters, legs stay open

	tr.orders = nil
	tr.now = day1.AddDate(0, 0, 1) // new day, but still holding
	s.OnTick(ctx, spotTick(24520))

	s.mu.Lock()
	entered, legs := s.entered, len(s.legs)
	s.mu.Unlock()

	if !entered {
		t.Error("rollover reset the entry flag while legs were still open")
	}
	if legs != 2 {
		t.Errorf("leg map has %d entries, want the 2 open legs preserved", legs)
	}
}

// TestSquareOffTimeUsesTheInjectedClock is what makes backtesting possible. If
// the strategy read the wall clock, replaying past data would evaluate the
// square-off window against today and exit immediately every time.
func TestSquareOffTimeUsesTheInjectedClock(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		wantPast bool
	}{
		{"before cutoff", time.Date(2026, 8, 13, 10, 0, 0, 0, ist), false},
		{"after cutoff", time.Date(2026, 8, 13, 15, 20, 0, 0, ist), true},
		{"cutoff in UTC still resolves in IST",
			time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), true}, // 15:30 IST
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pastSquareOff("15:15", tc.now); got != tc.wantPast {
				t.Errorf("pastSquareOff(15:15, %s) = %v, want %v",
					tc.now.Format(time.RFC3339), got, tc.wantPast)
			}
		})
	}
}

func TestSquareOffTimeIgnoresMalformedClock(t *testing.T) {
	if pastSquareOff("half past three", time.Now()) {
		t.Error("an unparseable clock should not trigger a square-off")
	}
}

// TestStopDoesNotTrade covers the semantic split: the engine decides whether an
// outgoing strategy is flattened, so Stop itself must place no orders.
func TestStopDoesNotTrade(t *testing.T) {
	s, tr := newStrategy(t, time.Date(2026, 8, 13, 10, 0, 0, 0, ist))
	ctx := context.Background()

	s.OnTick(ctx, spotTick(24510))
	tr.orders = nil

	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(tr.orders) != 0 {
		t.Errorf("Stop placed %d orders; flattening is the engine's decision", len(tr.orders))
	}

	// SquareOff, by contrast, must close the legs.
	if err := s.SquareOff(ctx, "test"); err != nil {
		t.Fatalf("SquareOff: %v", err)
	}
	if len(tr.orders) != 2 {
		t.Errorf("SquareOff placed %d orders, want 2 to close both legs", len(tr.orders))
	}
	for _, o := range tr.orders {
		if o.Side != broker.SideBuy {
			t.Errorf("closing order side = %s, want BUY to cover a short", o.Side)
		}
	}
}

// TestDefaultsComeFromTheDescriptor confirms Init no longer carries its own
// fallback values, so there is exactly one source of truth for each default.
func TestDefaultsComeFromTheDescriptor(t *testing.T) {
	s, _ := newStrategy(t, time.Now())
	if s.indexSymbol != "NIFTY 50" || s.underlying != "NIFTY" {
		t.Errorf("instrument defaults = %q/%q", s.indexSymbol, s.underlying)
	}
	if s.lots != 1 || s.strikeStep != 50 || s.exitDelta != 0.25 {
		t.Errorf("numeric defaults = lots %d, step %v, delta %v", s.lots, s.strikeStep, s.exitDelta)
	}
	if s.squareOffClock != "15:15" || s.product != broker.ProductMIS {
		t.Errorf("execution defaults = %q / %q", s.squareOffClock, s.product)
	}
}

// TestInvalidParamsAreRejected checks the descriptor's validation reaches Init.
func TestInvalidParamsAreRejected(t *testing.T) {
	s := New("short-straddle", nil)
	err := s.Init(context.Background(), &stubTrader{now: time.Now()},
		config.StrategyCfg{Params: map[string]any{"lots": "500"}}) // above max
	if err == nil {
		t.Error("out-of-range lots was accepted")
	}
}

// TestEntrySignalIsEmitted confirms the UI's activity feed gets told what the
// strategy did and why.
func TestEntrySignalIsEmitted(t *testing.T) {
	s, tr := newStrategy(t, time.Date(2026, 8, 13, 10, 0, 0, 0, ist))
	s.OnTick(context.Background(), spotTick(24510))

	if len(tr.signals) == 0 {
		t.Fatal("entering a position emitted no signal")
	}
	if tr.signals[0].Kind != "enter" {
		t.Errorf("signal kind = %q, want enter", tr.signals[0].Kind)
	}
	if tr.signals[0].Message == "" {
		t.Error("signal has no message; the UI would show a blank line")
	}
}
