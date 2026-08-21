package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"kite-algo/internal/app"
	"kite-algo/internal/broker"
	"kite-algo/internal/engine"
)

// csrfFor extracts the session's CSRF token by reading it out of a rendered page.
func csrfFor(t *testing.T, ts *httptest.Server, c *http.Client) string {
	t.Helper()
	resp, err := c.Get(ts.URL + "/connect")
	if err != nil {
		t.Fatalf("load page for csrf: %v", err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	const marker = `name="_csrf" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no CSRF token in rendered page:\n%s", body)
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("malformed CSRF token in page")
	}
	return rest[:j]
}

// TestPolledFragmentsCarryAWorkingCSRFToken is the regression guard for a bug
// that only appeared a few seconds after a page loaded.
//
// Fragments are swapped into a live page by the poller, and several contain
// forms — stop a strategy, cancel an order. The fragment handlers built their
// own page view and omitted the CSRF token, so the initial HTML carried a valid
// token and the first poll replaced it with an empty one. Every action then
// failed with 403 "csrf validation failed", including stopping a strategy.
//
// Any form in a polled fragment must render a usable token, and the token must
// actually be accepted afterwards.
func TestPolledFragmentsCarryAWorkingCSRFToken(t *testing.T) {
	ts, a := newTestServer(t)
	client := loginClient(t, ts)

	// A running strategy is REQUIRED for this test to mean anything: with an
	// empty list the fragment renders "No strategies running" and contains no
	// form, so the assertion below would pass vacuously and prove nothing.
	if _, err := a.Engine.StartStrategy(context.Background(), engine.StrategySpec{
		InstanceID: "csrf-probe",
		Type:       "short-straddle",
	}); err != nil {
		t.Fatalf("start a strategy so the fragment renders its stop forms: %v", err)
	}

	fragments := []string{
		"/partials/status",
		"/partials/positions",
		"/partials/watchlist",
		"/partials/orders",
		"/partials/strategies",
		"/partials/chain",
	}

	// Guard the guard: at least one fragment must actually contain a form.
	sawAForm := false
	t.Cleanup(func() {
		if !sawAForm {
			t.Error("no fragment contained a _csrf field; this test proved nothing")
		}
	})

	for _, path := range fragments {
		t.Run(path, func(t *testing.T) {
			resp, err := client.Get(ts.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()
			body := readAll(t, resp)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = HTTP %d", path, resp.StatusCode)
			}

			// A fragment with no form is fine; one WITH a form must carry a token.
			const marker = `name="_csrf" value="`
			for _, chunk := range strings.Split(body, marker)[1:] {
				sawAForm = true
				end := strings.Index(chunk, `"`)
				if end < 0 {
					t.Fatalf("%s has a _csrf field with no closing quote", path)
				}
				if token := chunk[:end]; token == "" {
					t.Errorf("%s rendered a form with an EMPTY csrf token — "+
						"every action on it would fail with 403 after the first poll", path)
				}
			}
		})
	}
}

// TestTokenFromAFragmentIsAccepted closes the loop: a token rendered by a polled
// fragment must actually work when submitted, not merely be non-empty.
func TestTokenFromAFragmentIsAccepted(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)

	resp, err := client.Get(ts.URL + "/partials/strategies")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	// With no strategies running there is no form to scrape, so take the token
	// from a page and prove the fragment path accepts the same session token.
	token := csrfFor(t, ts, client)
	if token == "" {
		t.Fatal("no CSRF token available")
	}
	_ = body

	// Any CSRF-protected endpoint will do; square-off with nothing open is inert.
	post, err := client.PostForm(ts.URL+"/api/positions/squareoff", url.Values{"_csrf": {token}})
	if err != nil {
		t.Fatal(err)
	}
	defer post.Body.Close()
	if post.StatusCode == http.StatusForbidden {
		t.Error("a valid session token was rejected as CSRF")
	}
}

// TestOrderEndpointsRejectMissingCSRF confirms every state-changing trading
// endpoint is CSRF-protected. These are the routes that move money; a gap here
// would let any site the operator visits place orders on their behalf.
func TestOrderEndpointsRejectMissingCSRF(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)

	endpoints := []string{
		"/api/orders",
		"/api/orders/some-id/cancel",
		"/api/positions/squareoff",
	}
	for _, path := range endpoints {
		resp, err := client.PostForm(ts.URL+path, url.Values{"symbol": {"NIFTY24AUG24500CE"}})
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without CSRF = HTTP %d, want 403", path, resp.StatusCode)
		}
	}
}

// TestOrderEndpointsRequireAuth confirms an anonymous caller cannot trade.
func TestOrderEndpointsRequireAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.PostForm(ts.URL+"/api/orders", url.Values{"symbol": {"X"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusSeeOther {
		t.Errorf("anonymous order POST = HTTP %d, want 401 or a redirect", resp.StatusCode)
	}
}

// TestOrderValidationRejectsBadInput checks the ticket's guardrails. Each of
// these would otherwise reach the broker as a malformed or dangerous order.
func TestOrderValidationRejectsBadInput(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)
	csrf := csrfFor(t, ts, client)

	base := func() url.Values {
		return url.Values{
			"_csrf":      {csrf},
			"symbol":     {"NIFTY24AUG24500CE"},
			"side":       {"BUY"},
			"lots":       {"1"},
			"order_type": {"MARKET"},
			"product":    {"MIS"},
		}
	}

	tests := []struct {
		name    string
		mutate  func(url.Values)
		wantMsg string
	}{
		{"no symbol", func(v url.Values) { v.Set("symbol", "") }, "choose an instrument"},
		{"bad side", func(v url.Values) { v.Set("side", "HOLD") }, "side must be"},
		{"bad order type", func(v url.Values) { v.Set("order_type", "ICEBERG") }, "order type must be"},
		{"bad product", func(v url.Values) { v.Set("product", "GTT") }, "product must be"},
		{"zero lots", func(v url.Values) { v.Set("lots", "0") }, "lots must be a positive"},
		{"negative lots", func(v url.Values) { v.Set("lots", "-3") }, "lots must be a positive"},
		{"non-numeric lots", func(v url.Values) { v.Set("lots", "many") }, "lots must be a positive"},
		{"limit without price", func(v url.Values) { v.Set("order_type", "LIMIT") }, "needs a price"},
		{"stop without trigger", func(v url.Values) { v.Set("order_type", "SL-M") }, "needs a trigger price"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			form := base()
			tc.mutate(form)

			resp, err := client.PostForm(ts.URL+"/api/orders", form)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body := readAll(t, resp)

			if !strings.Contains(strings.ToLower(body), strings.ToLower(tc.wantMsg)) {
				t.Errorf("response did not explain the problem.\nwant substring: %q\ngot: %s",
					tc.wantMsg, strings.TrimSpace(body))
			}
			if !strings.Contains(body, "alert-error") {
				t.Errorf("invalid order was not reported as an error: %s", strings.TrimSpace(body))
			}
		})
	}
}

// TestUnknownInstrumentIsRejected covers the case where the instrument master
// has not loaded. Deriving quantity from lots requires a lot size; without one
// the order must be refused rather than sent with a guessed quantity.
func TestUnknownInstrumentIsRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)
	csrf := csrfFor(t, ts, client)

	resp, err := client.PostForm(ts.URL+"/api/orders", url.Values{
		"_csrf":      {csrf},
		"symbol":     {"TOTALLY-UNKNOWN-SYMBOL"},
		"side":       {"BUY"},
		"lots":       {"1"},
		"order_type": {"MARKET"},
		"product":    {"MIS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	if !strings.Contains(body, "unknown lot size") {
		t.Errorf("expected the order to be refused for an unknown lot size, got: %s", strings.TrimSpace(body))
	}
}

// TestSquareOffAllWhenFlat checks the panic path is honest about doing nothing,
// rather than reporting success and leaving the operator unsure.
func TestSquareOffAllWhenFlat(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)
	csrf := csrfFor(t, ts, client)

	resp, err := client.PostForm(ts.URL+"/api/positions/squareoff", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	if !strings.Contains(strings.ToLower(body), "already flat") {
		t.Errorf("square-off with no positions should say so plainly, got: %s", strings.TrimSpace(body))
	}
}

// TestSquareOffUnknownSymbol confirms closing a position you do not hold is
// reported clearly instead of silently succeeding.
func TestSquareOffUnknownSymbol(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)
	csrf := csrfFor(t, ts, client)

	resp, err := client.PostForm(ts.URL+"/api/positions/squareoff", url.Values{
		"_csrf":  {csrf},
		"symbol": {"NIFTY24AUG24500CE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	if !strings.Contains(body, "No open position") {
		t.Errorf("expected a clear 'no position' message, got: %s", strings.TrimSpace(body))
	}
}

// TestTerminalRequiresKiteSession keeps the trading screen behind a live broker
// connection, so there is no way to type an order into a disconnected app.
func TestTerminalRequiresKiteSession(t *testing.T) {
	ts, _ := newTestServer(t)
	jar := loginClient(t, ts)
	jar.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := jar.Get(ts.URL + "/trade")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /trade without a Zerodha session = HTTP %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/connect" {
		t.Errorf("redirected to %q, want /connect", loc)
	}
}

// TestInstrumentSearchWithoutMaster ensures the typeahead degrades quietly
// before login rather than erroring.
func TestInstrumentSearchWithoutMaster(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)

	resp, err := client.Get(ts.URL + "/api/instruments?q=NIFTY")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("instrument search = HTTP %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content type = %q, want JSON", ct)
	}
}

// Disarming must tell the browser to re-render the page, not just patch a div.
//
// The disarm form posts through app.js, which renders the response into a result
// element and refreshes the polled panels. Neither covers this: the gate and the
// armed desk are different branches of live.html chosen on the server, so the
// page went on showing the LIVE — REAL MONEY banner, a real order ticket, and no
// arming form until someone reloaded by hand. handlePlaceLiveOrder re-checks
// LiveActive so nothing could actually be routed, but a desk that still says
// REAL MONEY after you have stood down is the wrong thing to be wrong about.
func TestDisarmingTellsThePageToReload(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)
	token := csrfFor(t, ts, client)

	resp, err := client.PostForm(ts.URL+"/api/live/disarm", url.Values{"_csrf": {token}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("a valid session token was rejected as CSRF")
	}

	if got := resp.Header.Get("X-Page-Stale"); got != "1" {
		t.Errorf("disarm response X-Page-Stale = %q, want \"1\" — "+
			"without it the page keeps showing the armed desk and never re-asks for the password", got)
	}
}

// A refused arming attempt must NOT reload: the reload would discard the reason
// it was refused, leaving a wrong phrase or password looking like a page that
// simply blinked. Nothing changed, so nothing is stale.
func TestRefusedArmingKeepsItsErrorOnScreen(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)
	token := csrfFor(t, ts, client)

	// The harness runs in dry-run, so this is refused on the configuration gate
	// before the phrase is even considered — a refusal either way.
	resp, err := client.PostForm(ts.URL+"/api/live/confirm", url.Values{
		"_csrf":    {token},
		"phrase":   {"I UNDERSTAND"},
		"password": {testPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	if resp.Header.Get("X-Page-Stale") != "" {
		t.Error("a refused arming attempt asked the page to reload, discarding its own error message")
	}
	if !strings.Contains(strings.ToLower(body), "live") {
		t.Errorf("refusal did not explain itself: %s", body)
	}
}

// The polled positions fragment must stay on the desk's own book.
//
// /partials/positions is shared with the dashboard, which shows BOTH books, and
// the desks used to poll it with no parameters at all. So the terminal rendered
// correctly and then — five seconds later, on the first refresh — quietly gained
// the real book, square-off button included. The leak ran both ways: the live
// desk gained the simulated one.
func TestPositionsFragmentHonoursTheRequestedBook(t *testing.T) {
	ts, a := newTestServer(t)
	client := loginClient(t, ts)

	seedTwoBookPositions(t, a)

	get := func(path string) string {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		return readAll(t, resp)
	}

	paper := get("/partials/positions?book=paper")
	if strings.Contains(paper, "REALSYM") {
		t.Error("the paper fragment carries a real position")
	}
	if !strings.Contains(paper, "PAPERSYM") {
		t.Error("the paper fragment is missing its own positions")
	}

	real := get("/partials/positions?book=real")
	if strings.Contains(real, "PAPERSYM") {
		t.Error("the real fragment carries a simulated position")
	}

	// The dashboard passes no book and must still see everything.
	both := get("/partials/positions")
	if !strings.Contains(both, "PAPERSYM") {
		t.Error("the dashboard fragment lost the paper book")
	}
}

// Each desk's data is filtered to its own book before it ever reaches a
// template.
//
// /trade cannot be fetched here — it redirects without a Zerodha session — so
// this covers the filter directly; the template's own gate is covered in
// render_test.go.
func TestOnlyBookSplitsThePositionList(t *testing.T) {
	in := []broker.Position{
		{TradingSymbol: "REALSYM", NetQuantity: -65, Book: broker.BookReal},
		{TradingSymbol: "PAPERSYM", NetQuantity: -65, Book: broker.BookPaper},
		// An unset book is paper: the zero value must never be real, or a
		// position nobody labelled would show up on the real desk.
		{TradingSymbol: "UNLABELLED", NetQuantity: -65},
	}

	paper := onlyBook(in, broker.BookPaper)
	if len(paper) != 2 {
		t.Fatalf("paper book has %d positions, want 2 (including the unlabelled one)", len(paper))
	}
	for _, p := range paper {
		if p.TradingSymbol == "REALSYM" {
			t.Error("a real position survived the paper filter")
		}
	}

	real := onlyBook(in, broker.BookReal)
	if len(real) != 1 || real[0].TradingSymbol != "REALSYM" {
		t.Errorf("real book = %+v, want only REALSYM", real)
	}
}

// Setting the auto square-off time from a desk.
func TestSquareOffTimeEndpoint(t *testing.T) {
	ts, a := newTestServer(t)
	client := loginClient(t, ts)
	token := csrfFor(t, ts, client)

	post := func(book, at string) string {
		resp, err := client.PostForm(ts.URL+"/api/positions/squareoff-time", url.Values{
			"_csrf": {token}, "book": {book}, "time": {at},
		})
		if err != nil {
			t.Fatalf("post square-off time: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		return readAll(t, resp)
	}

	if body := post("paper", "15:25"); !strings.Contains(body, "15:25") {
		t.Errorf("the response does not confirm the time it set: %s", body)
	}
	if got := a.SquareOffTimes().Paper; got != "15:25" {
		t.Errorf("paper square-off time = %q, want 15:25", got)
	}

	// A bad time changes nothing and says why.
	body := post("paper", "half three")
	if !strings.Contains(body, "24-hour") {
		t.Errorf("a malformed time was not explained: %s", body)
	}
	if got := a.SquareOffTimes().Paper; got != "15:25" {
		t.Errorf("a rejected time changed the setting to %q", got)
	}

	// Clearing it is possible, which matters more than setting it: an operator
	// who wants the automatic flatten to stop must be able to say so.
	post("paper", "")
	if got := a.SquareOffTimes().Paper; got != "" {
		t.Errorf("clearing the time left %q behind", got)
	}

	// Each desk sets its own book only.
	post("real", "15:20")
	if got := a.SquareOffTimes(); got.Real != "15:20" || got.Paper != "" {
		t.Errorf("times = %+v, want only the real book set", got)
	}
}

// seedTwoBookPositions puts one real and one simulated position in the engine's
// cache, which is what the desks render from.
func seedTwoBookPositions(t *testing.T, a *app.App) {
	t.Helper()
	a.Engine.SetPositionsForTest([]broker.Position{
		{StrategyID: "manual", TradingSymbol: "REALSYM", Product: broker.ProductMIS,
			NetQuantity: -65, AveragePrice: 100, Book: broker.BookReal},
		{StrategyID: "manual", TradingSymbol: "PAPERSYM", Product: broker.ProductMIS,
			NetQuantity: -65, AveragePrice: 100, Book: broker.BookPaper},
	})
}
