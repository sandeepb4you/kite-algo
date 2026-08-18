package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kite-algo/internal/app"
	"kite-algo/internal/backtest"
	"kite-algo/internal/broker"
	"kite-algo/internal/engine"
	"kite-algo/internal/kite"
	"kite-algo/internal/risk"
	"kite-algo/internal/strategy"
)

// TestEveryTemplateRenders executes each page template against the payload its
// handler actually supplies.
//
// Go templates resolve field names at EXECUTION time, so a partial that reads
// .Data.Positions compiles fine against a page whose Data has no such field and
// fails only when someone opens that page. That is exactly how the market page
// shipped broken: positions_table took the whole page view, so it worked on the
// dashboard and blew up everywhere else.
//
// Passing each partial the data it needs — rather than the enclosing view —
// removes the coupling; this test makes sure it stays removed.
// sampleDescriptor exercises every ParamKind the form partial can render.
var sampleDescriptor = strategy.Descriptor{
	Type: "short-straddle", Title: "Short straddle",
	Params: []strategy.ParamSpec{
		{Key: "lots", Label: "Lots", Kind: strategy.KindInt, Default: 1,
			Min: strategy.Ptr(1), Max: strategy.Ptr(50)},
		{Key: "product", Label: "Product", Kind: strategy.KindEnum,
			Options: []string{"MIS", "NRML"}, Default: "MIS"},
		{Key: "square_off_time", Label: "Square off", Kind: strategy.KindTime, Default: "15:15"},
		{Key: "exit_delta", Label: "Exit delta", Kind: strategy.KindFloat, Default: 0.25},
		{Key: "underlying", Label: "Underlying", Kind: strategy.KindString, Default: "NIFTY"},
		{Key: "hedged", Label: "Buy a hedge", Kind: strategy.KindBool, Default: false},
	},
}

func TestEveryTemplateRenders(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	positions := []broker.Position{{
		StrategyID: "short-straddle", Exchange: "NFO",
		TradingSymbol: "NIFTY24AUG24500CE", Product: broker.ProductMIS,
		NetQuantity: -75, AveragePrice: 120.5, LastPrice: 100.25, PnL: 1518.75,
	}}
	orders := []broker.Order{{
		ID: "o-1", TradingSymbol: "NIFTY24AUG24500CE", Side: broker.SideSell,
		OrderType: broker.OrderTypeMarket, Quantity: 75, FilledQuantity: 75,
		Status: broker.StatusComplete, Mode: "paper",
	}}
	watchlist := []quoteRow{{Symbol: "NIFTY 50", Last: 24512.35}}
	status := app.Status{
		Mode: "paper", RiskLimits: risk.Limits{MaxDailyLoss: 5000, MaxLotsPerTrade: 1},
	}

	cases := []struct {
		name string
		data any
	}{
		{"dashboard.html", dashboardData{Positions: positions, Streaming: true, Routing: "paper"}},
		{"market.html", marketData{Watchlist: watchlist, Positions: positions, Streaming: true}},
		{"trade.html", tradeData{
			Watchlist: watchlist, Positions: positions, Orders: orders,
			Streaming: true, Routing: "paper",
		}},
		{"connect.html", nil},
		{"error.html", nil},
		{"strategies.html", strategyData{
			Running:   []engine.StrategyStatus{{InstanceID: "s1", State: engine.StateRunning, Positions: positions}},
			Available: []strategy.Descriptor{{Type: "short-straddle", Title: "Short straddle"}},
		}},
		{"strategy_new.html", strategyFormData{
			Type: "short-straddle", Title: "Short straddle",
			Params: paramFields(sampleDescriptor, nil, false),
		}},
		{"risk.html", riskData{
			Limits:     risk.Limits{MaxDailyLoss: 7500, MaxOrderValue: 200000, MaxLotsPerTrade: 2, MaxOpenPositions: 8},
			Defaults:   risk.Limits{MaxDailyLoss: 5000, MaxOrderValue: 100000, MaxLotsPerTrade: 1, MaxOpenPositions: 5},
			Overridden: true,
		}},
		{"research.html", researchData{
			Symbol: "NIFTY 50", Interval: "5minute", From: "2024-08-01", To: "2024-08-02",
			Intervals: kite.Intervals, Snapshots: snapshotInfo{HaveToday: false, AsOf: "2024-08-01"},
		}},
		{"backtest.html", backtestData{
			Available: []strategy.Descriptor{{Type: "short-straddle", Title: "Short straddle"}},
			Intervals: kite.Intervals,
			Paths:     []backtest.BarPath{backtest.PathPessimist},
			Interval:  "5minute", From: "2024-08-01", To: "2024-08-02",
			Capital: "100000",
			Params:  paramFields(sampleDescriptor, nil, false),
		}},
		// The unchosen-strategy state: the form renders before any strategy is
		// selected, so the parameter block must survive an empty field list.
		{"backtest_params.html", []paramField(nil)},
		{"chain_fragment.html", tradeData{Chain: engine.OptionChain{
			Underlying: "NIFTY", SpotSymbol: "NIFTY 50", Spot: 24512.35,
			Expiry:      time.Now().AddDate(0, 0, 3),
			Expiries:    []time.Time{time.Now().AddDate(0, 0, 3), time.Now().AddDate(0, 0, 10)},
			Underlyings: []string{"NIFTY", "BANKNIFTY"},
			ATMStrike:   24500,
			Rows: []engine.ChainRow{{
				Strike: 24500, IsATM: true,
				Call: engine.ChainLeg{TradingSymbol: "NIFTY24AUG24500CE", LastPrice: 120.5, LotSize: 75, Held: -75},
				Put:  engine.ChainLeg{TradingSymbol: "NIFTY24AUG24500PE", LastPrice: 98.25, LotSize: 75},
			}},
			Truncated: true,
		}, Streaming: true}},
		{"positions_fragment.html", dashboardData{Positions: positions}},
		{"orders_fragment.html", tradeData{Orders: orders}},
		{"watchlist_fragment.html", marketData{Watchlist: watchlist}},
		{"strategies_fragment.html", strategyData{
			Running: []engine.StrategyStatus{{InstanceID: "s1", State: engine.StateRunning}},
		}},
		{"status_fragment.html", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			v := pageView{Title: "T", Status: status, CSRF: "csrf-token", Data: tc.data}
			if err := r.Render(w, 200, tc.name, v); err != nil {
				t.Fatalf("render %s: %v", tc.name, err)
			}
			if strings.TrimSpace(w.Body.String()) == "" {
				t.Errorf("%s rendered empty", tc.name)
			}
		})
	}
}

// TestChainWorksWithoutJavaScript is the property the chain was rebuilt around.
//
// Picking a contract originally relied on a delegated click handler, and
// selecting an expiry on an inline onchange — which the page's own CSP
// (script-src 'self') blocks outright. Both are now plain form submits, so the
// chain works with scripting broken, disabled, or blocked.
func TestChainWorksWithoutJavaScript(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x", Data: tradeData{
		Chain: engine.OptionChain{
			Underlying: "NIFTY", SpotSymbol: "NIFTY 50", Spot: 24512,
			Expiry:      time.Now().AddDate(0, 0, 3),
			Expiries:    []time.Time{time.Now().AddDate(0, 0, 3)},
			Underlyings: []string{"NIFTY"}, ATMStrike: 24500,
			Rows: []engine.ChainRow{{
				Strike: 24500,
				Call:   engine.ChainLeg{TradingSymbol: "NIFTY25AUG24500CE", LastPrice: 120.5, LotSize: 75},
				Put:    engine.ChainLeg{TradingSymbol: "NIFTY25AUG24500PE", LastPrice: 98.25, LotSize: 75},
			}},
		}}}
	if err := r.Render(w, 200, "chain_fragment.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	// No inline handlers: the CSP forbids them, so any that appear are dead code
	// that will silently do nothing in a browser.
	for _, attr := range []string{"onchange=", "onclick=", "onsubmit=", "javascript:"} {
		if strings.Contains(body, attr) {
			t.Errorf("chain uses %s, which the Content-Security-Policy blocks", attr)
		}
	}

	// Picking a contract must be a real submit carrying the symbol.
	if !strings.Contains(body, `type="submit" name="symbol" value="NIFTY25AUG24500CE"`) {
		t.Error("call premium is not a submit button carrying its symbol")
	}
	if !strings.Contains(body, `type="submit" name="symbol" value="NIFTY25AUG24500PE"`) {
		t.Error("put premium is not a submit button carrying its symbol")
	}
	// ...and must carry the chain context, or the reload would lose the expiry.
	if !strings.Contains(body, `name="underlying" value="NIFTY"`) {
		t.Error("the pick form does not preserve the underlying")
	}
	// Changing expiry must be submittable without scripting.
	if !strings.Contains(body, `type="submit"`) || !strings.Contains(body, ">Load<") {
		t.Error("no visible submit control for the expiry selector")
	}
}

// TestTerminalTabsAreCSSOnly checks the positions/orders switcher does not
// depend on JavaScript.
//
// Switching panels is a radio input plus :checked, so it survives a script
// error, a stale cache, or the CSP — the same failure modes that broke the
// chain's click handler. Only the count badges need scripting, and those
// degrade to their server-rendered values.
func TestTerminalTabsAreCSSOnly(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}

	positions := []broker.Position{
		{TradingSymbol: "NIFTY25AUG24500CE", NetQuantity: -75},
		{TradingSymbol: "NIFTY25AUG24500PE", NetQuantity: -75},
	}
	orders := []broker.Order{{ID: "o-1", TradingSymbol: "NIFTY25AUG24500CE",
		Side: broker.SideSell, OrderType: broker.OrderTypeMarket, Quantity: 75,
		Status: broker.StatusOpen, Mode: "paper"}}

	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x",
		Data: tradeData{Positions: positions, Orders: orders}}
	if err := r.Render(w, 200, "trade.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	// Radio-driven, not script-driven.
	for _, want := range []string{
		`type="radio" name="book" id="tab-positions"`,
		`type="radio" name="book" id="tab-orders"`,
		`<label for="tab-positions"`,
		`<label for="tab-orders"`,
		`id="panel-positions"`,
		`id="panel-orders"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("terminal is missing %q — tab switching would need JavaScript", want)
		}
	}
	if strings.Contains(body, "onclick=") {
		t.Error("tabs use an inline onclick, which the CSP blocks")
	}

	// Counts render server-side so they are correct before any script runs.
	if !strings.Contains(body, `data-count-of="#panel-positions">2<`) {
		t.Error("positions tab does not show its count (2) on first render")
	}
	if !strings.Contains(body, `data-count-of="#panel-orders">1<`) {
		t.Error("orders tab does not show its count (1) on first render")
	}

	// The square-off button must sit outside the polled region, or the first
	// background refresh would delete it.
	sqIdx := strings.Index(body, "Square off everything")
	pollIdx := strings.Index(body, `data-poll="/partials/positions"`)
	closeIdx := strings.Index(body[pollIdx:], "</div>")
	if sqIdx > 0 && pollIdx > 0 && sqIdx < pollIdx+closeIdx {
		t.Error("the square-off button is inside the polled region and would be wiped by a refresh")
	}
}

// TestSideToggleSubmitsAValidSide guards the field most likely to be wrong.
//
// Side moved from a <select> to a radio pair styled as a toggle. Radios post
// nothing when none is checked, so BUY must be checked by default — otherwise
// the ticket would submit without a side and be rejected, or worse, default to
// something the operator did not choose.
func TestSideToggleSubmitsAValidSide(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x", Data: tradeData{}}
	if err := r.Render(w, 200, "trade.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if !strings.Contains(body, `type="radio" name="side" id="side-buy" value="BUY" checked`) {
		t.Error("BUY is not checked by default; the ticket could submit with no side")
	}
	if !strings.Contains(body, `type="radio" name="side" id="side-sell" value="SELL"`) {
		t.Error("no SELL option in the side toggle")
	}
	// The values must match what the handler accepts.
	for _, side := range []string{`value="BUY"`, `value="SELL"`} {
		if !strings.Contains(body, side) {
			t.Errorf("side toggle is missing %s", side)
		}
	}
	if strings.Contains(body, `<select id="side"`) {
		t.Error("the old side dropdown is still present alongside the toggle")
	}

	// Double-click sends, so the ticket must be addressable by the handler and
	// the behaviour must be stated rather than hidden.
	if !strings.Contains(body, `id="ticket"`) {
		t.Error("the ticket form has no id; double-click-to-send cannot find it")
	}
	if !strings.Contains(body, "double-click to send") {
		t.Error("double-click-to-send is not signposted; a surprise order is the worst kind")
	}
}

// The terminal is simulated unconditionally, even while live routing is armed.
//
// This is the property the separate live desk buys: /trade posts to
// /api/orders, which stamps the paper book regardless of any form value, so
// there is no control on this page that could send real money and no
// double-click that could do it by surprise.
func TestTerminalNeverOffersARealOrder(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "live", LiveActive: true}, CSRF: "x",
		Data: tradeData{LiveMode: true}}
	if err := r.Render(w, 200, "trade.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if strings.Contains(body, "/api/live/orders") {
		t.Error("the terminal can post to the live endpoint")
	}
	if strings.Contains(body, `name="route"`) {
		t.Error("the terminal still carries a route picker; the live desk replaced it")
	}
	if strings.Contains(body, "PLACE REAL ORDER") {
		t.Error("the terminal offers a real-order button")
	}
	// And it should point the operator at where real orders actually happen.
	if !strings.Contains(body, `href="/live"`) {
		t.Error("the terminal does not link to the live desk while armed")
	}
}

// The live desk must not render a ticket until routing is armed, and the gate
// must demand both the phrase and the password.
func TestLiveDeskGatesBeforeArming(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "live", LiveArmed: true}, CSRF: "x",
		Data: liveData{Configured: true, Armed: false, SessionOK: true}}
	if err := r.Render(w, 200, "live.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if strings.Contains(body, `action="/api/live/orders"`) {
		t.Error("an unarmed live desk rendered an order ticket")
	}
	if !strings.Contains(body, "I UNDERSTAND") {
		t.Error("the gate does not ask for the confirmation phrase")
	}
	if !strings.Contains(body, `name="password"`) {
		t.Error("the gate does not re-ask for the password")
	}
}

// Once armed the desk shows a ticket that confirms unconditionally — every
// order from this page is real, so there is no case where the prompt is noise.
func TestLiveDeskArmedTicketConfirms(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "live", LiveActive: true}, CSRF: "x",
		Data: liveData{tradeData: tradeData{Live: true},
			Configured: true, Armed: true, SessionOK: true}}
	if err := r.Render(w, 200, "live.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if !strings.Contains(body, `action="/api/live/orders"`) {
		t.Error("the armed desk has no real order ticket")
	}
	// No confirmation dialog, by the operator's decision — a modal between the
	// decision and the fill costs time that matters to a discretionary trader.
	// The ticket must still be unmistakably the real one.
	if !strings.Contains(body, "PLACE REAL ORDER") {
		t.Error("the real ticket is not labelled as real")
	}
	// Disarming must be present and must not be gated behind a phrase.
	if !strings.Contains(body, `action="/api/live/disarm"`) {
		t.Error("no way to disarm from the page that armed")
	}
}

// A build that is not configured for live must say so instead of showing a gate
// that cannot open.
func TestLiveDeskExplainsWhenNotConfigured(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x",
		Data: liveData{Configured: false, Mode: "paper"}}
	if err := r.Render(w, 200, "live.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if strings.Contains(body, "I UNDERSTAND") {
		t.Error("offered a confirmation gate in a build that cannot go live")
	}
	if !strings.Contains(body, "live_confirm") {
		t.Error("did not explain what to change to enable live trading")
	}
}

// TestPositionsTableKeepsPnLVisible checks the compact layout: P&L is the number
// looked at most, and must not be the one pushed off the edge by a wide table in
// a narrow column.
func TestPositionsTableKeepsPnLVisible(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x", Data: dashboardData{
		Positions: []broker.Position{{
			StrategyID: "short-straddle", TradingSymbol: "NIFTY25AUG24500CE",
			Product: broker.ProductMIS, NetQuantity: -75,
			AveragePrice: 120.5, LastPrice: 100.25, PnL: 1518.75,
		}},
	}}
	if err := r.Render(w, 200, "positions_fragment.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	// Instrument, Qty, Avg, LTP, P&L plus a narrow action column — not the
	// original seven. Count closing tags: "<th" also matches "<thead".
	headers := strings.Count(body, "</th>")
	if headers > 6 {
		t.Errorf("positions table has %d columns; more than six forces a horizontal "+
			"scroll in the book column and hides P&L", headers)
	}
	// Each open position must be closable from its own row.
	if !strings.Contains(body, `name="symbol" value="NIFTY25AUG24500CE"`) {
		t.Error("no per-position close button")
	}
	// A simulated position closes on one click — there is nothing to protect.
	if strings.Contains(body, "data-confirm=") {
		t.Error("a paper close still prompts; nothing is at stake there")
	}
	// Entry price sits beside LTP: comparing what you paid against what it is
	// worth is the point of the row.
	if !strings.Contains(body, "120.50") {
		t.Error("average entry price is not shown")
	}
	if !strings.Contains(body, "100.25") {
		t.Error("last traded price is not shown")
	}
	// Attribution stays as secondary text rather than its own column.
	for _, want := range []string{"short-straddle", "MIS"} {
		if !strings.Contains(body, want) {
			t.Errorf("compact row lost %q; the information should move, not vanish", want)
		}
	}
	// html/template escapes the leading "+" as &#43;, which the browser renders
	// as "+". Match the digits rather than the sign.
	if !strings.Contains(body, "1,518.75") {
		t.Error("P&L is not rendered")
	}
	if !strings.Contains(body, "pnl-up") {
		t.Error("a profit is not coloured as one")
	}
}

// TestTicketPreFillsFromTheChain covers the server side of that flow: the symbol
// arrives as a query parameter and must land in the ticket.
func TestTicketPreFillsFromTheChain(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x", Data: tradeData{
		TicketSymbol: "NIFTY25AUG24500CE", TicketLot: 75,
	}}
	if err := r.Render(w, 200, "trade.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if !strings.Contains(body, `value="NIFTY25AUG24500CE"`) {
		t.Error("the ticket did not pre-fill with the picked contract")
	}
	if !strings.Contains(body, "Lot size 75") {
		t.Error("the lot size hint did not render for the picked contract")
	}
}

// TestEmptyStateTemplatesRender covers the zero-value path. Most of these pages
// are first seen with nothing in them — no positions, no orders, no results —
// and a nil-dereference there would break the page on first use.
func TestEmptyStateTemplatesRender(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		data any
	}{
		{"dashboard.html", dashboardData{}},
		{"market.html", marketData{}},
		{"trade.html", tradeData{}},
		{"strategies.html", strategyData{}},
		{"research.html", researchData{Intervals: kite.Intervals}},
		{"backtest.html", backtestData{Intervals: kite.Intervals}},
		// Not overridden: the page must render the config-defaults branch too.
		{"risk.html", riskData{}},
		{"positions_fragment.html", dashboardData{}},
		{"orders_fragment.html", tradeData{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			v := pageView{Title: "T", Status: app.Status{Mode: "paper"}, Data: tc.data}
			if err := r.Render(w, 200, tc.name, v); err != nil {
				t.Fatalf("render %s with empty data: %v", tc.name, err)
			}
		})
	}
}

// TestLiveAndHaltChromeRender exercises the alternate header states, which only
// appear in unusual conditions and would otherwise be discovered live — the
// worst moment to find a broken page.
func TestLiveAndHaltChromeRender(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}

	states := map[string]app.Status{
		"live active": {Mode: "live", LiveActive: true},
		"live armed":  {Mode: "live", LiveArmed: true},
		"halted": {Mode: "paper", Halt: engine.HaltState{
			Halted: true, Reason: "kill switch", By: "operator", At: time.Now(),
		}},
	}

	for name, st := range states {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			v := pageView{Title: "T", Status: st, Data: dashboardData{}}
			if err := r.Render(w, 200, "dashboard.html", v); err != nil {
				t.Fatalf("render dashboard in %s state: %v", name, err)
			}
			body := w.Body.String()
			if st.LiveActive {
				// The banner must name BOTH halves of mixed routing. "LIVE"
				// alone would imply the running strategies are live too, and an
				// operator who believes that misjudges every position on screen.
				if !strings.Contains(body, "LIVE") || !strings.Contains(body, "REAL MONEY") {
					t.Error("live session did not render the LIVE banner")
				}
				if !strings.Contains(body, "strategies remain simulated") {
					t.Error("live banner did not say strategies stay simulated; " +
						"an operator could read it as everything being live")
				}
			}
			if st.Halt.Halted && !strings.Contains(body, "TRADING HALTED") {
				t.Error("halted session did not render the halt banner")
			}
		})
	}
}

// The instrument label must decompose a dense symbol, and must never guess.
func TestInstrumentLabelSplitsSymbols(t *testing.T) {
	cases := []struct{ symbol, name, expiry string }{
		{"NIFTY2681824350CE", "NIFTY 24350 CE", "18 Aug"},
		{"NIFTY2681824350PE", "NIFTY 24350 PE", "18 Aug"},
		{"SENSEX2682075700CE", "SENSEX 75700 CE", "20 Aug"},
	}
	for _, tc := range cases {
		got := instrumentLabel(tc.symbol)
		if got.Name != tc.name {
			t.Errorf("%s: Name = %q, want %q", tc.symbol, got.Name, tc.name)
		}
		if got.Expiry != tc.expiry {
			t.Errorf("%s: Expiry = %q, want %q", tc.symbol, got.Expiry, tc.expiry)
		}
		if got.Raw != tc.symbol {
			t.Errorf("%s: Raw = %q, the full symbol must survive for copying", tc.symbol, got.Raw)
		}
	}
}

// A prettified label that is subtly wrong about the strike would be worse than
// the dense original, so anything unparseable falls back verbatim.
func TestInstrumentLabelFallsBackToTheRawSymbol(t *testing.T) {
	for _, sym := range []string{"NIFTY 50", "SOMETHING-ODD", "", "INFY"} {
		got := instrumentLabel(sym)
		if got.Name != sym {
			t.Errorf("instrumentLabel(%q).Name = %q, want the raw symbol", sym, got.Name)
		}
	}
}

// The live desk must be the terminal, not a reduced copy of it.
//
// A separate simplified desk would drift as the terminal gained features, and a
// real ticket that has quietly diverged from the paper one is precisely the
// difference that produces a mis-click. Both render terminal_body.
func TestLiveDeskIsTheFullTerminal(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	render := func(tmpl string, data any) string {
		w := httptest.NewRecorder()
		v := pageView{Status: app.Status{Mode: "live", LiveActive: true}, CSRF: "x", Data: data}
		if err := r.Render(w, 200, tmpl, v); err != nil {
			t.Fatalf("render %s: %v", tmpl, err)
		}
		return w.Body.String()
	}

	live := render("live.html", liveData{tradeData: tradeData{Live: true},
		Configured: true, Armed: true, SessionOK: true})
	paper := render("trade.html", tradeData{LiveMode: true})

	// Everything the terminal has, the live desk has too.
	for _, want := range []string{
		"Option chain",  // the chain was missing entirely before
		"col-chain",     //
		"col-ticket",    //
		"col-book",      // positions + orders panel
		"tab-positions", //
		"tab-orders",    //
	} {
		if !strings.Contains(live, want) {
			t.Errorf("live desk is missing %q, which the terminal has", want)
		}
		if !strings.Contains(paper, want) {
			t.Errorf("terminal is missing %q", want)
		}
	}
}

// The two desks must post to different endpoints, and each must poll fragments
// that keep it on its own page.
func TestDesksPostAndPollToTheirOwnEndpoints(t *testing.T) {
	paper := tradeData{}
	live := tradeData{Live: true}

	if got := paper.OrderAction(); got != "/api/orders" {
		t.Errorf("terminal posts to %q", got)
	}
	if got := live.OrderAction(); got != "/api/live/orders" {
		t.Errorf("live desk posts to %q", got)
	}
	if got := paper.PageURL(); got != "/trade" {
		t.Errorf("terminal chain submits to %q", got)
	}
	if got := live.PageURL(); got != "/live" {
		t.Errorf("live desk chain submits to %q", got)
	}

	// The page identity must survive into every polled fragment, or a
	// background refresh rewrites the chain's forms to point at /trade and
	// clicking a premium moves the operator off the real desk silently.
	for _, got := range []string{live.ChainPollURL(), live.PositionsPollURL(), live.OrdersPollURL()} {
		if !strings.Contains(got, "page=live") {
			t.Errorf("live poll URL %q loses the page identity", got)
		}
	}
	// And the terminal's stay clean.
	if got := paper.PositionsPollURL(); got != "/partials/positions" {
		t.Errorf("terminal positions poll = %q, want a bare path", got)
	}
}

// Closing a REAL position still confirms, and the prompt names the instrument
// and side — "are you sure?" alone tells you nothing when several positions are
// open, and closing the wrong leg of a spread is worse than not closing at all.
func TestRealPositionCloseStillConfirms(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	render := func(book broker.Book) string {
		w := httptest.NewRecorder()
		v := pageView{Status: app.Status{Mode: "live"}, CSRF: "x", Data: dashboardData{
			Positions: []broker.Position{{
				StrategyID: "manual", TradingSymbol: "NIFTY25AUG24500CE",
				Product: broker.ProductMIS, NetQuantity: -75,
				AveragePrice: 120.5, LastPrice: 100.25, Book: book,
			}},
		}}
		if err := r.Render(w, 200, "positions_fragment.html", v); err != nil {
			t.Fatal(err)
		}
		return w.Body.String()
	}

	real := render(broker.BookReal)
	if !strings.Contains(real, "data-confirm=") {
		t.Error("closing a REAL position does not confirm")
	}
	if !strings.Contains(real, "NIFTY25AUG24500CE") || !strings.Contains(real, "BUY") {
		t.Error("the confirmation does not name the instrument and side")
	}

	paper := render(broker.BookPaper)
	if strings.Contains(paper, "data-confirm=") {
		t.Error("closing a simulated position prompts")
	}
}

// The square-off control is split per book: flattening simulated positions
// costs nothing and is one click, flattening real ones spends money and keeps
// its prompt. One control doing both forced the careful treatment onto the
// harmless case, which is how an operator learns to click through the prompt
// that matters.
func TestSquareOffButtonsAreSplitPerBook(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	pos := func(sym string, book broker.Book) broker.Position {
		return broker.Position{
			StrategyID: "manual", TradingSymbol: sym, Product: broker.ProductMIS,
			NetQuantity: -65, AveragePrice: 100, Book: book,
		}
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "live", LiveActive: true}, CSRF: "x",
		Data: tradeData{Positions: []broker.Position{
			pos("REAL-CE", broker.BookReal),
			pos("PAPER-CE", broker.BookPaper),
		}}}
	if err := r.Render(w, 200, "trade.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if !strings.Contains(body, `name="book" value="real"`) {
		t.Error("no real-book square-off button")
	}
	if !strings.Contains(body, `name="book" value="paper"`) {
		t.Error("no paper-book square-off button")
	}
	// The paper button must not prompt; the real one must.
	realIdx := strings.Index(body, `value="real"`)
	paperIdx := strings.Index(body, `value="paper"`)
	realForm := body[strings.LastIndex(body[:realIdx], "<form"):realIdx]
	paperForm := body[strings.LastIndex(body[:paperIdx], "<form"):paperIdx]
	if !strings.Contains(realForm, "data-confirm=") {
		t.Error("flattening the REAL book does not confirm")
	}
	if strings.Contains(paperForm, "data-confirm=") {
		t.Error("flattening the paper book prompts; nothing is at stake there")
	}
}

// A book with nothing open offers no button at all, rather than one that
// reports "already flat" after a click.
func TestSquareOffButtonsHiddenWhenABookIsFlat(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x",
		Data: tradeData{Positions: []broker.Position{{
			StrategyID: "s", TradingSymbol: "PAPER-CE", Product: broker.ProductMIS,
			NetQuantity: -65, Book: broker.BookPaper,
		}}}}
	if err := r.Render(w, 200, "trade.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if strings.Contains(body, `name="book" value="real"`) {
		t.Error("offered a real square-off with no real positions open")
	}
	if !strings.Contains(body, `name="book" value="paper"`) {
		t.Error("paper square-off missing when paper positions are open")
	}
}

// The live tab must show the market before it is armed.
//
// Opening /live used to render the arming form and nothing else, so the page was
// useless for the decision it exists to support: whether to arm at all. The
// chain is prices, not a control, so it renders — read-only, with no ticket
// anywhere on the page.
func TestUnarmedLiveDeskShowsAReadOnlyChain(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	// Live: true is what liveData really carries — it forces page=live before
	// building the shared trade data — so this is the state a browser sees.
	v := pageView{Status: app.Status{Mode: "live", LiveArmed: true}, CSRF: "x",
		Data: liveData{
			tradeData: tradeData{Live: true, LiveMode: false, Chain: engine.OptionChain{
				Underlying: "NIFTY", SpotSymbol: "NIFTY 50", Spot: 24512,
				Expiry:      time.Now().AddDate(0, 0, 3),
				Expiries:    []time.Time{time.Now().AddDate(0, 0, 3)},
				Underlyings: []string{"NIFTY"}, ATMStrike: 24500,
				Rows: []engine.ChainRow{{
					Strike: 24500,
					Call:   engine.ChainLeg{TradingSymbol: "NIFTY25AUG24500CE", LastPrice: 120.5, LotSize: 75},
					Put:    engine.ChainLeg{TradingSymbol: "NIFTY25AUG24500PE", LastPrice: 98.25, LotSize: 75},
				}},
			}},
			Configured: true, Armed: false, SessionOK: true,
		}}
	if err := r.Render(w, 200, "live.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	// The prices are there.
	if !strings.Contains(body, "NIFTY25AUG24500CE") {
		t.Error("the unarmed live desk does not show the option chain")
	}
	if !strings.Contains(body, "120.50") {
		t.Error("the chain renders without premiums")
	}

	// ...and cannot be traded from. Both premiums inert, and no ticket at all.
	if strings.Count(body, "disabled") < 2 {
		t.Error("premium cells are still clickable on an unarmed desk")
	}
	if strings.Contains(body, `action="/api/live/orders"`) {
		t.Error("an unarmed live desk rendered a real order ticket")
	}
	if strings.Contains(body, "PLACE REAL ORDER") {
		t.Error("an unarmed live desk offers to place a real order")
	}

	// The gate is still the first thing on the page, above the chain.
	gate := strings.Index(body, "I UNDERSTAND")
	chain := strings.Index(body, "NIFTY25AUG24500CE")
	if gate < 0 || chain < 0 || gate > chain {
		t.Error("the arming form is not above the chain")
	}
}

// Armed, the same chain becomes a picker again — the cells load a contract into
// the real ticket.
func TestArmedLiveDeskChainIsClickable(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "live", LiveActive: true}, CSRF: "x",
		Data: liveData{
			tradeData: tradeData{Live: true, LiveMode: true, Chain: engine.OptionChain{
				Underlying: "NIFTY", SpotSymbol: "NIFTY 50", Spot: 24512,
				Expiry:      time.Now().AddDate(0, 0, 3),
				Expiries:    []time.Time{time.Now().AddDate(0, 0, 3)},
				Underlyings: []string{"NIFTY"}, ATMStrike: 24500,
				Rows: []engine.ChainRow{{
					Strike: 24500,
					Call:   engine.ChainLeg{TradingSymbol: "NIFTY25AUG24500CE", LastPrice: 120.5, LotSize: 75},
					Put:    engine.ChainLeg{TradingSymbol: "NIFTY25AUG24500PE", LastPrice: 98.25, LotSize: 75},
				}},
			}},
			Configured: true, Armed: true, SessionOK: true,
		}}
	if err := r.Render(w, 200, "live.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if !strings.Contains(body, "load into the ticket") {
		t.Error("the armed chain does not offer to load a contract into the ticket")
	}
	if strings.Contains(body, "arm live routing to trade it") {
		t.Error("the armed chain still renders its read-only cells")
	}
}

// ChainReadOnly is derived rather than passed in, so the polled fragment agrees
// with the page that spawned it. The truth table is small and load-bearing: get
// it wrong in one direction and the live desk shows a dead chain when it is
// armed, in the other and it offers contracts it cannot trade.
func TestChainReadOnlyTruthTable(t *testing.T) {
	cases := []struct {
		name string
		d    tradeData
		want bool
	}{
		{"paper terminal is a picker", tradeData{Live: false, LiveMode: false}, false},
		{"live desk before arming is read-only", tradeData{Live: true, LiveMode: false}, true},
		{"live desk once armed is a picker", tradeData{Live: true, LiveMode: true}, false},
		// LiveMode without Live cannot arise from a real request — the live desk
		// is the only page that sets Live — but the paper terminal must stay a
		// picker regardless of what routing is doing elsewhere.
		{"paper terminal while live is armed", tradeData{Live: false, LiveMode: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.ChainReadOnly(); got != tc.want {
				t.Errorf("ChainReadOnly() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A switched-off cap must say so, not print a zero.
//
// A zero limit means "no limit" in risk.Check, but rendered as a bare number it
// says the opposite: "max open positions: 0" reads as a lockout and "max lots per
// trade: 0" as a book that cannot trade at all. The operator turned these caps
// off deliberately and needs the page to confirm that, not to look broken.
func TestRiskPageNamesASwitchedOffCap(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "live"}, CSRF: "x", Data: riskData{
		// Everything off except the daily loss, which is how this box is run.
		Limits:     risk.Limits{MaxDailyLoss: 25000},
		Defaults:   risk.Limits{MaxDailyLoss: 25000},
		LiveLimits: risk.Limits{},
		LivePolicy: "1% of opening balance",
	}}
	if err := r.Render(w, 200, "risk.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	// Three read-only live rows, all off.
	if got := strings.Count(body, "no limit"); got < 3 {
		t.Errorf("risk page said \"no limit\" %d times, want at least 3 — "+
			"a zero cap is being printed as a number", got)
	}
	// The specific misreadings that prompted this — the read-only policy cells.
	// Scoped to those cells rather than the whole page, because a zero elsewhere
	// (an empty order-count badge, say) is perfectly honest.
	for _, bad := range []string{`<td class="mono">0</td>`, `<td class="mono">0.00</td>`} {
		if strings.Contains(body, bad) {
			t.Errorf("the live policy table still renders a bare zero (%s) for a disabled cap", bad)
		}
	}
	// The cap that IS set must still show its value.
	if !strings.Contains(body, "25,000.00") {
		t.Error("the configured daily loss limit is not shown")
	}
}

// The dashboard's one-line summary has the same trap, and it is the line seen
// most often.
func TestDashboardSummaryNamesASwitchedOffCap(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{
		Mode:       "live",
		RiskLimits: risk.Limits{MaxDailyLoss: 25000},
	}, CSRF: "x", Data: dashboardData{}}
	if err := r.Render(w, 200, "dashboard.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if strings.Contains(body, "max 0 lot") || strings.Contains(body, "max 0 open") {
		t.Error("dashboard reports a disabled cap as a cap of zero")
	}
	if !strings.Contains(body, "no limit") {
		t.Error("dashboard does not name the disabled caps")
	}
}

// The ticket must stream its own price, and offer it as the LIMIT price.
//
// data-ltp is how ws.js discovers what to subscribe to, so the attribute is not
// decoration — without it the readout never updates and selecting LIMIT has
// nothing to copy, leaving the operator retyping a premium off the chain into an
// order that spends real money.
func TestTicketCarriesALivePriceCell(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x", Data: tradeData{
		TicketSymbol: "NIFTY25AUG24500CE",
		TicketLot:    75,
		TicketPrice:  120.5,
	}}
	if err := r.Render(w, 200, "trade.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if !strings.Contains(body, `id="ticket-ltp"`) {
		t.Error("the ticket has no price cell for the limit price to be read from")
	}
	if !strings.Contains(body, `data-ltp="NIFTY25AUG24500CE"`) {
		t.Error("the ticket's price cell is not subscribed to the ticket's symbol")
	}
	// Server-rendered, so a LIMIT chosen before the first tick still has a price.
	if !strings.Contains(body, "120.50") {
		t.Errorf("the ticket does not show the contract's current price:\n%s", body)
	}
}

// With no contract chosen the readout is suppressed rather than showing a dash
// above an empty instrument field.
func TestEmptyTicketHidesThePriceCell(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x", Data: tradeData{}}
	if err := r.Render(w, 200, "trade.html", v); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()

	if !strings.Contains(body, "ticket-ltp is-empty") {
		t.Error("the price readout is shown with no contract selected")
	}
	// An empty data-ltp must not become a subscription for the symbol "".
	if strings.Contains(body, `data-ltp=""`) && !strings.Contains(body, "is-empty") {
		t.Error("empty symbol would be subscribed to")
	}
}

// The narrowing filters must load on their own, and must clear what they
// invalidate.
//
// data-cascade is the whole mechanism: it names the fields in narrowing order, so
// changing one clears everything downstream before submitting. Losing the
// attribute silently returns the page to needing a Load click, and losing the
// ORDER silently reintroduces the stale-selection error — switching underlying
// with an old expiry still selected makes /options answer "no NIFTY contracts
// expiring 2026-09-25 in that snapshot" instead of offering the new expiries.
func TestNarrowingFormsDeclareTheirCascade(t *testing.T) {
	r, err := NewRenderer(false)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("options browser", func(t *testing.T) {
		w := httptest.NewRecorder()
		v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x", Data: optionsData{
			Date:        "2026-08-19",
			Underlyings: []string{"NIFTY", "SENSEX"},
			Intervals:   kite.Intervals,
			Interval:    "5minute",
		}}
		if err := r.Render(w, 200, "options.html", v); err != nil {
			t.Fatal(err)
		}
		body := w.Body.String()

		if !strings.Contains(body, `data-cascade="date underlying expiry strike"`) {
			t.Error("the options filters do not declare their narrowing order")
		}
		// The Load button stays: it is the no-JavaScript path.
		if !strings.Contains(body, ">Load<") {
			t.Error("the no-script Load button was removed")
		}
	})

	t.Run("option chain", func(t *testing.T) {
		w := httptest.NewRecorder()
		v := pageView{Status: app.Status{Mode: "paper"}, CSRF: "x", Data: tradeData{
			Chain: engine.OptionChain{
				Underlying: "NIFTY", Expiry: time.Now().AddDate(0, 0, 3),
				Expiries:    []time.Time{time.Now().AddDate(0, 0, 3)},
				Underlyings: []string{"NIFTY"},
			},
		}}
		if err := r.Render(w, 200, "chain_fragment.html", v); err != nil {
			t.Fatal(err)
		}
		body := w.Body.String()

		if !strings.Contains(body, `data-cascade="underlying expiry"`) {
			t.Error("the chain selectors do not declare their narrowing order")
		}
	})
}
