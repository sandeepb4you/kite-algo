package kite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAuthorizationHeaderFormat pins Kite's exact header format.
//
// It must be "token api_key:access_token" — colon separated. The client
// originally used a space, and Kite rejected EVERY authenticated call with
// "authorization value should atleast be `api_key`:`access_token`"
// (InputException, HTTP 400).
//
// The failure is deceptive: logging in still works, because GenerateSession
// authenticates with a checksum rather than this header. So the break only
// surfaces on the first call after a successful login, which reads like a bad
// or expired token rather than a malformed header.
func TestAuthorizationHeaderFormat(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"user_id":"AB1234"}}`))
	}))
	defer srv.Close()

	c := New("myapikey", "secret", "myaccesstoken", srv.URL, nil)
	if _, err := c.GetProfile(context.Background()); err != nil {
		t.Fatalf("GetProfile: %v", err)
	}

	const want = "token myapikey:myaccesstoken"
	if got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if strings.Contains(got, "myapikey myaccesstoken") {
		t.Error("api_key and access_token are space separated; Kite requires a colon")
	}
}

// TestAuthorizationHeaderOnEveryPath covers the three request builders, which
// each set the header independently and had all drifted the same way.
func TestAuthorizationHeaderOnEveryPath(t *testing.T) {
	const want = "token k:tok"

	seen := make(map[string]string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/instruments/historical"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"candles":[]}}`))
		case r.URL.Path == "/instruments/NFO":
			// The CSV path returns raw bytes, not the JSON envelope.
			_, _ = w.Write([]byte("instrument_token,tradingsymbol\n"))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{}}`))
		}
	}))
	defer srv.Close()

	c := New("k", "s", "tok", srv.URL, nil)
	ctx := context.Background()

	// do() — the standard JSON path.
	if _, err := c.GetProfile(ctx); err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	// getLimited() — the historical path, on its own rate-limit budget.
	if _, err := c.GetHistorical(ctx, HistoricalRequest{
		InstrumentToken: 1, Interval: IntervalDay,
		From: nowIST(), To: nowIST().AddDate(0, 0, 1),
	}); err != nil {
		t.Fatalf("GetHistorical: %v", err)
	}
	// rawGetBytes() — the instrument CSV path.
	if _, err := c.FetchInstrumentsExchange(ctx, "NFO"); err != nil {
		t.Fatalf("FetchInstrumentsExchange: %v", err)
	}

	if len(seen) < 3 {
		t.Fatalf("only %d paths exercised: %v", len(seen), seen)
	}
	for path, header := range seen {
		if header != want {
			t.Errorf("%s sent Authorization %q, want %q", path, header, want)
		}
	}
}

// TestNoAuthorizationHeaderWithoutAToken keeps unauthenticated calls clean
// rather than sending a header with an empty token.
func TestNoAuthorizationHeaderWithoutAToken(t *testing.T) {
	var got string
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, present = r.Header.Get("Authorization"), r.Header.Get("Authorization") != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{}}`))
	}))
	defer srv.Close()

	c := New("k", "s", "", srv.URL, nil) // no access token
	_, _ = c.GetProfile(context.Background())

	if present {
		t.Errorf("Authorization = %q; no header should be sent without a token", got)
	}
}

func nowIST() time.Time { return time.Date(2024, 8, 1, 9, 15, 0, 0, IST) }
