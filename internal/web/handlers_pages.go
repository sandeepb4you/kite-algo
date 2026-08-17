package web

import (
	"encoding/json"
	"net/http"
	"time"

	"kite-algo/internal/broker"
)

// dashboardData is the dashboard page's payload.
type dashboardData struct {
	Positions []broker.Position
	Streaming bool
	Routing   string
}

// handleDashboard is the operator's landing page.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Without a Zerodha session there is nothing to show, so send the operator
	// straight to the page that fixes that.
	if !s.app.Kite.Snapshot().Connected() {
		http.Redirect(w, r, "/connect", http.StatusSeeOther)
		return
	}
	s.renderPage(w, r, "dashboard.html", "Dashboard", dashboardData{
		Positions: s.app.Engine.Positions(),
		Streaming: s.app.Engine.HasMarketData(),
		Routing:   s.app.Engine.BrokerMode(),
	})
}

// handleStatusFragment powers the header's htmx poll. This is the fallback that
// keeps the UI honest when the WebSocket is unavailable.
func (s *Server) handleStatusFragment(w http.ResponseWriter, r *http.Request) {
	s.renderFragment(w, r, "status_fragment.html", nil)
}

// handleAccountFragment refreshes the balance and P&L shown in the header.
func (s *Server) handleAccountFragment(w http.ResponseWriter, r *http.Request) {
	s.renderFragment(w, r, "account_fragment.html", nil)
}

// handleHealth is an unauthenticated liveness and readiness probe.
//
// It reports the Zerodha session state deliberately: the single most likely
// cause of a silent trading outage is the daily token expiring unnoticed, so a
// monitor can alert on kite_state != "active" after the market opens.
// tickAge is seconds since the last tick, or -1 if none has ever arrived.
//
// A number rather than a boolean because the useful monitor threshold differs
// by context: seconds during market hours, all night outside them.
func tickAge(last time.Time) int {
	if last.IsZero() {
		return -1
	}
	return int(time.Since(last).Seconds())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.app.Status()
	published, dropped, subscribers := s.app.Bus.Stats()

	// A halt is reported here so a monitor can alert on it: an unattended server
	// that halted overnight looks perfectly healthy by every other measure while
	// silently trading nothing.
	body := map[string]any{
		"status":     "ok",
		"mode":       st.Mode,
		"kite_state": st.Kite.State,
		// streaming means ticks are ARRIVING. market_data_attached means only
		// that a ticker is wired up, which stays true when its connection has
		// died — the two disagreeing is exactly the silent outage worth alerting
		// on, so a monitor gets both rather than a single ambiguous flag.
		"streaming":            st.Streaming,
		"market_data_attached": st.Kite.Attached,
		"last_tick_age_s":      tickAge(st.Kite.LastTickAt),
		"live_active":     st.LiveActive,
		"order_routing":   s.app.Engine.BrokerMode(),
		"halted":          st.Halt.Halted,
		"halt_reason":     st.Halt.Reason,
		"strategies":      len(s.app.Engine.ListStrategies()),
		"uptime":          st.Uptime,
		"events_total":    published,
		"events_dropped":  dropped,
		"event_consumers": subscribers,
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Debug("write health response failed", "err", err)
	}
}
