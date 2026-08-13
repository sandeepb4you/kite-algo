package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"kite-algo/internal/storage"
)

// CookieName is the browser cookie holding the opaque session id.
const CookieName = "ka_session"

// tokenBytes is the entropy in a session id and a CSRF token. 256 bits — these
// are bearer tokens for something that can place real orders.
const tokenBytes = 32

// Sessions manages logged-in browser sessions.
//
// Session ids are opaque random tokens with server-side state, rather than
// signed self-describing cookies. That choice buys instant revocation and a
// natural home for the per-session CSRF token, at the cost of a lookup — which
// is free here, since sessions are cached in memory and there is one user.
//
// Sessions are also written to storage so that a service restart (a deploy, a
// crash, a systemd Restart=always) does not log the operator out.
type Sessions struct {
	store  storage.SessionStore
	ttl    time.Duration
	secure bool
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]storage.WebSession
}

// NewSessions returns a session manager. secure marks cookies Secure, which
// must be true whenever the app is reached over anything but localhost.
func NewSessions(store storage.SessionStore, ttl time.Duration, secure bool, logger *slog.Logger) *Sessions {
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour
	}
	return &Sessions{
		store:  store,
		ttl:    ttl,
		secure: secure,
		logger: logger,
		cache:  make(map[string]storage.WebSession),
	}
}

// Create starts a new session and sets the cookie on w.
func (s *Sessions) Create(ctx context.Context, w http.ResponseWriter, r *http.Request) (storage.WebSession, error) {
	id, err := randomToken()
	if err != nil {
		return storage.WebSession{}, err
	}
	csrf, err := randomToken()
	if err != nil {
		return storage.WebSession{}, err
	}

	now := time.Now()
	sess := storage.WebSession{
		ID:        id,
		CSRFToken: csrf,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
		UserAgent: r.UserAgent(),
		IP:        r.RemoteAddr,
	}
	if err := s.store.SaveWebSession(ctx, sess); err != nil {
		return storage.WebSession{}, err
	}

	s.mu.Lock()
	s.cache[id] = sess
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:  CookieName,
		Value: id,
		Path:  "/",
		// HttpOnly keeps the token away from any script on the page.
		HttpOnly: true,
		Secure:   s.secure,
		// Lax, NOT Strict. Zerodha returns the operator to /kite/callback via a
		// cross-site top-level GET navigation, and a Strict cookie is not sent
		// on that request — the callback would arrive unauthenticated and the
		// request_token would be lost. Lax covers top-level GETs while still
		// withholding the cookie from cross-site POSTs.
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
		MaxAge:   int(s.ttl / time.Second),
	})
	return sess, nil
}

// Get returns the session referenced by the request's cookie, if it is valid
// and unexpired.
func (s *Sessions) Get(ctx context.Context, r *http.Request) (storage.WebSession, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return storage.WebSession{}, false
	}

	s.mu.RLock()
	sess, ok := s.cache[c.Value]
	s.mu.RUnlock()

	if !ok {
		// Cold cache — the process restarted since this cookie was issued.
		var err error
		sess, ok, err = s.store.GetWebSession(ctx, c.Value)
		if err != nil || !ok {
			return storage.WebSession{}, false
		}
		s.mu.Lock()
		s.cache[sess.ID] = sess
		s.mu.Unlock()
	}

	if time.Now().After(sess.ExpiresAt) {
		s.Destroy(ctx, sess.ID)
		return storage.WebSession{}, false
	}
	return sess, true
}

// Destroy removes a session everywhere and is safe to call for unknown ids.
func (s *Sessions) Destroy(ctx context.Context, id string) {
	s.mu.Lock()
	delete(s.cache, id)
	s.mu.Unlock()
	if err := s.store.DeleteWebSession(ctx, id); err != nil && s.logger != nil {
		s.logger.Warn("delete web session failed", "err", err)
	}
}

// Clear removes the session named by the request cookie and expires the cookie
// in the browser.
func (s *Sessions) Clear(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		s.Destroy(ctx, c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// ValidCSRF reports whether tok matches the session's CSRF token.
func ValidCSRF(sess storage.WebSession, tok string) bool {
	if sess.CSRFToken == "" || tok == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sess.CSRFToken), []byte(tok)) == 1
}

// GC periodically sweeps expired sessions from storage and the cache. It blocks
// until ctx is cancelled; run it in a goroutine.
func (s *Sessions) GC(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Hour
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			if err := s.store.DeleteExpiredWebSessions(ctx, now); err != nil && s.logger != nil {
				s.logger.Warn("web session sweep failed", "err", err)
			}
			s.mu.Lock()
			for id, sess := range s.cache {
				if now.After(sess.ExpiresAt) {
					delete(s.cache, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// randomToken returns a URL-safe cryptographically random bearer token.
func randomToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("auth: entropy unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
