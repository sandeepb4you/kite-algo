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
		{"strategy_new.html", strategy.Descriptor{
			Type: "short-straddle", Title: "Short straddle",
			Params: []strategy.ParamSpec{
				{Key: "lots", Label: "Lots", Kind: strategy.KindInt, Default: 1},
				{Key: "product", Label: "Product", Kind: strategy.KindEnum,
					Options: []string{"MIS", "NRML"}, Default: "MIS"},
				{Key: "square_off_time", Label: "Square off", Kind: strategy.KindTime, Default: "15:15"},
				{Key: "exit_delta", Label: "Exit delta", Kind: strategy.KindFloat, Default: 0.25},
				{Key: "underlying", Label: "Underlying", Kind: strategy.KindString, Default: "NIFTY"},
			},
		}},
		{"risk.html", risk.Limits{MaxDailyLoss: 5000, MaxOrderValue: 100000, MaxLotsPerTrade: 1, MaxOpenPositions: 5}},
		{"research.html", researchData{
			Symbol: "NIFTY 50", Interval: "5minute", From: "2024-08-01", To: "2024-08-02",
			Intervals: kite.Intervals, Snapshots: snapshotInfo{HaveToday: false, AsOf: "2024-08-01"},
		}},
		{"backtest.html", backtestData{
			Available: []strategy.Descriptor{{Type: "short-straddle", Title: "Short straddle"}},
			Intervals: kite.Intervals,
			Paths:     []backtest.BarPath{backtest.PathPessimist},
			Interval:  "5minute", From: "2024-08-01", To: "2024-08-02",
			Capital: "100000", Lots: "1",
		}},
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
			if st.LiveActive && !strings.Contains(body, "LIVE TRADING") {
				t.Error("live session did not render the LIVE banner")
			}
			if st.Halt.Halted && !strings.Contains(body, "TRADING HALTED") {
				t.Error("halted session did not render the halt banner")
			}
		})
	}
}
