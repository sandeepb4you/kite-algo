package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
)

// chainCSV builds an instrument-master CSV covering two expiries of NIFTY
// options across a range of strikes, mimicking Kite's feed.
func chainCSV(strikes []float64, expiries []string) string {
	var b strings.Builder
	b.WriteString("instrument_token,tradingsymbol,name,expiry,strike,lot_size,instrument_type,segment,exchange,tick_size\n")

	token := uint32(10000)
	for _, exp := range expiries {
		tag := strings.ReplaceAll(exp, "-", "")
		for _, s := range strikes {
			for _, typ := range []string{"CE", "PE"} {
				token++
				fmt.Fprintf(&b, "%d,NIFTY%s%.0f%s,NIFTY,%s,%.0f,75,%s,NFO-OPT,NFO,0.05\n",
					token, tag, s, typ, exp, s, typ)
			}
		}
	}
	return b.String()
}

func chainEngine(t *testing.T, csv string) *Engine {
	t.Helper()
	m, err := kite.ParseInstruments(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse instruments: %v", err)
	}
	e := newTestEngine()
	e.instruments = m
	return e
}

// nextWeekdays returns n future dates, so the fixture is never accidentally in
// the past — expired contracts are filtered out by design.
func nextWeekdays(n int) []string {
	out := make([]string, 0, n)
	d := time.Now().AddDate(0, 0, 2)
	for i := 0; i < n; i++ {
		out = append(out, d.Format("2006-01-02"))
		d = d.AddDate(0, 0, 7)
	}
	return out
}

func TestOptionChainCentresOnATM(t *testing.T) {
	strikes := []float64{24000, 24100, 24200, 24300, 24400, 24500, 24600, 24700, 24800, 24900, 25000}
	expiries := nextWeekdays(2)
	e := chainEngine(t, chainCSV(strikes, expiries))

	// Spot near 24520 → ATM should be 24500.
	e.handleTick(marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 24520})

	chain, err := e.OptionChain("NIFTY", time.Time{}, 2)
	if err != nil {
		t.Fatalf("OptionChain: %v", err)
	}

	if chain.ATMStrike != 24500 {
		t.Errorf("ATM strike = %v, want 24500 (nearest to spot 24520)", chain.ATMStrike)
	}
	if chain.Spot != 24520 {
		t.Errorf("spot = %v, want 24520", chain.Spot)
	}
	// depth 2 → 2 either side plus ATM.
	if len(chain.Rows) != 5 {
		t.Fatalf("got %d rows at depth 2, want 5", len(chain.Rows))
	}
	if chain.Rows[0].Strike != 24300 || chain.Rows[4].Strike != 24700 {
		t.Errorf("window = %v..%v, want 24300..24700",
			chain.Rows[0].Strike, chain.Rows[4].Strike)
	}

	var atmRows int
	for _, r := range chain.Rows {
		if r.IsATM {
			atmRows++
			if r.Strike != 24500 {
				t.Errorf("row marked ATM at strike %v", r.Strike)
			}
		}
	}
	if atmRows != 1 {
		t.Errorf("%d rows marked ATM, want exactly 1", atmRows)
	}
}

// TestOptionChainDefaultsToNearestExpiry is what a weekly trader expects: open
// the terminal and see this week, not a random month.
func TestOptionChainDefaultsToNearestExpiry(t *testing.T) {
	expiries := nextWeekdays(3)
	e := chainEngine(t, chainCSV([]float64{24500}, expiries))

	chain, err := e.OptionChain("NIFTY", time.Time{}, 5)
	if err != nil {
		t.Fatalf("OptionChain: %v", err)
	}
	if got := chain.Expiry.Format("2006-01-02"); got != expiries[0] {
		t.Errorf("default expiry = %s, want the nearest (%s)", got, expiries[0])
	}
	if len(chain.Expiries) != 3 {
		t.Errorf("offered %d expiries, want 3", len(chain.Expiries))
	}
}

func TestOptionChainHonoursRequestedExpiry(t *testing.T) {
	expiries := nextWeekdays(3)
	e := chainEngine(t, chainCSV([]float64{24500}, expiries))

	want, _ := time.Parse("2006-01-02", expiries[2])
	chain, err := e.OptionChain("NIFTY", want, 5)
	if err != nil {
		t.Fatalf("OptionChain: %v", err)
	}
	if got := chain.Expiry.Format("2006-01-02"); got != expiries[2] {
		t.Errorf("expiry = %s, want the requested %s", got, expiries[2])
	}
}

// TestOptionChainPairsCallsAndPuts checks each row carries both legs at the
// same strike — mixing them up would put an operator one click from selling the
// wrong side.
func TestOptionChainPairsCallsAndPuts(t *testing.T) {
	expiries := nextWeekdays(1)
	e := chainEngine(t, chainCSV([]float64{24400, 24500, 24600}, expiries))
	e.handleTick(marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 24500})

	chain, err := e.OptionChain("NIFTY", time.Time{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range chain.Rows {
		if !strings.HasSuffix(r.Call.TradingSymbol, "CE") {
			t.Errorf("strike %v call leg is %q", r.Strike, r.Call.TradingSymbol)
		}
		if !strings.HasSuffix(r.Put.TradingSymbol, "PE") {
			t.Errorf("strike %v put leg is %q", r.Strike, r.Put.TradingSymbol)
		}
		if r.Call.LotSize != 75 || r.Put.LotSize != 75 {
			t.Errorf("strike %v lot sizes = %d/%d, want 75",
				r.Strike, r.Call.LotSize, r.Put.LotSize)
		}
		strike := fmt.Sprintf("%.0f", r.Strike)
		if !strings.Contains(r.Call.TradingSymbol, strike) {
			t.Errorf("call %q does not belong to strike %v", r.Call.TradingSymbol, r.Strike)
		}
	}
}

// TestOptionChainShowsHeldQuantity surfaces an existing position inline, so the
// operator sees what they already hold while placing the next order.
func TestOptionChainShowsHeldQuantity(t *testing.T) {
	expiries := nextWeekdays(1)
	e := chainEngine(t, chainCSV([]float64{24500}, expiries))
	e.handleTick(marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 24500})

	chain, err := e.OptionChain("NIFTY", time.Time{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	ceSymbol := chain.Rows[0].Call.TradingSymbol

	// Simulate holding a short call.
	e.mu.Lock()
	e.positions = []broker.Position{{TradingSymbol: ceSymbol, NetQuantity: -75}}
	e.mu.Unlock()

	chain, err = e.OptionChain("NIFTY", time.Time{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if chain.Rows[0].Call.Held != -75 {
		t.Errorf("held = %d, want -75", chain.Rows[0].Call.Held)
	}
	if chain.Rows[0].Put.Held != 0 {
		t.Errorf("put leg shows %d held, want 0", chain.Rows[0].Put.Held)
	}
}

// TestOptionChainWithoutSpotStillRenders covers the moment right after login,
// before the first tick — the page must show usable strikes rather than the
// deepest out-of-the-money ones.
func TestOptionChainWithoutSpotStillRenders(t *testing.T) {
	expiries := nextWeekdays(1)
	strikes := []float64{24000, 24100, 24200, 24300, 24400, 24500, 24600, 24700}
	e := chainEngine(t, chainCSV(strikes, expiries))

	chain, err := e.OptionChain("NIFTY", time.Time{}, 2)
	if err != nil {
		t.Fatalf("OptionChain with no spot price: %v", err)
	}
	if len(chain.Rows) == 0 {
		t.Fatal("no rows rendered without a spot price")
	}
	// Falls back to the middle of the chain, not the extremes.
	mid := strikes[len(strikes)/2]
	if chain.ATMStrike != mid {
		t.Errorf("fallback centre = %v, want the middle strike %v", chain.ATMStrike, mid)
	}
}

func TestOptionChainNeedsInstruments(t *testing.T) {
	e := newTestEngine()
	if _, err := e.OptionChain("NIFTY", time.Time{}, 5); err == nil {
		t.Error("a chain without an instrument master should fail clearly")
	}
}

func TestOptionChainUnknownUnderlying(t *testing.T) {
	e := chainEngine(t, chainCSV([]float64{24500}, nextWeekdays(1)))
	if _, err := e.OptionChain("BANKNIFTY", time.Time{}, 5); err == nil {
		t.Error("an underlying with no contracts should report an error")
	}
}

// TestChainSymbolsCoversEverythingOnScreen ensures the subscription set matches
// what is rendered — a missing symbol would show a price that never updates.
func TestChainSymbolsCoversEverythingOnScreen(t *testing.T) {
	e := chainEngine(t, chainCSV([]float64{24400, 24500, 24600}, nextWeekdays(1)))
	e.handleTick(marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 24500})

	chain, err := e.OptionChain("NIFTY", time.Time{}, 5)
	if err != nil {
		t.Fatal(err)
	}

	symbols := chain.ChainSymbols()
	want := len(chain.Rows)*2 + 1 // both legs per row, plus the spot index
	if len(symbols) != want {
		t.Errorf("subscribing to %d symbols, want %d (every leg plus spot)", len(symbols), want)
	}

	set := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		set[s] = true
	}
	if !set[chain.SpotSymbol] {
		t.Error("spot index is not in the subscription set")
	}
	for _, r := range chain.Rows {
		if !set[r.Call.TradingSymbol] || !set[r.Put.TradingSymbol] {
			t.Errorf("strike %v has a leg missing from the subscription set", r.Strike)
		}
	}
}

// TestSpotSymbolMapping covers the naming mismatch: options are written on
// "NIFTY" but the index quote is "NIFTY 50", so a chain cannot find its own
// spot without the translation.
func TestSpotSymbolMapping(t *testing.T) {
	cases := map[string]string{
		"NIFTY":     "NIFTY 50",
		"BANKNIFTY": "NIFTY BANK",
		"FINNIFTY":  "NIFTY FIN SERVICE",
		"INFY":      "INFY", // no mapping: stock options quote under their own name
	}
	for underlying, want := range cases {
		if got := SpotSymbolFor(underlying); got != want {
			t.Errorf("SpotSymbolFor(%q) = %q, want %q", underlying, got, want)
		}
	}
}
