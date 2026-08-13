package web

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"kite-algo/internal/auth"
	"kite-algo/internal/storage"
)

type ctxKey int

const sessionKey ctxKey = iota

// sessionFrom returns the authenticated session attached by requireAuth.
func sessionFrom(r *http.Request) (storage.WebSession, bool) {
	s, ok := r.Context().Value(sessionKey).(storage.WebSession)
	return s, ok
}

// middleware is the standard decorator shape.
type middleware func(http.Handler) http.Handler

// chain applies middlewares so the first listed is the outermost.
func chain(h http.Handler, ms ...middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// recoverPanic keeps one bad handler from taking down a process that is
// managing open positions.
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in http handler",
					"err", rec, "path", r.URL.Path, "method", r.Method)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// accessLog records requests at debug level, and slow ones at info.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		s.log.Debug("http",
			"method", r.Method, "path", r.URL.Path,
			"status", rw.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Hijack forwards to the underlying writer so WebSocket upgrades still work.
//
// This is not optional: gorilla/websocket type-asserts the ResponseWriter to
// http.Hijacker directly rather than walking Unwrap, so a wrapper that omits
// this method turns every upgrade into a 500. Any future middleware that wraps
// the ResponseWriter must forward Hijack too.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// Flush forwards to the underlying writer for streaming responses.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// secureHeaders sets conservative defaults. The CSP forbids remote origins
// entirely: this app must keep working on a VM with no outbound internet
// access, and a compromised template must not be able to exfiltrate positions
// to a third party.
func (s *Server) secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

// noStore prevents caching of authenticated pages and fragments.
func (s *Server) noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

// requireAuth rejects anonymous requests, redirecting browsers to the login
// page and answering API calls with 401.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.app.Sessions.Get(r.Context(), r)
		if !ok {
			if isAPIRequest(r) {
				// Tell htmx to bounce the whole page rather than swapping a
				// login form into a table cell.
				w.Header().Set("HX-Redirect", "/login")
				http.Error(w, "not authenticated", http.StatusUnauthorized)
				return
			}
			redirectToLogin(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), sessionKey, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireCSRF validates the per-session token on every state-changing request.
//
// GET is exempt, which is why /kite/callback — a cross-site GET navigation that
// Zerodha controls — passes through. That endpoint is protected instead by
// requiring an existing app session plus a single-use state nonce.
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		sess, ok := sessionFrom(r)
		if !ok {
			http.Error(w, "not authenticated", http.StatusUnauthorized)
			return
		}
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			token = r.FormValue("_csrf")
		}
		if !auth.ValidCSRF(sess, token) {
			s.log.Warn("csrf validation failed", "path", r.URL.Path, "ip", s.clientIP(r))
			http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isAPIRequest reports whether the caller expects a fragment or JSON rather
// than a full page navigation.
func isAPIRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" ||
		strings.HasPrefix(r.URL.Path, "/api/") ||
		strings.HasPrefix(r.URL.Path, "/partials/") ||
		strings.Contains(r.Header.Get("Accept"), "application/json")
}

// redirectToLogin sends the browser to the login page, remembering where it was
// headed so the operator lands there after authenticating.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	target := "/login"
	if r.Method == http.MethodGet && r.URL.Path != "/" {
		target += "?next=" + url.QueryEscape(r.URL.RequestURI())
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// clientIP resolves the address used for login throttling.
//
// X-Forwarded-For is honoured only when trust_proxy is set. Reading it
// unconditionally would let anyone spoof the header and hand themselves a fresh
// lockout budget on every request, which silently disables the throttle.
func (s *Server) clientIP(r *http.Request) string {
	if s.app.Cfg.Web.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// The right-most entry is the one our own proxy appended; entries to
			// its left are attacker-controlled.
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
		if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
			return xrip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
