package web

import (
	"net/http"
	"sort"
)

// defaultWatchlist is what the market page streams before any user-defined
// watchlist exists. These are the index symbols the platform already knows how
// to resolve without an instrument-master entry.
var defaultWatchlist = []string{"NIFTY 50", "NIFTY BANK", "NIFTY FIN SERVICE", "INDIA VIX"}

// quoteRow is one line of the watchlist.
type quoteRow struct {
	Symbol string
	Last   float64
}

type marketData struct {
	Watchlist []quoteRow
	Streaming bool
}

// handleMarket renders the live market dashboard.
func (s *Server) handleMarket(w http.ResponseWriter, r *http.Request) {
	if !s.app.Kite.Snapshot().Connected() {
		http.Redirect(w, r, "/connect", http.StatusSeeOther)
		return
	}
	s.renderPage(w, r, "market.html", "Market", marketData{
		Watchlist: s.watchlist(),
		Streaming: s.app.Engine.HasMarketData(),
	})
}

// handleWatchlistFragment is the polled fallback for the watchlist table.
func (s *Server) handleWatchlistFragment(w http.ResponseWriter, r *http.Request) {
	v := pageView{Status: s.app.Status(), Data: marketData{
		Watchlist: s.watchlist(),
		Streaming: s.app.Engine.HasMarketData(),
	}}
	s.renderFragment(w, "watchlist_fragment.html", v)
}

// handlePositionsFragment is the polled fallback for the positions table.
func (s *Server) handlePositionsFragment(w http.ResponseWriter, r *http.Request) {
	v := pageView{Status: s.app.Status(), Data: dashboardData{
		Positions: s.app.Engine.Positions(),
		Streaming: s.app.Engine.HasMarketData(),
		Routing:   s.app.Engine.BrokerMode(),
	}}
	s.renderFragment(w, "positions_fragment.html", v)
}

// watchlist builds the quote rows, seeding prices from the engine's cache so a
// freshly loaded page shows the last known values rather than a column of
// zeroes while it waits for the first tick.
func (s *Server) watchlist() []quoteRow {
	prices := s.app.Engine.Prices()

	symbols := make([]string, 0, len(defaultWatchlist))
	seen := make(map[string]struct{}, len(defaultWatchlist))
	for _, sym := range defaultWatchlist {
		symbols = append(symbols, sym)
		seen[sym] = struct{}{}
	}
	// Include anything currently held, so open positions are always visible
	// on the market page even if they are not on the watchlist.
	for _, p := range s.app.Engine.Positions() {
		if !p.IsOpen() {
			continue
		}
		if _, dup := seen[p.TradingSymbol]; !dup {
			symbols = append(symbols, p.TradingSymbol)
			seen[p.TradingSymbol] = struct{}{}
		}
	}
	sort.Strings(symbols[len(defaultWatchlist):])

	rows := make([]quoteRow, 0, len(symbols))
	for _, sym := range symbols {
		rows = append(rows, quoteRow{Symbol: sym, Last: prices[sym]})
	}
	return rows
}

// renderFragment writes a partial template with no surrounding document.
func (s *Server) renderFragment(w http.ResponseWriter, tmpl string, v pageView) {
	if err := s.render.Render(w, http.StatusOK, tmpl, v); err != nil {
		s.log.Error("render fragment failed", "template", tmpl, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
