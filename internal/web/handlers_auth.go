package web

import (
	"net/http"
	"strings"
	"time"

	"kite-algo/internal/auth"
)

// loginDelay is applied to every login attempt, successful or not, so response
// timing cannot distinguish "wrong password" from "no password configured".
const loginDelay = 300 * time.Millisecond

type loginView struct {
	Title string
	Error string
	Next  string
	Mode  string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	// Already signed in — no reason to show the form again.
	if _, ok := s.app.Sessions.Get(r.Context(), r); ok {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.renderLogin(w, http.StatusOK, "", r.URL.Query().Get("next"))
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, http.StatusBadRequest, "Malformed form submission.", "")
		return
	}
	next := r.FormValue("next")
	ip := s.clientIP(r)

	if ok, retry := s.app.Guard.Allow(ip); !ok {
		s.log.Warn("login attempt while locked out", "ip", ip, "retry_in", retry)
		s.renderLogin(w, http.StatusTooManyRequests,
			"Too many failed attempts. Try again in "+retry.Round(time.Second).String()+".", next)
		return
	}

	password := r.FormValue("password")
	time.Sleep(loginDelay)

	hash := s.app.Cfg.Web.PasswordHash
	if hash == "" {
		// Burn equivalent work so an unconfigured install is not detectable by
		// how fast it says no.
		auth.DummyVerify(password)
		s.renderLogin(w, http.StatusForbidden,
			"No password is configured. Run 'tradebot -set-password' on the server.", next)
		return
	}

	if !auth.VerifyPassword(hash, password) {
		if lock := s.app.Guard.Fail(ip); lock > 0 {
			s.log.Warn("login locked out after repeated failures", "ip", ip, "lockout", lock)
		} else {
			s.log.Warn("failed login", "ip", ip)
		}
		s.renderLogin(w, http.StatusUnauthorized, "Incorrect password.", next)
		return
	}

	s.app.Guard.Succeed(ip)
	if _, err := s.app.Sessions.Create(r.Context(), w, r); err != nil {
		s.log.Error("create session failed", "err", err)
		s.renderLogin(w, http.StatusInternalServerError, "Could not start a session.", next)
		return
	}
	s.log.Info("operator logged in", "ip", ip)
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.app.Sessions.Clear(r.Context(), w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) renderLogin(w http.ResponseWriter, status int, errMsg, next string) {
	v := loginView{
		Title: "Sign in",
		Error: errMsg,
		Next:  safeNext(next),
		Mode:  string(s.app.Cfg.Mode),
	}
	if err := s.render.Render(w, status, "login.html", v); err != nil {
		s.log.Error("render login failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// safeNext confines post-login redirects to this site. Without it, ?next= is an
// open redirect: a crafted link would bounce the operator to an attacker's page
// immediately after they authenticate, which is a convincing place to ask for
// the password again.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}
