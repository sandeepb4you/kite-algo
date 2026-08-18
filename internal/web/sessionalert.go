package web

import (
	"net/http"
)

// The missing-session banner.
//
// The DECISION — whether to warn and how loudly — lives in app.SessionAlert,
// because the Telegram alert needs the same judgement and a second copy would
// drift from this one. What is left here is presentation: where the banner is
// rendered, and the polled fragment that makes it appear and clear without a
// reload.
//
// It renders in the sticky chrome rather than on the dashboard, for two reasons.
// The dashboard redirects to /connect when there is no session, so a warning
// placed there could never appear. And a session that dies mid-session leaves
// the operator on /history or /options, neither of which redirects.

// handleSessionAlertFragment re-renders the banner on a poll, so it appears
// when a token lapses mid-session and clears the moment you reconnect —
// without the operator having to reload anything.
func (s *Server) handleSessionAlertFragment(w http.ResponseWriter, r *http.Request) {
	if err := s.render.Render(w, http.StatusOK, "session_alert.html", pageView{
		Session: s.app.SessionAlert(r.Context()),
	}); err != nil {
		s.log.Debug("render session alert failed", "err", err)
	}
}
