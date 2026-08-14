// Package storage defines the persistence interface for the trading platform.
//
// The interface is small and synchronous: the trading engine calls it to record
// orders, fills, positions, and (optionally) market data. SQLite is the default
// implementation, but the interface lets us swap in another backend later.
package storage

import (
	"context"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/marketdata"
)

// KiteSession is a persisted Zerodha access token.
//
// Zerodha's access tokens are short-lived — they expire daily at roughly 06:00
// IST — and are obtained through an interactive browser login. Persisting the
// current one is what lets the process restart mid-session (a deploy, a crash,
// a systemd restart) without dragging the operator back through Zerodha's login
// page. It is session state, not a provisioned secret: api_key and api_secret
// still live in the operator-managed secrets file.
type KiteSession struct {
	APIKey      string
	AccessToken string
	UserID      string
	IssuedAt    time.Time
	ExpiresAt   time.Time
}

// Valid reports whether the session is usable at time t. A token whose api_key
// no longer matches the configured one is never valid — that means credentials
// were rotated and the stored token belongs to a different app.
func (s KiteSession) Valid(t time.Time, apiKey string) bool {
	return s.AccessToken != "" && s.APIKey == apiKey && t.Before(s.ExpiresAt)
}

// WebSession is a logged-in browser session for the single operator.
type WebSession struct {
	ID        string
	CSRFToken string
	CreatedAt time.Time
	ExpiresAt time.Time
	UserAgent string
	IP        string
}

// SettingsStore persists operator settings that are changed at runtime and must
// survive a restart.
//
// Values are opaque strings (JSON in practice) so a caller can evolve the shape
// of a setting without a schema migration.
type SettingsStore interface {
	// GetSetting returns a stored value, reporting whether one exists. A missing
	// setting is not an error: it means "use the configured default".
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
	// DeleteSetting removes an override, restoring the configured default.
	DeleteSetting(ctx context.Context, key string) error
}

// SessionStore persists authentication state across process restarts.
type SessionStore interface {
	// Kite session (single row — there is one broker account)
	SaveKiteSession(ctx context.Context, s KiteSession) error
	GetKiteSession(ctx context.Context) (KiteSession, bool, error)
	ClearKiteSession(ctx context.Context) error

	// Browser sessions
	SaveWebSession(ctx context.Context, s WebSession) error
	GetWebSession(ctx context.Context, id string) (WebSession, bool, error)
	DeleteWebSession(ctx context.Context, id string) error
	DeleteExpiredWebSessions(ctx context.Context, now time.Time) error
}

// Store is the persistence boundary for everything the platform does.
type Store interface {
	// Close releases any underlying resources (e.g. the DB handle).
	Close() error

	// Orders
	SaveOrder(ctx context.Context, o *broker.Order) error
	GetOpenOrders(ctx context.Context) ([]broker.Order, error)

	// Fills
	SaveFill(ctx context.Context, f *broker.Fill) error

	// Positions (upsert: a position is keyed by strategy + symbol + product)
	UpsertPosition(ctx context.Context, p *broker.Position) error
	GetOpenPositions(ctx context.Context) ([]broker.Position, error)

	// Market data (only used when recording is enabled)
	SaveTick(ctx context.Context, t *marketdata.Tick) error
	SaveCandle(ctx context.Context, c *marketdata.Candle) error

	// Aggregate realized + unrealized PnL for the current trading day,
	// used by the risk manager to enforce the daily loss limit.
	GetDayPnL(ctx context.Context) (float64, error)

	// Authentication state (Kite access token, browser sessions).
	SessionStore

	// Runtime-editable operator settings.
	SettingsStore
}
