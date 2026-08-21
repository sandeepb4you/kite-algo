package app

import (
	"context"
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/history"
)

// A typed time is either a two-digit 24-hour clock or nothing.
//
// "3:20" is the case that matters. time.Parse reads it as 03:20, so a trader who
// meant the afternoon would get a flatten that fires at twenty past three in the
// morning, finds a closed market and nothing to close, and marks the day done —
// after which the real square-off never runs, silently, on a day they believe it
// is set.
func TestNormalizeSquareOffTime(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "15:20", want: "15:20"},
		{in: " 15:20 ", want: "15:20"},
		{in: "00:00", want: "00:00"},
		{in: "23:59", want: "23:59"},
		{in: "", want: ""},
		{in: "off", want: ""},
		{in: "OFF", want: ""},
		{in: "none", want: ""},
		{in: "3:20", wantErr: true},
		{in: "15:20:00", wantErr: true},
		{in: "24:00", wantErr: true},
		{in: "15.20", wantErr: true},
		{in: "3pm", wantErr: true},
		{in: "1520", wantErr: true},
	} {
		got, err := normalizeSquareOffTime(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeSquareOffTime(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeSquareOffTime(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeSquareOffTime(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Each desk sets its own book's time, and must not disturb the other's.
func TestSaveSquareOffTimeTouchesOneBookAndSurvivesAReload(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t)

	if _, _, err := a.SaveSquareOffTimes(ctx, broker.BookReal, "15:20"); err != nil {
		t.Fatalf("save real: %v", err)
	}
	if _, _, err := a.SaveSquareOffTimes(ctx, broker.BookPaper, "15:25"); err != nil {
		t.Fatalf("save paper: %v", err)
	}

	got := a.SquareOffTimes()
	if got.Real != "15:20" || got.Paper != "15:25" {
		t.Fatalf("times = %+v, want real 15:20 and paper 15:25 — "+
			"setting one book's time must not disturb the other's", got)
	}

	// Persisted, not merely in memory: a time that vanishes on restart is worse
	// than one that was never set, because the operator believes it is armed.
	reloaded := loadSquareOffTimes(ctx, a.Store, a.Cfg, nil)
	if reloaded != got {
		t.Errorf("reloaded %+v, want %+v", reloaded, got)
	}

	// And clearing one leaves the other alone.
	if _, _, err := a.SaveSquareOffTimes(ctx, broker.BookReal, "off"); err != nil {
		t.Fatalf("disable real: %v", err)
	}
	if after := a.SquareOffTimes(); after.Real != "" || after.Paper != "15:25" {
		t.Errorf("after disabling the real book: %+v, want real off and paper 15:25", after)
	}
}

// config.yaml supplies the starting value; a saved time overrides it.
func TestSquareOffTimesFallBackToConfigThenSaved(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{}
	cfg.Risk.Live.SquareOffTime = "15:10"
	cfg.Risk.Paper.SquareOffTime = "15:30"

	if got := configuredSquareOffTimes(cfg); got.Real != "15:10" || got.Paper != "15:30" {
		t.Errorf("configured = %+v", got)
	}
	// A nil store cannot override anything, so the config stands.
	if got := loadSquareOffTimes(ctx, nil, cfg, nil); got.Real != "15:10" {
		t.Errorf("loaded %+v with no store, want the config values", got)
	}
	// A garbage config value is off rather than fatal — the platform must still
	// boot, because it is the only way to reach the screen that flattens by hand.
	bad := &config.Config{}
	bad.Risk.Live.SquareOffTime = "half past three"
	if got := configuredSquareOffTimes(bad); got.Real != "" {
		t.Errorf("unparseable config time became %q, want off", got.Real)
	}
}

// A time typed after it has already passed starts TOMORROW.
//
// Otherwise pressing Save at 15:30 on a 15:20 setting would flatten the book
// then and there. Nobody typing a time into a form expects it to be a
// square-off button.
func TestSquareOffTimeSetAfterItPassedSkipsToday(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t)

	// One minute ago in IST, so the "already passed" branch is taken whenever
	// this test runs.
	past := time.Now().In(history.IST).Add(-time.Minute).Format("15:04")

	_, skipped, err := a.SaveSquareOffTimes(ctx, broker.BookPaper, past)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !skipped {
		t.Fatal("a time that has already passed today was accepted as due — " +
			"saving it would have flattened the book on the spot")
	}
	if got := a.SquareOffStatusFor(broker.BookPaper); !got.Done {
		t.Error("status does not report today as done, so the desk would show a " +
			"pending flatten that will not happen until tomorrow")
	}
}

// The scheduler flattens the book at its time, once, and leaves the other book
// alone.
func TestSquareOffSchedulerFlattensOneBookOncePerDay(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t)

	const symbol = "NIFTY2681824350CE"
	a.paper.OnPrice(symbol, 100)
	if _, err := a.Engine.PlaceOrder(ctx, broker.OrderRequest{
		StrategyID: "manual", Exchange: "NFO", TradingSymbol: symbol,
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideSell, Quantity: 65, Validity: broker.ValidityDay,
	}); err != nil {
		t.Fatalf("open a paper position: %v", err)
	}
	a.Engine.RefreshPositions(ctx)
	if openPaper(a) != 1 {
		t.Fatalf("setup: %d open paper positions, want 1", openPaper(a))
	}

	if _, _, err := a.SaveSquareOffTimes(ctx, broker.BookPaper, "15:20"); err != nil {
		t.Fatalf("set the time: %v", err)
	}
	// SaveSquareOffTimes may have marked today done, if the test happens to run
	// after 15:20 IST. Clear that: this test is about the tick, not the save.
	a.squareOff.mu.Lock()
	a.squareOff.done = map[string]string{}
	a.squareOff.mu.Unlock()

	sched := a.squareOff
	day := time.Date(2026, 8, 18, 0, 0, 0, 0, history.IST)

	// Before the time, nothing happens.
	sched.tick(ctx, day.Add(15*time.Hour+19*time.Minute))
	a.Engine.RefreshPositions(ctx)
	if openPaper(a) != 1 {
		t.Fatal("the book was flattened a minute early")
	}

	// At the time, it flattens.
	sched.tick(ctx, day.Add(15*time.Hour+20*time.Minute))
	a.Engine.RefreshPositions(ctx)
	if got := openPaper(a); got != 0 {
		t.Fatalf("%d paper positions still open after the square-off time", got)
	}

	// Once per day, and only once. Re-entering after the flatten and ticking
	// again proves the day was claimed rather than the book merely being empty:
	// a scheduler that fired on every later tick would close a position the
	// operator deliberately opened after the square-off had run.
	if _, err := a.Engine.PlaceOrder(ctx, broker.OrderRequest{
		StrategyID: "manual", Exchange: "NFO", TradingSymbol: symbol,
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideSell, Quantity: 65, Validity: broker.ValidityDay,
	}); err != nil {
		t.Fatalf("re-open after the flatten: %v", err)
	}
	a.Engine.RefreshPositions(ctx)
	if openPaper(a) != 1 {
		t.Fatalf("setup: the re-opened position is not visible")
	}

	sched.tick(ctx, day.Add(15*time.Hour+25*time.Minute))
	sched.tick(ctx, day.Add(15*time.Hour+30*time.Minute))
	a.Engine.RefreshPositions(ctx)
	if openPaper(a) != 1 {
		t.Error("the scheduler flattened again on a later tick the same day")
	}

	// The real book was never asked for, having no time set.
	if a.SquareOffTimes().Real != "" {
		t.Error("setting the paper time also set the real one")
	}
}

// With no time set, the scheduler does nothing at all.
func TestSquareOffSchedulerIsInertWhenOff(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t)

	const symbol = "NIFTY2681824350CE"
	a.paper.OnPrice(symbol, 100)
	if _, err := a.Engine.PlaceOrder(ctx, broker.OrderRequest{
		StrategyID: "manual", Exchange: "NFO", TradingSymbol: symbol,
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideSell, Quantity: 65, Validity: broker.ValidityDay,
	}); err != nil {
		t.Fatalf("open a paper position: %v", err)
	}
	a.Engine.RefreshPositions(ctx)

	day := time.Date(2026, 8, 18, 0, 0, 0, 0, history.IST)
	for h := 9; h <= 23; h++ {
		a.squareOff.tick(ctx, day.Add(time.Duration(h)*time.Hour))
	}
	a.Engine.RefreshPositions(ctx)

	if openPaper(a) != 1 {
		t.Error("a position was flattened with no square-off time configured")
	}
}

// The desk needs to tell a pending flatten from one that has already run.
func TestSquareOffStatusReportsWhatTheDeskShows(t *testing.T) {
	a := newTestApp(t)

	off := a.SquareOffStatusFor(broker.BookReal)
	if off.Enabled() {
		t.Error("reported enabled with no time set")
	}
	if off.Label() != "" {
		t.Errorf("a disabled square-off has a countdown label %q", off.Label())
	}

	st := SquareOffStatus{In: 2*time.Hour + 14*time.Minute}
	if got := st.Label(); got != "2h 14m" {
		t.Errorf("Label() = %q, want %q", got, "2h 14m")
	}
	// Rounded up, so a flatten a few seconds away never reads as "0m", which
	// looks like something that has already happened.
	if got := (SquareOffStatus{In: 30 * time.Second}).Label(); got != "1m" {
		t.Errorf("Label() for 30s = %q, want %q", got, "1m")
	}
}

func openPaper(a *App) int {
	n := 0
	for _, p := range a.Engine.Positions() {
		if p.IsOpen() && !p.Book.IsReal() {
			n++
		}
	}
	return n
}
