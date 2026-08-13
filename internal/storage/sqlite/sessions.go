package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"kite-algo/internal/storage"
)

// --- Kite session (single row) ---

// SaveKiteSession stores the current Zerodha access token, replacing any
// previous one. There is exactly one broker account, so the row id is pinned.
func (s *Store) SaveKiteSession(ctx context.Context, k storage.KiteSession) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO kite_sessions (id, api_key, access_token, user_id, issued_at, expires_at)
		VALUES (1,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			api_key      = excluded.api_key,
			access_token = excluded.access_token,
			user_id      = excluded.user_id,
			issued_at    = excluded.issued_at,
			expires_at   = excluded.expires_at`,
		k.APIKey, k.AccessToken, k.UserID,
		k.IssuedAt.Format(time.RFC3339), k.ExpiresAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("save kite session: %w", err)
	}
	return nil
}

// GetKiteSession returns the stored Kite session, if any.
func (s *Store) GetKiteSession(ctx context.Context) (storage.KiteSession, bool, error) {
	var (
		k                 storage.KiteSession
		issuedAt, expires string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT api_key, access_token, user_id, issued_at, expires_at
		FROM kite_sessions WHERE id = 1`).
		Scan(&k.APIKey, &k.AccessToken, &k.UserID, &issuedAt, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.KiteSession{}, false, nil
	}
	if err != nil {
		return storage.KiteSession{}, false, fmt.Errorf("get kite session: %w", err)
	}
	k.IssuedAt, _ = time.Parse(time.RFC3339, issuedAt)
	k.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
	return k, true, nil
}

// ClearKiteSession removes the stored token — used on explicit disconnect and
// when Zerodha rejects it as expired.
func (s *Store) ClearKiteSession(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kite_sessions WHERE id = 1`)
	if err != nil {
		return fmt.Errorf("clear kite session: %w", err)
	}
	return nil
}

// --- Web sessions ---

// SaveWebSession persists a logged-in browser session.
func (s *Store) SaveWebSession(ctx context.Context, w storage.WebSession) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO web_sessions (id, csrf_token, created_at, expires_at, user_agent, ip)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			csrf_token = excluded.csrf_token,
			expires_at = excluded.expires_at,
			user_agent = excluded.user_agent,
			ip         = excluded.ip`,
		w.ID, w.CSRFToken, w.CreatedAt.Format(time.RFC3339),
		w.ExpiresAt.Format(time.RFC3339), w.UserAgent, w.IP)
	if err != nil {
		return fmt.Errorf("save web session: %w", err)
	}
	return nil
}

// GetWebSession looks up a browser session by its opaque id.
func (s *Store) GetWebSession(ctx context.Context, id string) (storage.WebSession, bool, error) {
	var (
		w                storage.WebSession
		created, expires string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, csrf_token, created_at, expires_at, user_agent, ip
		FROM web_sessions WHERE id = ?`, id).
		Scan(&w.ID, &w.CSRFToken, &created, &expires, &w.UserAgent, &w.IP)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.WebSession{}, false, nil
	}
	if err != nil {
		return storage.WebSession{}, false, fmt.Errorf("get web session: %w", err)
	}
	w.CreatedAt, _ = time.Parse(time.RFC3339, created)
	w.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
	return w, true, nil
}

// DeleteWebSession removes a single session (logout).
func (s *Store) DeleteWebSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete web session: %w", err)
	}
	return nil
}

// DeleteExpiredWebSessions sweeps sessions that have aged out.
func (s *Store) DeleteExpiredWebSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM web_sessions WHERE expires_at <= ?`, now.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("sweep web sessions: %w", err)
	}
	return nil
}
