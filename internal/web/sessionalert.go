package web

import (
	"net/http"
	"strings"
	"time"

	"kite-algo/internal/history"
	"kite-algo/internal/storage"
)

// The missing-session warning.
//
// Zerodha invalidates access tokens daily around 06:00 IST and they can only be
// renewed through an interactive browser login, so every trading day begins with
// no session. Forgetting is the single most expensive routine mistake available
// here, and it is a quiet one: the platform keeps serving pages, the strategies
// page still lists strategies, and nothing looks broken.
//
// What is actually lost is not recoverable. The instrument snapshot and the
// daily option capture both run off that session, and Kite drops expired
// contracts from its historical feed entirely — so a contract that expires
// before the next login takes its price history with it, permanently.
//
// This renders in the sticky chrome rather than on the dashboard, for two
// reasons. The dashboard redirects to /connect when there is no session, so a
// warning placed there could never appear. And a session that dies mid-session
// leaves the operator on /history or /options, neither of which redirects.

// sessionAlert is the banner state for a missing Zerodha session.
type sessionAlert struct {
	Show bool
	// Critical marks the point after which today's capture has already been
	// missed, which is a different problem from "log in soon".
	Critical bool
	Headline string
	Detail   string
}

// sessionAlertFor decides whether to warn, and how loudly.
//
// Silent on non-trading days: there is nothing to capture on a Sunday, and a
// banner that cries wolf all weekend is one nobody reads on Monday.
func (s *Server) sessionAlertFor(r *http.Request) sessionAlert {
	if s.app.Kite.Snapshot().Connected() {
		return sessionAlert{}
	}

	cal := history.NSE()
	cal.SetHolidays(s.app.Cfg.Capture.Holidays)
	now := time.Now().In(history.IST)
	if !cal.IsTradingDay(now) {
		return sessionAlert{}
	}

	// Before the exchange opens there is still plenty of time and no data at
	// risk yet; nagging from midnight would train the operator to ignore this.
	session, ok := cal.SessionFor(now)
	if !ok || now.Before(session.From) {
		return sessionAlert{}
	}

	haveSnapshot := s.haveSnapshotToday(r)
	captureMissed := s.captureWindowPassed(now)

	switch {
	case captureMissed:
		// The 15:40 job needed a session and did not get one. Anything expiring
		// today is now unrecoverable, which no later login can undo.
		return sessionAlert{
			Show: true, Critical: true,
			Headline: "No Zerodha session — today's option capture has been missed.",
			Detail: "The capture runs at " + s.app.Cfg.Capture.RunAt +
				" IST and needs a live session. Contracts expiring today have " +
				"taken their price history with them; Kite cannot supply it again. " +
				"Connect now to salvage the contracts that have not expired yet.",
		}

	case !haveSnapshot:
		return sessionAlert{
			Show: true, Critical: true,
			Headline: "No Zerodha session — today has no instrument snapshot.",
			Detail: "Nothing traded today can be resolved for a future backtest " +
				"until you connect, and option candles are only captured while a " +
				"session is live. Neither can be recovered after the contracts expire.",
		}

	default:
		// The snapshot is safe — they connected earlier and the token lapsed.
		// Still worth saying, because the capture has not run yet.
		return sessionAlert{
			Show: true,
			Headline: "Zerodha session has ended. Reconnect before " +
				s.app.Cfg.Capture.RunAt + " IST.",
			Detail: "Today's instrument snapshot is already saved, so that is safe. " +
				"The option capture still needs a live session, and no live data " +
				"or trading is possible until you reconnect.",
		}
	}
}

// haveSnapshotToday reports whether today's instrument master is already stored.
func (s *Server) haveSnapshotToday(r *http.Request) bool {
	store, ok := s.app.Store.(storage.HistoryStore)
	if !ok {
		return false
	}
	have, err := store.HasInstrumentSnapshot(r.Context(), time.Now().In(history.IST))
	return err == nil && have
}

// captureWindowPassed reports whether the daily capture time has gone by.
//
// Only meaningful when capture is enabled: with it off, nothing was scheduled
// to be missed and claiming otherwise would be false alarm.
func (s *Server) captureWindowPassed(now time.Time) bool {
	if !s.app.Cfg.Capture.Enabled {
		return false
	}
	t, err := time.Parse("15:04", strings.TrimSpace(s.app.Cfg.Capture.RunAt))
	if err != nil {
		return false
	}
	local := now.In(history.IST)
	runAt := time.Date(local.Year(), local.Month(), local.Day(),
		t.Hour(), t.Minute(), 0, 0, history.IST)
	return local.After(runAt)
}

// handleSessionAlertFragment re-renders the banner on a poll, so it appears
// when a token lapses mid-session and clears the moment you reconnect —
// without the operator having to reload anything.
func (s *Server) handleSessionAlertFragment(w http.ResponseWriter, r *http.Request) {
	if err := s.render.Render(w, http.StatusOK, "session_alert.html", pageView{
		Session: s.sessionAlertFor(r),
	}); err != nil {
		s.log.Debug("render session alert failed", "err", err)
	}
}
