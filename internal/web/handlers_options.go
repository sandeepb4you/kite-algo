package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"kite-algo/internal/history"
	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/options"
	"kite-algo/internal/storage"
)

// optionsData drives the captured-option-price viewer.
//
// This page exists because /research cannot show an expired contract. That page
// resolves symbols through the LIVE instrument master and will fetch from Kite
// on a miss — and an expired option is absent from the master and unfetchable
// from the API. Precisely the data the capture job works hardest to preserve
// would be invisible. So this one resolves through the day's INSTRUMENT
// SNAPSHOT and reads only from local storage.
type optionsData struct {
	// Form state.
	Date       string
	Underlying string
	Expiry     string
	Strike     string
	Interval   string

	// Choices, each narrowed by the selection above it.
	Underlyings []string
	Expiries    []string
	Strikes     []strikeChoice
	Intervals   []kite.Interval

	// CapturedStrikes counts how many of Strikes actually hold data, which is
	// what tells an operator at a glance whether capture ran that day at all.
	CapturedStrikes int

	// Results.
	Call     *legSeries
	Put      *legSeries
	Spot     []marketdata.Candle
	SpotName string
	Rows     []optionRow

	SnapshotDate string
	Error        string
	Warning      string

	// Capture is the status panel, so the answer to "why is there no data" and
	// the button that fixes it sit on the same page as the question.
	Capture captureView
}

// strikeChoice is one strike in the dropdown, flagged with whether anything was
// actually captured for it on the chosen day.
//
// Without the flag the list is a chain listing, not a data listing: every strike
// the exchange offered appears, and picking one that was never captured looks
// identical to picking one that had no trades. On a day capture did not run,
// that is every strike.
type strikeChoice struct {
	Value    string
	Captured bool
}

// Label renders the strike with a marker for captured data.
func (s strikeChoice) Label() string {
	if s.Captured {
		return s.Value + " ●"
	}
	return s.Value
}

// legSeries is one contract's captured series.
type legSeries struct {
	Symbol   string
	Type     string
	Strike   float64
	Expiry   time.Time
	LotSize  int
	Candles  []marketdata.Candle
	Coverage string
}

// optionRow is one timestamp across both legs, with greeks derived on the fly.
//
// Greeks are NOT stored. Kite's historical endpoint returns OHLC, volume and
// open interest — there is no greek in the feed, and there is nothing to
// capture. They are reconstructed here from premium, spot, strike and time to
// expiry using the same Black-Scholes code the live strategy runs, which is the
// only way the numbers a backtest sees can match the numbers that drove the
// trade.
type optionRow struct {
	Time time.Time
	Spot float64

	CallPrice float64
	CallOI    int64
	CallIV    float64
	CallDelta float64
	CallGamma float64
	CallTheta float64
	CallVega  float64
	CallOK    bool

	PutPrice float64
	PutOI    int64
	PutIV    float64
	PutDelta float64
	PutGamma float64
	PutTheta float64
	PutVega  float64
	PutOK    bool

	// StraddleDelta is the net delta of one short straddle at this strike, the
	// figure the delta-managed strategy actually keys its exit off.
	StraddleDelta float64
	StraddleOK    bool
}

// handleOptions renders the captured option-price viewer.
func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := optionsData{
		Date:       strings.TrimSpace(q.Get("date")),
		Underlying: strings.ToUpper(strings.TrimSpace(q.Get("underlying"))),
		Expiry:     strings.TrimSpace(q.Get("expiry")),
		Strike:     strings.TrimSpace(q.Get("strike")),
		Interval:   q.Get("interval"),
		Intervals:  kite.Intervals,
	}
	if data.Interval == "" {
		data.Interval = string(kite.Interval5Minute)
	}
	if data.Date == "" {
		data.Date = time.Now().In(history.IST).Format("2006-01-02")
	}

	if err := s.fillOptions(r, &data); err != nil {
		data.Error = err.Error()
	}
	data.Capture = s.captureStatusView(r)
	s.renderPage(w, r, "options.html", "Options", data)
}

// fillOptions resolves the form against the snapshot and loads what is stored.
//
// Each step narrows the next, so the page is usable without knowing any symbol:
// pick a day, and it offers the underlyings that existed; pick one, and it
// offers that day's expiries; and so on.
func (s *Server) fillOptions(r *http.Request, d *optionsData) error {
	store, ok := s.app.Store.(storage.HistoryStore)
	if !ok {
		return fmt.Errorf("this storage backend cannot serve historical data")
	}

	day, err := time.ParseInLocation("2006-01-02", d.Date, history.IST)
	if err != nil {
		return fmt.Errorf("'date' must be a date like 2026-08-14")
	}

	master, err := history.LoadAsOf(r.Context(), store, day)
	if err != nil {
		return err
	}
	d.SnapshotDate = master.Date().In(history.IST).Format("2006-01-02")

	d.Underlyings = indicesFirst(master.Underlyings())
	if d.Underlying == "" {
		return nil
	}

	expiries := master.Expiries(d.Underlying)
	for _, e := range expiries {
		d.Expiries = append(d.Expiries, e.Format("2006-01-02"))
	}
	if len(expiries) == 0 {
		return fmt.Errorf("no %s options in the %s snapshot", d.Underlying, d.SnapshotDate)
	}
	if d.Expiry == "" {
		return nil
	}

	expiry, err := time.ParseInLocation("2006-01-02", d.Expiry, history.IST)
	if err != nil {
		return fmt.Errorf("'expiry' must be a date like 2026-08-18")
	}
	chain := master.Chain(d.Underlying, expiry)
	if len(chain) == 0 {
		return fmt.Errorf("no %s contracts expiring %s in that snapshot", d.Underlying, d.Expiry)
	}

	// One query answers "what was captured that day" for the whole chain; asking
	// per strike would be hundreds of round trips per page load.
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, history.IST)
	dayEnd := dayStart.AddDate(0, 0, 1)
	stored, err := store.CapturedSymbols(r.Context(), d.Interval, dayStart, dayEnd)
	if err != nil {
		return err
	}
	haveData := make(map[string]bool, len(stored))
	for _, sym := range stored {
		haveData[sym] = true
	}

	// A strike counts as captured if either leg has bars.
	capturedStrike := map[float64]bool{}
	for _, c := range chain {
		if haveData[c.TradingSymbol] {
			capturedStrike[c.Strike] = true
		}
	}

	seen := map[float64]bool{}
	for _, c := range chain {
		if !seen[c.Strike] {
			seen[c.Strike] = true
			d.Strikes = append(d.Strikes, strikeChoice{
				Value:    strconv.FormatFloat(c.Strike, 'f', -1, 64),
				Captured: capturedStrike[c.Strike],
			})
		}
	}
	sort.Slice(d.Strikes, func(i, j int) bool {
		a, _ := strconv.ParseFloat(d.Strikes[i].Value, 64)
		b, _ := strconv.ParseFloat(d.Strikes[j].Value, 64)
		return a < b
	})
	d.CapturedStrikes = len(capturedStrike)

	if d.Strike == "" {
		return nil
	}

	strike, err := strconv.ParseFloat(d.Strike, 64)
	if err != nil {
		return fmt.Errorf("'strike' must be a number")
	}

	// Resolve both legs at this strike from the snapshot.
	for _, c := range chain {
		if c.Strike != strike {
			continue
		}
		leg := &legSeries{
			Symbol: c.TradingSymbol, Type: c.InstrumentType,
			Strike: c.Strike, Expiry: c.Expiry, LotSize: c.LotSize,
		}
		switch c.InstrumentType {
		case "CE":
			d.Call = leg
		case "PE":
			d.Put = leg
		}
	}
	if d.Call == nil && d.Put == nil {
		return fmt.Errorf("strike %s not found in the %s chain", d.Strike, d.Expiry)
	}

	// Load the session's bars from storage ONLY. No provider, no upstream: this
	// page reports what was captured, and a silent Kite fetch would both fail on
	// expired contracts and make a gap in the capture look like data.
	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, history.IST)
	to := from.AddDate(0, 0, 1)

	for _, leg := range []*legSeries{d.Call, d.Put} {
		if leg == nil {
			continue
		}
		candles, err := store.GetCandles(r.Context(), leg.Symbol, d.Interval, from, to)
		if err != nil {
			return err
		}
		leg.Candles = candles
		leg.Coverage = describeCoverage(r.Context(), store, leg.Symbol, d.Interval)
	}

	// The spot series both prices the greeks and gives the page its context.
	d.SpotName = spotFor(d.Underlying)
	if d.SpotName != "" {
		spot, err := store.GetCandles(r.Context(), d.SpotName, d.Interval, from, to)
		if err != nil {
			return err
		}
		d.Spot = spot
	}

	d.Rows = buildOptionRows(d)
	if len(d.Rows) == 0 {
		d.Warning = d.explainNoData()
	} else if len(d.Spot) == 0 {
		d.Warning = fmt.Sprintf(
			"No %s spot bars stored for this day, so implied vol and the greeks cannot be derived. "+
				"Prices and open interest below are unaffected.", d.SpotName)
	}
	return nil
}

// explainNoData says why the table is empty, distinguishing the three cases an
// operator needs to tell apart.
//
// "Nothing was captured" is true in all three and useless in all three: the
// question is always whether this is fixable. A day where capture never ran but
// the contracts are still alive can be backfilled today; one where they have
// expired cannot be, ever.
func (d optionsData) explainNoData() string {
	switch {
	case d.CapturedStrikes == 0:
		return fmt.Sprintf(
			"No %s options were captured at all on %s (%s interval) — not this strike, none of them. "+
				"The daily capture job did not run that day. If these contracts have not yet expired "+
				"you can still backfill them; once they expire the data is gone for good.",
			d.Underlying, d.Date, d.Interval)

	case len(d.Strikes) > 0:
		// The count is deliberately left out of this sentence: it is already in
		// the form hint above, and phrasing it here means getting "1 other
		// strikes" wrong for the single-strike case.
		return fmt.Sprintf(
			"Strike %s was not captured on %s — it fell outside that day's capture window. "+
				"Strikes marked ● in the dropdown do have data.",
			d.Strike, d.Date)

	default:
		return fmt.Sprintf("Nothing captured for %s on %s at the %s interval.",
			d.Underlying, d.Date, d.Interval)
	}
}

// indexUnderlyings are the chains anyone actually opens this page for, in the
// order they are usually wanted.
var indexUnderlyings = []string{"NIFTY", "BANKNIFTY", "FINNIFTY", "MIDCPNIFTY", "NIFTYNXT50", "SENSEX", "BANKEX"}

// indicesFirst floats the index chains to the top of the underlying list.
//
// An NFO snapshot holds ~220 underlyings, almost all single-stock options. Left
// alphabetical, NIFTY sits between NHPC and NMDC and the one entry that matters
// is the hardest to find. The rest stay listed, in order, below a separator.
func indicesFirst(all []string) []string {
	have := make(map[string]bool, len(all))
	for _, u := range all {
		have[u] = true
	}

	out := make([]string, 0, len(all))
	promoted := make(map[string]bool, len(indexUnderlyings))
	for _, idx := range indexUnderlyings {
		if have[idx] {
			out = append(out, idx)
			promoted[idx] = true
		}
	}
	for _, u := range all {
		if !promoted[u] {
			out = append(out, u)
		}
	}
	return out
}

// spotFor maps an option underlying to the index symbol that prices it.
func spotFor(underlying string) string {
	switch strings.ToUpper(underlying) {
	case "NIFTY":
		return "NIFTY 50"
	case "BANKNIFTY":
		return "NIFTY BANK"
	case "FINNIFTY":
		return "NIFTY FIN SERVICE"
	case "MIDCPNIFTY":
		return "NIFTY MID SELECT"
	case "SENSEX":
		return "SENSEX"
	case "BANKEX":
		return "BANKEX"
	}
	return ""
}

// describeCoverage summarises what storage holds for a symbol.
func describeCoverage(ctx context.Context, store storage.HistoryStore, symbol, interval string) string {
	ranges, err := store.Coverage(ctx, symbol, interval)
	if err != nil || len(ranges) == 0 {
		return "none"
	}
	lo, hi := ranges[0].From, ranges[0].To
	for _, r := range ranges[1:] {
		if r.From.Before(lo) {
			lo = r.From
		}
		if r.To.After(hi) {
			hi = r.To
		}
	}
	return fmt.Sprintf("%s → %s (%d window(s))",
		lo.In(history.IST).Format("2006-01-02"),
		hi.In(history.IST).Format("2006-01-02"), len(ranges))
}

// buildOptionRows joins the two legs and the spot on their bar timestamps and
// derives the greeks.
//
// Bars are joined on open time rather than zipped by index: an illiquid strike
// prints fewer bars than the index, and zipping would silently pair a 09:20 put
// premium with an 11:00 spot — producing greeks that look plausible and are
// entirely fictional.
func buildOptionRows(d *optionsData) []optionRow {
	spotAt := make(map[int64]float64, len(d.Spot))
	for _, c := range d.Spot {
		spotAt[c.OpenTime.Unix()] = c.Close
	}

	byTime := map[int64]*optionRow{}
	order := []int64{}
	touch := func(t time.Time) *optionRow {
		k := t.Unix()
		if row, ok := byTime[k]; ok {
			return row
		}
		row := &optionRow{Time: t, Spot: spotAt[k]}
		byTime[k] = row
		order = append(order, k)
		return row
	}

	// The same annualized rate the short-straddle descriptor defaults to. It is
	// a constant rather than a form field because at Indian option tenors the
	// rate moves delta in the fourth decimal — offering it as a knob would imply
	// a precision the reconstruction does not have.
	const rate = 0.06

	if d.Call != nil {
		for _, c := range d.Call.Candles {
			row := touch(c.OpenTime)
			row.CallPrice, row.CallOI = c.Close, c.OpenInterest
			row.CallIV, row.CallDelta, row.CallGamma, row.CallTheta, row.CallVega, row.CallOK =
				greeksFor(c.Close, row.Spot, d.Call.Strike, c.OpenTime, d.Call.Expiry, rate, options.Call)
		}
	}
	if d.Put != nil {
		for _, c := range d.Put.Candles {
			row := touch(c.OpenTime)
			row.PutPrice, row.PutOI = c.Close, c.OpenInterest
			row.PutIV, row.PutDelta, row.PutGamma, row.PutTheta, row.PutVega, row.PutOK =
				greeksFor(c.Close, row.Spot, d.Put.Strike, c.OpenTime, d.Put.Expiry, rate, options.Put)
		}
	}

	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]optionRow, 0, len(order))
	for _, k := range order {
		row := byTime[k]
		if row.CallOK && row.PutOK {
			// Short straddle: position delta is the negative of the sum.
			row.StraddleDelta = -(row.CallDelta + row.PutDelta)
			row.StraddleOK = true
		}
		out = append(out, *row)
	}
	return out
}

// greeksFor solves implied vol from the premium, then prices the greeks off it.
func greeksFor(premium, spot, strike float64, at, expiry time.Time, rate float64, typ options.OptionType) (iv, delta, gamma, theta, vega float64, ok bool) {
	if premium <= 0 || spot <= 0 {
		return 0, 0, 0, 0, 0, false
	}
	t := options.YearsToExpiry(at, expiry)
	iv, err := options.ImpliedVol(premium, spot, strike, t, rate, typ)
	if err != nil {
		return 0, 0, 0, 0, 0, false
	}
	g := options.BlackScholes(spot, strike, t, iv, rate, typ)
	return iv, g.Delta, g.Gamma, g.Theta, g.Vega, true
}

// Recent returns the last 60 joined rows, newest last.
func (d optionsData) Recent() []optionRow {
	const n = 60
	if len(d.Rows) <= n {
		return d.Rows
	}
	return d.Rows[len(d.Rows)-n:]
}

// HasGreeks reports whether any row could be priced, which decides whether the
// greek columns are worth rendering at all.
func (d optionsData) HasGreeks() bool {
	for _, r := range d.Rows {
		if r.CallOK || r.PutOK {
			return true
		}
	}
	return false
}

// handleOptionCandlesJSON feeds the chart on the options page.
//
// It shares the resolution path with the page rather than taking a raw symbol,
// so the chart cannot end up showing a different contract than the table above
// it — and so an expired contract charts at all.
func (s *Server) handleOptionCandlesJSON(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := optionsData{
		Date:       strings.TrimSpace(q.Get("date")),
		Underlying: strings.ToUpper(strings.TrimSpace(q.Get("underlying"))),
		Expiry:     strings.TrimSpace(q.Get("expiry")),
		Strike:     strings.TrimSpace(q.Get("strike")),
		Interval:   q.Get("interval"),
	}
	if d.Interval == "" {
		d.Interval = string(kite.Interval5Minute)
	}
	if d.Underlying == "" || d.Expiry == "" || d.Strike == "" {
		http.Error(w, "date, underlying, expiry and strike are required", http.StatusBadRequest)
		return
	}
	if err := s.fillOptions(r, &d); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	leg := d.Call
	if strings.EqualFold(q.Get("type"), "PE") {
		leg = d.Put
	}
	if leg == nil {
		http.Error(w, "no such leg at that strike", http.StatusNotFound)
		return
	}
	writeCandlesJSON(w, leg.Candles)
}
