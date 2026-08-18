package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	// Embeds the IANA timezone database. Kite's session lifetime is defined in
	// IST, and a bare cloud VM frequently has no tzdata installed, which would
	// make time.LoadLocation("Asia/Kolkata") fail at runtime — on the one code
	// path that decides whether the trading day's token is still valid.
	_ "time/tzdata"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/engine"
	"kite-algo/internal/events"
	"kite-algo/internal/history"
	"kite-algo/internal/kite"
	"kite-algo/internal/storage"
)

// SessionState is where the Zerodha connection currently stands.
type SessionState string

const (
	// StateNoCredentials means api_key/api_secret are missing entirely.
	StateNoCredentials SessionState = "no_credentials"
	// StateNeedsLogin means credentials exist but there is no usable token.
	StateNeedsLogin SessionState = "needs_login"
	// StateConnecting means a token is being exchanged or instruments loaded.
	StateConnecting SessionState = "connecting"
	// StateActive means the REST client is authenticated and ticks are flowing.
	StateActive SessionState = "active"
	// StateExpired means Zerodha rejected the token or the trading day rolled.
	StateExpired SessionState = "expired"
	// StateError means connecting failed for a reason other than expiry.
	StateError SessionState = "error"
)

// IST is the exchange timezone. All Kite session lifetimes are defined in it.
var IST = time.FixedZone("IST", 5*3600+30*60)

// tokenExpiryHour is when Zerodha invalidates access tokens (about 06:00 IST).
// We treat the token as expired slightly before that so a strategy is never
// mid-order when the session dies.
const tokenExpiryHour = 6

// tokenSafetyMargin is subtracted from the computed expiry.
const tokenSafetyMargin = 15 * time.Minute

// loginStateTTL bounds how long a pending browser login round-trip may take.
const loginStateTTL = 10 * time.Minute

// ErrBadLoginState means the callback carried a missing, forged, replayed, or
// expired state nonce. It is a client-side fault, distinct from Zerodha
// refusing the exchange, so callers can pick an accurate status code.
var ErrBadLoginState = errors.New("login state is invalid or expired; start the login again")

// SessionInfo is a snapshot of the Kite connection for display.
type SessionInfo struct {
	State     SessionState `json:"state"`
	UserID    string       `json:"user_id,omitempty"`
	UserName  string       `json:"user_name,omitempty"`
	IssuedAt  time.Time    `json:"issued_at,omitempty"`
	ExpiresAt time.Time    `json:"expires_at,omitempty"`
	LastError string       `json:"last_error,omitempty"`
	// Streaming reports that ticks are arriving now. Attached reports only that
	// a ticker object is wired up — true even when its connection is dead, which
	// is precisely the state that hid a market-data outage once.
	Streaming  bool      `json:"streaming"`
	Attached   bool      `json:"market_data_attached"`
	LastTickAt time.Time `json:"last_tick_at,omitempty"`
}

// lastTick reads the engine's most recent tick time, tolerating a nil engine.
func lastTick(e *engine.Engine) time.Time {
	if e == nil {
		return time.Time{}
	}
	return e.LastTickAt()
}

// Connected reports whether the session can currently reach Zerodha.
func (s SessionInfo) Connected() bool { return s.State == StateActive }

// KiteSession owns the Zerodha connection lifecycle: obtaining an access token
// through the browser, validating it, bringing up the instrument master and
// ticker, and tearing all of that down when the token expires.
//
// The whole point of this type is that the process no longer needs credentials
// at startup. It boots, serves a login page, and connects afterwards.
type KiteSession struct {
	cfg    *config.Config
	store  storage.Store
	eng    *engine.Engine
	bus    *events.Bus
	logger *slog.Logger

	mu          sync.RWMutex
	state       SessionState
	client      *kite.Client
	instruments *kite.Instruments
	ticker      *kite.Ticker
	userID      string
	userName    string
	issuedAt    time.Time
	expiresAt   time.Time
	lastErr     string

	// token is the access token the active session was built on, so a repeat
	// Activate with the same one can be recognised as a no-op.
	token string

	// activateMu serialises Activate. Distinct from mu, which guards the fields
	// above: Activate holds this across two slow network calls, and blocking
	// every status read for the duration would freeze the UI.
	activateMu sync.Mutex

	// pending holds unconsumed CSRF nonces for in-flight login round-trips.
	pending map[string]time.Time

	// onMarketData runs once market data is attached and the instrument master
	// is available. Strategy restore hangs off this rather than off boot: a
	// resumed strategy has to resolve its open legs against the master and
	// subscribe for the ticks that drive its exits, and neither exists until a
	// session is live.
	onMarketData func(context.Context)
}

// OnMarketData registers a callback to run each time market data comes up.
func (s *KiteSession) OnMarketData(fn func(context.Context)) {
	s.mu.Lock()
	s.onMarketData = fn
	s.mu.Unlock()
}

// NewKiteSession builds a session manager. The client is constructed eagerly
// with whatever credentials exist so LoginURL works before any token does.
func NewKiteSession(cfg *config.Config, store storage.Store, eng *engine.Engine, bus *events.Bus, logger *slog.Logger) *KiteSession {
	s := &KiteSession{
		cfg:     cfg,
		store:   store,
		eng:     eng,
		bus:     bus,
		logger:  logger,
		pending: make(map[string]time.Time),
		state:   StateNeedsLogin,
	}
	if cfg.Kite.APIKey == "" || cfg.Kite.APISecret == "" {
		s.state = StateNoCredentials
	}
	s.client = kite.New(cfg.Kite.APIKey, cfg.Kite.APISecret, "", cfg.Kite.BaseURL, logger)
	return s
}

// Snapshot returns the current session state for rendering.
func (s *KiteSession) Snapshot() SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return SessionInfo{
		State:     s.state,
		UserID:    s.userID,
		UserName:  s.userName,
		IssuedAt:  s.issuedAt,
		ExpiresAt: s.expiresAt,
		LastError: s.lastErr,
		// Whether ticks are ARRIVING, not whether a ticker is attached. Those
		// came apart on 2026-08-17: a cancelled ticker stayed attached, so this
		// reported streaming while nothing had arrived for fifteen minutes.
		// A health signal that cannot tell "connected" from "receiving" is the
		// one that lets a silent outage run through a trading session.
		Streaming:  s.eng != nil && s.eng.Streaming(),
		Attached:   s.eng != nil && s.eng.HasMarketData(),
		LastTickAt: lastTick(s.eng),
	}
}

// Client returns the Kite REST client. It is never nil, but only carries a
// usable token once the session is active.
func (s *KiteSession) Client() *kite.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

// Instruments returns the loaded instrument master, or nil before login.
func (s *KiteSession) Instruments() *kite.Instruments {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.instruments
}

// LoginURL returns the Zerodha login URL plus a single-use state nonce to be
// echoed back on the callback.
func (s *KiteSession) LoginURL() (loginURL, state string, err error) {
	s.mu.RLock()
	apiKey := s.cfg.Kite.APIKey
	client := s.client
	s.mu.RUnlock()

	if apiKey == "" {
		return "", "", errors.New("kite api_key is not configured")
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate login state: %w", err)
	}
	state = base64.RawURLEncoding.EncodeToString(b)

	s.mu.Lock()
	s.expirePendingLocked()
	s.pending[state] = time.Now().Add(loginStateTTL)
	s.mu.Unlock()

	return client.LoginURL(), state, nil
}

// consumeState validates and burns a login nonce. Nonces are single-use, so a
// replayed callback URL cannot re-drive the exchange.
func (s *KiteSession) consumeState(state string) bool {
	if state == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expirePendingLocked()

	for candidate, deadline := range s.pending {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(state)) == 1 {
			delete(s.pending, candidate)
			return time.Now().Before(deadline)
		}
	}
	return false
}

func (s *KiteSession) expirePendingLocked() {
	now := time.Now()
	for k, deadline := range s.pending {
		if now.After(deadline) {
			delete(s.pending, k)
		}
	}
}

// Complete exchanges a request_token from the Zerodha redirect for an access
// token, then brings the session up.
//
// This is the first and only caller of kite.Client.GenerateSession — before the
// web UI existed, nothing captured the request token and the operator had to
// paste an access token into the secrets file by hand every trading day.
func (s *KiteSession) Complete(ctx context.Context, requestToken, state string) error {
	if !s.consumeState(state) {
		return ErrBadLoginState
	}
	if requestToken == "" {
		return errors.New("no request_token in the callback")
	}

	s.setState(StateConnecting, "")

	token, err := s.Client().GenerateSession(ctx, requestToken)
	if err != nil {
		s.fail(fmt.Errorf("exchange request token: %w", err))
		return err
	}
	return s.Activate(ctx, token, true)
}

// Activate validates an access token and brings up instruments and streaming.
// persist controls whether the token is written to storage; a token restored
// from storage does not need rewriting.
func (s *KiteSession) Activate(ctx context.Context, token string, persist bool) error {
	if token == "" {
		return errors.New("empty access token")
	}

	// One activation at a time.
	//
	// Two can overlap in normal use: the boot-time token restore and an operator
	// login started before it finished. Both spend ~20s downloading the
	// instrument master, so they finish milliseconds apart, and each builds and
	// starts its own ticker. Two websockets connect, the loser is cancelled, and
	// the logs read like a connection failure rather than a race.
	//
	// This happened on 2026-08-17: two "kite ticker connected" lines 200ms
	// apart, a "context canceled" three seconds later, and then no market data
	// at all until the process was restarted.
	s.activateMu.Lock()
	defer s.activateMu.Unlock()

	// Already live on this exact token. Rebuilding would re-download the
	// instrument master and swap a working feed for an identical one, which is
	// pure risk for no gain — an operator pressing Connect on an already-
	// connected session should be a no-op, not a reconnection.
	s.mu.RLock()
	same := s.state == StateActive && s.token == token && s.ticker != nil
	s.mu.RUnlock()
	if same {
		if s.logger != nil {
			s.logger.Info("session already active on this token; not reconnecting")
		}
		return nil
	}

	s.setState(StateConnecting, "")

	client := s.Client()
	client.SetAccessToken(token)

	// Validate immediately so a bad or already-expired token fails here rather
	// than as a confusing error from the first strategy order.
	profile, err := client.GetProfile(ctx)
	if err != nil {
		s.fail(fmt.Errorf("validate access token: %w", err))
		return err
	}

	// NSE and BSE derivatives are separate downloads. Loading NFO alone — which
	// is all this did originally — makes every SENSEX and BANKEX contract look
	// like a contract that does not exist, and silently omits them from the
	// instrument snapshot, so BSE option data can never be backtested however
	// diligently it is captured. Which exchanges are needed follows from the
	// configured capture chains.
	exchanges := s.cfg.CaptureExchanges()
	instruments, err := client.FetchInstrumentsExchanges(ctx, exchanges...)
	if err != nil {
		s.fail(fmt.Errorf("load instruments: %w", err))
		return err
	}
	client.LogInstrumentsSummary(instruments)
	if s.logger != nil {
		s.logger.Info("instrument master loaded",
			"exchanges", strings.Join(instruments.Exchanges(), ","),
			"requested", strings.Join(exchanges, ","),
			"instruments", instruments.Len())
	}

	now := time.Now()
	expires := TokenExpiry(now)

	ticker := kite.NewTicker(s.cfg.Kite.APIKey, token, s.cfg.Kite.TickerURL, instruments, s.logger)

	s.mu.Lock()
	// Close whatever this replaces. The engine also cancels the ticker it is
	// swapping out, but only when there IS an engine — and relying on the
	// consumer to clean up the producer's resource is how the leak came back
	// the first time.
	old := s.ticker
	s.instruments = instruments
	s.ticker = ticker
	s.token = token
	s.userID = profile.UserID
	s.userName = profile.UserName
	s.issuedAt = now
	s.expiresAt = expires
	s.lastErr = ""
	s.state = StateActive
	s.mu.Unlock()

	if old != nil && old != ticker {
		old.Close()
	}

	if persist {
		rec := storage.KiteSession{
			APIKey:      s.cfg.Kite.APIKey,
			AccessToken: token,
			UserID:      profile.UserID,
			IssuedAt:    now,
			ExpiresAt:   expires,
		}
		if err := s.store.SaveKiteSession(ctx, rec); err != nil && s.logger != nil {
			// Not fatal: the session works, it just won't survive a restart.
			s.logger.Warn("could not persist kite session; a restart will require logging in again", "err", err)
		}
	}

	if s.eng != nil {
		s.eng.AttachMarketData(instruments, ticker)
	}

	// After AttachMarketData, so a restored strategy's Subscribe reaches a live
	// ticker; before the snapshot below, which is slow and must not delay
	// re-adopting an open position.
	s.mu.RLock()
	resume := s.onMarketData
	s.mu.RUnlock()
	if resume != nil {
		resume(ctx)
	}

	// Snapshot the instrument master before doing anything else with it.
	//
	// Kite lists only live contracts, and historical candles are keyed by
	// instrument token — so an expired option becomes permanently unresolvable.
	// Every session without a snapshot is a trading day that can never be
	// backtested, and no later action can recover it.
	if hs, ok := s.store.(storage.HistoryStore); ok {
		if err := history.SnapshotInstruments(ctx, hs, instruments, now, s.logger); err != nil && s.logger != nil {
			s.logger.Error("could not snapshot the instrument master; "+
				"today will not be backtestable", "err", err)
		}
	}

	if s.logger != nil {
		s.logger.Info("kite session active",
			"user", profile.UserID, "name", profile.UserName, "expires_at", expires.In(IST).Format(time.RFC3339))
	}
	s.publishStatus(events.LevelInfo, "connected to Zerodha as "+profile.UserID)
	return nil
}

// Restore brings up a session from a previously persisted token, so a restart
// inside the trading day does not send the operator back through Zerodha.
//
// Precedence is deliberate: a stored token wins over the configured one. A
// stale KITE_ACCESS_TOKEN left in an environment file would otherwise
// permanently shadow every fresh browser login. The configured token is only a
// bootstrap seed for scripted setups.
func (s *KiteSession) Restore(ctx context.Context) error {
	apiKey := s.cfg.Kite.APIKey
	if apiKey == "" || s.cfg.Kite.APISecret == "" {
		s.setState(StateNoCredentials, "")
		return nil
	}

	rec, found, err := s.store.GetKiteSession(ctx)
	if err != nil && s.logger != nil {
		s.logger.Warn("read stored kite session failed", "err", err)
	}
	if found && rec.Valid(time.Now(), apiKey) {
		if err := s.Activate(ctx, rec.AccessToken, false); err == nil {
			return nil
		}
		// Stored token was rejected; clear it so we don't retry it forever.
		if err := s.store.ClearKiteSession(ctx); err != nil && s.logger != nil {
			s.logger.Warn("clear stale kite session failed", "err", err)
		}
	}

	if s.cfg.Kite.AccessToken != "" {
		if err := s.Activate(ctx, s.cfg.Kite.AccessToken, true); err == nil {
			return nil
		}
	}

	s.setState(StateNeedsLogin, "")
	if s.logger != nil {
		s.logger.Info("no usable Zerodha session; log in through the web UI to connect")
	}
	return nil
}

// Invalidate tears the session down, e.g. when Zerodha rejects the token. The
// engine keeps running so positions and PnL stay visible.
func (s *KiteSession) Invalidate(ctx context.Context, reason string) {
	s.mu.Lock()
	s.state = StateExpired
	s.lastErr = reason
	s.ticker = nil
	s.mu.Unlock()

	if s.eng != nil {
		s.eng.DetachMarketData()
	}
	if err := s.store.ClearKiteSession(ctx); err != nil && s.logger != nil {
		s.logger.Warn("clear kite session failed", "err", err)
	}
	if s.logger != nil {
		s.logger.Warn("kite session invalidated; log in again", "reason", reason)
	}
	s.publishStatus(events.LevelWarn, "Zerodha session ended: "+reason)
}

// Supervise watches for token expiry until ctx is cancelled. Run it in a
// goroutine.
func (s *KiteSession) Supervise(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.RLock()
			state, expires := s.state, s.expiresAt
			s.mu.RUnlock()
			if state == StateActive && !expires.IsZero() && time.Now().After(expires) {
				s.Invalidate(ctx, "access token expired (Zerodha tokens end each day at ~06:00 IST)")
			}
		}
	}
}

// setState transitions the session and announces it.
func (s *KiteSession) setState(st SessionState, errMsg string) {
	s.mu.Lock()
	s.state = st
	s.lastErr = errMsg
	s.mu.Unlock()
}

// fail records a connection error.
func (s *KiteSession) fail(err error) {
	s.setState(StateError, err.Error())
	if s.logger != nil {
		s.logger.Error("kite session failed", "err", err)
	}
	s.publishStatus(events.LevelError, err.Error())
}

func (s *KiteSession) publishStatus(level events.Level, msg string) {
	if s.bus == nil {
		return
	}
	info := s.Snapshot()
	s.bus.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   level,
		Message: msg,
		Fields: map[string]any{
			"kite_state": string(info.State),
			"user_id":    info.UserID,
			"streaming":  info.Streaming,
		},
	})
}

// TokenExpiry returns when a token issued at `now` stops being usable.
//
// Zerodha invalidates access tokens daily at roughly 06:00 IST regardless of
// when they were issued, so a token minted at 09:00 lasts ~21 hours while one
// minted at 05:00 lasts one. A safety margin is subtracted so the session is
// declared dead slightly before Zerodha does.
func TokenExpiry(now time.Time) time.Time {
	ist := now.In(IST)
	next := time.Date(ist.Year(), ist.Month(), ist.Day(), tokenExpiryHour, 0, 0, 0, IST)
	if !next.After(ist) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Add(-tokenSafetyMargin)
}

// LiveBrokerFor builds a live broker from the session's authenticated client.
// Returns an error unless the session is active, so a live broker can never be
// constructed against an unvalidated token.
func (s *KiteSession) LiveBrokerFor() (broker.Broker, error) {
	s.mu.RLock()
	state, client := s.state, s.client
	s.mu.RUnlock()
	if state != StateActive {
		return nil, fmt.Errorf("cannot go live: Zerodha session is %s", state)
	}
	return broker.NewLiveBroker(client, s.logger, s.cfg.Kite.MarketProtection), nil
}
