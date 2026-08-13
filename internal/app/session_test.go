package app

import (
	"testing"
	"time"

	"kite-algo/internal/storage"
)

// storageSession is a small constructor keeping the validity table readable.
func storageSession(apiKey, token string, expires time.Time) storage.KiteSession {
	return storage.KiteSession{APIKey: apiKey, AccessToken: token, ExpiresAt: expires}
}

// TestTokenExpiry pins the daily Zerodha session boundary. Access tokens die at
// about 06:00 IST regardless of when they were minted, so expiry is a wall-clock
// date calculation rather than "issued_at + N hours". Getting this wrong means
// either the app declares a working session dead, or it keeps trying to trade on
// a token Zerodha has already revoked.
func TestTokenExpiry(t *testing.T) {
	tests := []struct {
		name        string
		issued      time.Time
		wantDay     int
		wantHourIST int
		wantMinIST  int
	}{
		{
			name:        "morning issue expires next day",
			issued:      time.Date(2026, 8, 13, 9, 30, 0, 0, IST),
			wantDay:     14,
			wantHourIST: 5,
			wantMinIST:  45,
		},
		{
			name:        "late night issue still expires the coming morning",
			issued:      time.Date(2026, 8, 13, 23, 55, 0, 0, IST),
			wantDay:     14,
			wantHourIST: 5,
			wantMinIST:  45,
		},
		{
			name:        "issued before the cutover expires the same morning",
			issued:      time.Date(2026, 8, 13, 3, 0, 0, 0, IST),
			wantDay:     13,
			wantHourIST: 5,
			wantMinIST:  45,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TokenExpiry(tc.issued).In(IST)
			if got.Day() != tc.wantDay || got.Hour() != tc.wantHourIST || got.Minute() != tc.wantMinIST {
				t.Errorf("TokenExpiry(%s) = %s, want day %d at %02d:%02d IST",
					tc.issued.Format(time.RFC3339), got.Format(time.RFC3339),
					tc.wantDay, tc.wantHourIST, tc.wantMinIST)
			}
			if !got.After(tc.issued) {
				t.Errorf("expiry %s is not after issue time %s", got, tc.issued)
			}
		})
	}
}

// TestTokenExpiryUsesISTNotLocalTime guards against the expiry drifting with the
// server's timezone. A VM in UTC must compute the same instant as one in IST.
func TestTokenExpiryUsesIST(t *testing.T) {
	istIssue := time.Date(2026, 8, 13, 9, 30, 0, 0, IST)
	utcIssue := istIssue.UTC()

	if !TokenExpiry(istIssue).Equal(TokenExpiry(utcIssue)) {
		t.Errorf("expiry depends on the caller's timezone: IST=%s UTC=%s",
			TokenExpiry(istIssue), TokenExpiry(utcIssue))
	}
}

// TestLoginStateIsSingleUse ensures a captured callback URL cannot be replayed
// to drive a second session exchange.
func TestLoginStateIsSingleUse(t *testing.T) {
	s := &KiteSession{pending: map[string]time.Time{}}
	const nonce = "abc123"
	s.pending[nonce] = time.Now().Add(time.Minute)

	if !s.consumeState(nonce) {
		t.Fatal("first use of a valid nonce should succeed")
	}
	if s.consumeState(nonce) {
		t.Error("nonce was accepted twice; a replayed callback would be honoured")
	}
}

func TestLoginStateRejectsUnknownAndExpired(t *testing.T) {
	s := &KiteSession{pending: map[string]time.Time{}}
	s.pending["expired"] = time.Now().Add(-time.Minute)

	if s.consumeState("") {
		t.Error("empty state accepted")
	}
	if s.consumeState("never-issued") {
		t.Error("forged state accepted")
	}
	if s.consumeState("expired") {
		t.Error("expired state accepted")
	}
}

func TestKiteSessionValidity(t *testing.T) {
	now := time.Date(2026, 8, 13, 10, 0, 0, 0, IST)
	base := storageSession("key-a", "tok", now.Add(time.Hour))

	if !base.Valid(now, "key-a") {
		t.Error("a fresh token for the matching api_key should be valid")
	}
	if base.Valid(now.Add(2*time.Hour), "key-a") {
		t.Error("an expired token must not be valid")
	}
	// Credentials rotated: the stored token belongs to a different Kite app.
	if base.Valid(now, "key-b") {
		t.Error("token must not be valid for a different api_key")
	}
	if storageSession("key-a", "", now.Add(time.Hour)).Valid(now, "key-a") {
		t.Error("an empty token must never be valid")
	}
}
