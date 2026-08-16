package web

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"kite-algo/internal/analytics"
	"kite-algo/internal/broker"
	"kite-algo/internal/charges"
	"kite-algo/internal/history"
	"kite-algo/internal/storage"
)

// historyData drives the realised trade-history and performance page.
//
// This reads the fills the trading path has been writing all along. Until now
// nothing read them back: Store's only order query is GetOpenOrders, so a
// closed order left the UI permanently and there was no way to ask "how did
// last week go?" of anything except a backtest that was never persisted.
type historyData struct {
	From     string
	To       string
	Strategy string
	Period   string

	Strategies []string
	Periods    []string

	Trades   []analytics.Trade
	Summary  analytics.Metrics
	Buckets  []analytics.PeriodSummary
	Orders   []broker.Order
	OpenLegs int

	HaveData  bool
	SpanFirst string
	SpanLast  string
	Error     string
	Notice    string
}

// Recent caps the trade table; the summary above it still covers everything.
func (d historyData) Recent() []analytics.Trade {
	const n = 200
	if len(d.Trades) <= n {
		return d.Trades
	}
	return d.Trades[len(d.Trades)-n:]
}

// RecentOrders caps the order table the same way.
func (d historyData) RecentOrders() []broker.Order {
	const n = 100
	if len(d.Orders) <= n {
		return d.Orders
	}
	return d.Orders[:n]
}

// handleHistory renders realised trades and their period breakdown.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := historyData{
		From:     strings.TrimSpace(q.Get("from")),
		To:       strings.TrimSpace(q.Get("to")),
		Strategy: strings.TrimSpace(q.Get("strategy")),
		Period:   string(analytics.ParsePeriod(q.Get("period"))),
		Periods:  []string{"daily", "weekly", "monthly"},
	}
	if err := s.fillHistory(r, &d); err != nil {
		d.Error = err.Error()
	}
	s.renderPage(w, r, "history.html", "History", d)
}

// fillHistory loads fills for the window and derives everything from them.
func (s *Server) fillHistory(r *http.Request, d *historyData) error {
	store, ok := s.app.Store.(storage.TradeStore)
	if !ok {
		return fmt.Errorf("this storage backend cannot serve trade history")
	}

	first, last, haveAny, err := store.FillsSpan(r.Context())
	if err != nil {
		return err
	}
	d.HaveData = haveAny
	if haveAny {
		d.SpanFirst = first.In(history.IST).Format("2006-01-02")
		d.SpanLast = last.In(history.IST).Format("2006-01-02")
	}

	// Default the window to where the data actually is. A last-30-days default
	// would open empty for anyone whose trading predates it, which reads as
	// "there is no history" rather than "you are looking at the wrong month".
	if d.From == "" || d.To == "" {
		if !haveAny {
			d.Notice = "No fills have been recorded yet. Trades appear here once a strategy or a manual order fills."
			return nil
		}
		if d.From == "" {
			d.From = d.SpanFirst
		}
		if d.To == "" {
			d.To = d.SpanLast
		}
	}

	from, err := time.ParseInLocation("2006-01-02", d.From, history.IST)
	if err != nil {
		return fmt.Errorf("'from' must be a date like 2026-08-14")
	}
	to, err := time.ParseInLocation("2006-01-02", d.To, history.IST)
	if err != nil {
		return fmt.Errorf("'to' must be a date like 2026-08-14")
	}
	to = to.AddDate(0, 0, 1) // inclusive of the end date's own session
	if !to.After(from) {
		return fmt.Errorf("'to' must be on or after 'from'")
	}

	fills, err := store.Fills(r.Context(), from, to)
	if err != nil {
		return err
	}
	d.Strategies = strategyNames(fills)

	if d.Strategy != "" {
		kept := fills[:0:0]
		for _, f := range fills {
			if f.StrategyID == d.Strategy {
				kept = append(kept, f)
			}
		}
		fills = kept
	}
	if len(fills) == 0 {
		d.Notice = fmt.Sprintf("No fills between %s and %s.", d.From, d.To)
		return s.loadHistoryOrders(r, store, d, from, to)
	}

	// The same pairing the backtester uses, over real fills instead of
	// simulated ones — so a live week and a backtest are measured identically.
	// Costs come from the shared rate card for the same reason.
	model := charges.DefaultNSEOptions()
	trades := analytics.BuildTrades(fills, model.CostOf)
	d.Trades = trades

	// A position still open at the end of the window produces no round trip.
	// Saying so matters: otherwise an open short straddle simply does not
	// appear, and the page looks like the strategy did nothing.
	d.OpenLegs = openLegs(fills)

	capital := s.app.Margins().Available
	if capital <= 0 {
		capital = 100000
	}
	d.Summary = analytics.Compute(trades, capital, 0.06)
	d.Buckets = analytics.ByPeriod(trades, analytics.ParsePeriod(d.Period), capital, 0.06)

	return s.loadHistoryOrders(r, store, d, from, to)
}

// loadHistoryOrders attaches the raw order log for the same window.
func (s *Server) loadHistoryOrders(r *http.Request, store storage.TradeStore, d *historyData, from, to time.Time) error {
	orders, err := store.Orders(r.Context(), from, to)
	if err != nil {
		return err
	}
	if d.Strategy != "" {
		kept := orders[:0:0]
		for _, o := range orders {
			if o.StrategyID == d.Strategy {
				kept = append(kept, o)
			}
		}
		orders = kept
	}
	d.Orders = orders
	return nil
}

// strategyNames lists the distinct strategies present in a fill set.
func strategyNames(fills []broker.Fill) []string {
	seen := map[string]struct{}{}
	for _, f := range fills {
		if f.StrategyID != "" {
			seen[f.StrategyID] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// openLegs counts instruments left with a non-zero net position at the end of
// the window.
//
// Computed from the signed fill quantities rather than by counting unpaired
// fills: BuildTrades matches FIFO and splits partial fills, so a fill can be
// half-closed and there is no "one entry fill per trade" arithmetic that holds.
// Net quantity is the only definition of still-open that survives scaling in
// and out of a position.
//
// Worth surfacing because an open leg contributes no round trip at all — an
// unclosed short straddle would otherwise make the page read as "the strategy
// did nothing" rather than "the strategy is still in the trade".
func openLegs(fills []broker.Fill) int {
	type key struct{ strategy, symbol string }
	net := map[key]int{}
	for _, f := range fills {
		k := key{f.StrategyID, f.TradingSymbol}
		if f.Side == broker.SideBuy {
			net[k] += f.Quantity
		} else {
			net[k] -= f.Quantity
		}
	}
	n := 0
	for _, q := range net {
		if q != 0 {
			n++
		}
	}
	return n
}
