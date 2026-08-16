package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"kite-algo/internal/app"
)

// handleConnect shows the "connect to Zerodha" page.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "connect.html", "Connect to Zerodha", nil)
}

// handleKiteLogin sends the operator to Zerodha's login page.
//
// A single-use state nonce is carried across the round-trip via redirect_params
// so a replayed or forged callback URL cannot drive a session exchange.
func (s *Server) handleKiteLogin(w http.ResponseWriter, r *http.Request) {
	loginURL, state, err := s.app.Kite.LoginURL()
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Cannot start Zerodha login", err.Error())
		return
	}
	target := loginURL + "&redirect_params=" + url.QueryEscape("state="+state)
	s.log.Info("redirecting operator to zerodha login")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleKiteCallback receives Zerodha's redirect and completes the session.
//
// This is the endpoint that closes the long-standing gap in this codebase:
// kite.Client.GenerateSession was implemented but had no caller, so an access
// token had to be pasted into the secrets file by hand every trading day.
//
// Note this is a GET with no CSRF token, because it is a cross-site top-level
// navigation that Zerodha initiates. It is protected instead by requiring an
// existing operator session (requireAuth runs first) plus the state nonce — and
// by the session cookie being SameSite=Lax rather than Strict, without which
// the browser would not send it on this navigation at all.
func (s *Server) handleKiteCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if status := q.Get("status"); status != "" && status != "success" {
		msg := q.Get("message")
		if msg == "" {
			msg = "Zerodha reported status=" + status
		}
		s.renderError(w, r, http.StatusBadRequest, "Zerodha login was not completed", msg)
		return
	}

	requestToken := q.Get("request_token")
	if requestToken == "" {
		s.renderError(w, r, http.StatusBadRequest, "Zerodha login was not completed",
			"The redirect carried no request_token. Start the login again.")
		return
	}

	if err := s.app.Kite.Complete(r.Context(), requestToken, q.Get("state")); err != nil {
		// A bad nonce is a stale or forged callback, not an upstream failure —
		// distinguish them so the status code matches the actual fault.
		status := http.StatusBadGateway
		if errors.Is(err, app.ErrBadLoginState) {
			status = http.StatusBadRequest
		}
		s.renderError(w, r, status, "Could not complete the Zerodha session", err.Error())
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleKiteDisconnect drops the Zerodha session and stops streaming.
func (s *Server) handleKiteDisconnect(w http.ResponseWriter, r *http.Request) {
	s.app.Kite.Invalidate(r.Context(), "disconnected by operator")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- shared page rendering ---

// pageView is the envelope every full page receives.
type pageView struct {
	Title  string
	Status app.Status
	CSRF   string
	Nav    string
	// Session warns when there is no Zerodha session on a trading day. Carried
	// on every page rather than one: the dashboard redirects to /connect when
	// disconnected, so a warning there could never appear, and a token that
	// lapses mid-session leaves the operator on some other page entirely.
	Session sessionAlert
	Data    any
	// Build versions the static asset URLs so a changed stylesheet or script is
	// never served from a stale cache.
	Build string

	// Error page fields.
	ErrTitle  string
	ErrDetail string
}

// renderPage renders a full document with the standard header context.
func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, tmpl, title string, data any) {
	sess, _ := sessionFrom(r)
	v := pageView{
		Title:   title,
		Status:  s.app.Status(),
		CSRF:    sess.CSRFToken,
		Nav:     strings.TrimPrefix(r.URL.Path, "/"),
		Data:    data,
		Build:   buildID,
		Session: s.sessionAlertFor(r),
	}
	if err := s.render.Render(w, http.StatusOK, tmpl, v); err != nil {
		s.log.Error("render failed", "template", tmpl, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// renderError renders a human-readable error page.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	s.log.Warn("ui error", "title", title, "detail", detail, "path", r.URL.Path)
	sess, _ := sessionFrom(r)
	v := pageView{
		Title:     title,
		Status:    s.app.Status(),
		CSRF:      sess.CSRFToken,
		Build:     buildID,
		ErrTitle:  title,
		ErrDetail: detail,
	}
	if err := s.render.Render(w, status, "error.html", v); err != nil {
		s.log.Error("render error page failed", "err", err)
		http.Error(w, title+": "+detail, status)
	}
}
