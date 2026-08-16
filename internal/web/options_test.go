package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"kite-algo/internal/app"
	"kite-algo/internal/history"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage"
)

// seedOptionDay writes an instrument snapshot and a day of candles for a
// NIFTY straddle, mimicking what the capture job stores.
func seedOptionDay(t *testing.T, a *app.App, day time.Time) {
	t.Helper()
	store, ok := a.Store.(storage.HistoryStore)
	if !ok {
		t.Fatal("store is not a HistoryStore")
	}
	ctx := context.Background()
	expiry := time.Date(2026, 8, 18, 15, 30, 0, 0, history.IST)

	rows := []storage.InstrumentRow{
		{InstrumentToken: 1, TradingSymbol: "NIFTY2681824350CE", Name: "NIFTY",
			Expiry: expiry, Strike: 24350, LotSize: 65, InstrumentType: "CE",
			Segment: "NFO-OPT", Exchange: "NFO"},
		{InstrumentToken: 2, TradingSymbol: "NIFTY2681824350PE", Name: "NIFTY",
			Expiry: expiry, Strike: 24350, LotSize: 65, InstrumentType: "PE",
			Segment: "NFO-OPT", Exchange: "NFO"},
		{InstrumentToken: 3, TradingSymbol: "NIFTY2681824400CE", Name: "NIFTY",
			Expiry: expiry, Strike: 24400, LotSize: 65, InstrumentType: "CE",
			Segment: "NFO-OPT", Exchange: "NFO"},
	}
	if err := store.SaveInstrumentSnapshot(ctx, day, rows); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	open := time.Date(day.Year(), day.Month(), day.Day(), 9, 15, 0, 0, history.IST)
	var candles []marketdata.Candle
	add := func(sym string, tok uint32, base float64) {
		for i := 0; i < 6; i++ {
			at := open.Add(time.Duration(i) * 5 * time.Minute)
			p := base + float64(i)
			candles = append(candles, marketdata.Candle{
				InstrumentToken: tok, TradingSymbol: sym, Interval: "5minute",
				Open: p, High: p + 2, Low: p - 2, Close: p,
				Volume: 1000, OpenInterest: 50000,
				OpenTime: at, CloseTime: at.Add(5 * time.Minute),
			})
		}
	}
	add("NIFTY 50", 256265, 24361)
	add("NIFTY2681824350CE", 1, 146)
	add("NIFTY2681824350PE", 2, 74)

	if err := store.SaveCandles(ctx, candles); err != nil {
		t.Fatalf("save candles: %v", err)
	}
}

// getStatusBody is getBody plus the status code, for the endpoints where a
// non-200 is the thing under test.
func getStatusBody(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestOptionsPageNarrowsChoicesFromTheSnapshot(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	day := time.Date(2026, 8, 14, 0, 0, 0, 0, history.IST)
	seedOptionDay(t, a, day)

	// Day alone: offers the underlyings that existed.
	code, body := getStatusBody(t, c, ts.URL+"/options?date=2026-08-14")
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if !strings.Contains(body, "NIFTY") {
		t.Error("underlying NIFTY not offered for a day that has a snapshot")
	}

	// Plus underlying: offers that day's expiries.
	body = getBody(t, c, ts.URL+"/options?date=2026-08-14&underlying=NIFTY")
	if !strings.Contains(body, "2026-08-18") {
		t.Error("expiry 2026-08-18 not offered")
	}

	// Plus expiry: offers strikes.
	body = getBody(t, c, ts.URL+"/options?date=2026-08-14&underlying=NIFTY&expiry=2026-08-18")
	if !strings.Contains(body, "24350") {
		t.Error("strike 24350 not offered")
	}
}

func TestOptionsPageShowsBothLegsAndDerivedGreeks(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	day := time.Date(2026, 8, 14, 0, 0, 0, 0, history.IST)
	seedOptionDay(t, a, day)

	code, body := getStatusBody(t, c,
		ts.URL+"/options?date=2026-08-14&underlying=NIFTY&expiry=2026-08-18&strike=24350&interval=5minute")
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	for _, want := range []string{"NIFTY2681824350CE", "NIFTY2681824350PE"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s missing from the page", want)
		}
	}
	// Greeks are derived, so the header only appears when a row actually priced.
	if !strings.Contains(body, "Call IV") {
		t.Error("greek columns absent; implied vol did not solve from the stored premiums")
	}
	if !strings.Contains(body, "Straddle") {
		t.Error("straddle delta column missing")
	}
}

// The reason this page exists: /research resolves through the LIVE master and
// would fail here, because there is no Kite session in this test at all and the
// contract may well have expired. Reading from storage via the snapshot must
// work regardless.
func TestOptionsPageWorksWithNoKiteSession(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	day := time.Date(2026, 8, 14, 0, 0, 0, 0, history.IST)
	seedOptionDay(t, a, day)

	if a.Kite.Snapshot().Connected() {
		t.Fatal("test server unexpectedly has a live Kite session")
	}
	code, body := getStatusBody(t, c,
		ts.URL+"/options?date=2026-08-14&underlying=NIFTY&expiry=2026-08-18&strike=24350&interval=5minute")
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if strings.Contains(body, "log in first") {
		t.Error("page demanded a Zerodha session to read locally stored data")
	}
	if !strings.Contains(body, "146.00") {
		t.Error("stored call premium not rendered")
	}
}

// A day before capture began has no snapshot, and that must be said plainly
// rather than rendering an empty page that looks like "no trades happened".
func TestOptionsPageExplainsAMissingSnapshot(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	seedOptionDay(t, a, time.Date(2026, 8, 14, 0, 0, 0, 0, history.IST))

	body := getBody(t, c, ts.URL+"/options?date=2026-07-15")
	if !strings.Contains(body, "no instrument snapshot") {
		t.Errorf("missing snapshot not explained; body was:\n%s", truncate(body, 600))
	}
}

// An uncaptured strike on a day that WAS captured is a gap in the strike
// window. Saying only "nothing captured" would read as "capture is broken".
func TestOptionsPageWarnsAboutAnUncapturedStrike(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	day := time.Date(2026, 8, 14, 0, 0, 0, 0, history.IST)
	seedOptionDay(t, a, day)

	body := getBody(t, c,
		ts.URL+"/options?date=2026-08-14&underlying=NIFTY&expiry=2026-08-18&strike=24400&interval=5minute")
	if !strings.Contains(body, "fell outside that day&#39;s capture window") {
		t.Errorf("no per-strike warning; body was:\n%s", truncate(body, 800))
	}
	if strings.Contains(body, "did not run that day") {
		t.Error("blamed the scheduler for a day it demonstrably captured")
	}
}

// A day capture never touched is a different problem with a different fix, and
// the page must not conflate the two.
func TestOptionsPageDistinguishesADayCaptureNeverRan(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	day := time.Date(2026, 8, 14, 0, 0, 0, 0, history.IST)
	seedOptionDay(t, a, day)

	// The snapshot exists, but no candles were stored at this interval.
	body := getBody(t, c,
		ts.URL+"/options?date=2026-08-14&underlying=NIFTY&expiry=2026-08-18&strike=24350&interval=15minute")
	if !strings.Contains(body, "did not run that day") {
		t.Errorf("did not identify a day with no capture at all; body was:\n%s", truncate(body, 800))
	}
}

// Marking which strikes hold data is what turns the dropdown from a chain
// listing into a data listing.
func TestOptionsPageMarksCapturedStrikes(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	day := time.Date(2026, 8, 14, 0, 0, 0, 0, history.IST)
	seedOptionDay(t, a, day)

	body := getBody(t, c,
		ts.URL+"/options?date=2026-08-14&underlying=NIFTY&expiry=2026-08-18&interval=5minute")
	// 24350 has bars; 24400 is in the chain but was never captured.
	if !strings.Contains(body, "24350 ●") {
		t.Error("captured strike 24350 not marked")
	}
	if strings.Contains(body, "24400 ●") {
		t.Error("uncaptured strike 24400 marked as having data")
	}
}

func TestOptionCandlesJSONServesTheSelectedLeg(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	day := time.Date(2026, 8, 14, 0, 0, 0, 0, history.IST)
	seedOptionDay(t, a, day)

	code, body := getStatusBody(t, c,
		ts.URL+"/api/option-candles?date=2026-08-14&underlying=NIFTY&expiry=2026-08-18&strike=24350&interval=5minute&type=PE")
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", code, body)
	}
	// The put series starts at 74; the call at 146. Getting the wrong one back
	// would chart a contract the table never showed.
	if !strings.Contains(body, `"c":74`) {
		t.Errorf("put series not returned, got: %s", truncate(body, 300))
	}
	if strings.Contains(body, `"c":146`) {
		t.Error("call bars leaked into the put series")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// An NFO snapshot holds ~220 underlyings, nearly all single-stock options.
// Alphabetically NIFTY lands between NHPC and NMDC, which buries the only entry
// most people open this page for.
func TestIndicesAreOfferedFirst(t *testing.T) {
	got := indicesFirst([]string{"ABB", "BANKNIFTY", "NHPC", "NIFTY", "NMDC", "SENSEX", "ZEEL"})
	want := []string{"NIFTY", "BANKNIFTY", "SENSEX", "ABB", "NHPC", "NMDC", "ZEEL"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("indicesFirst() = %v, want %v", got, want)
		}
	}
	if len(got) != len(want) {
		t.Errorf("dropped or duplicated entries: got %d, want %d", len(got), len(want))
	}
}

// The nav and the mode band must sit inside the sticky wrapper: the nav so any
// page stays one click away from the bottom of a long table, and the band
// because "simulated or real?" must be answerable at every scroll position.
func TestLayoutPinsNavAndModeBand(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loginClient(t, ts)

	body := getBody(t, c, ts.URL+"/options")

	open := strings.Index(body, `<div class="chrome">`)
	if open < 0 {
		t.Fatal("sticky chrome wrapper missing from the layout")
	}
	close := strings.Index(body, `</div>{{/* .chrome */}}`)
	if close < 0 {
		close = strings.Index(body[open:], "<div id=\"toasts\"")
		if close < 0 {
			t.Fatal("could not find the end of the chrome wrapper")
		}
		close += open
	}
	chrome := body[open:close]

	if !strings.Contains(chrome, "modeband") {
		t.Error("mode band is outside the sticky chrome; it scrolls away")
	}
	if !strings.Contains(chrome, `href="/"`) || !strings.Contains(chrome, "Dashboard") {
		t.Error("nav is outside the sticky chrome; Dashboard scrolls away")
	}
	if !strings.Contains(chrome, "</header>") {
		t.Error("topbar is not enclosed by the sticky chrome")
	}
}

// Sticky positioning keeps the header in normal flow, unlike fixed. The
// terminal sizes its panels with calc(100vh - 260px) on that assumption, so a
// switch to fixed would silently overlap the first rows of every table.
func TestChromeIsStickyNotFixed(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loginClient(t, ts)

	css := getBody(t, c, ts.URL+"/static/app.css")
	i := strings.Index(css, ".chrome {")
	if i < 0 {
		t.Fatal(".chrome rule missing from the stylesheet")
	}
	rule := css[i : i+strings.Index(css[i:], "}")]
	if !strings.Contains(rule, "position: sticky") {
		t.Errorf(".chrome is not sticky:\n%s", rule)
	}
	if strings.Contains(rule, "position: fixed") {
		t.Error(".chrome is fixed; it would overlap content and break panel sizing")
	}
	if !strings.Contains(rule, "background") {
		t.Error(".chrome has no opaque background; content will show through it")
	}
}
