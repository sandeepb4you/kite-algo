package kite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetLTPRepeatsInstrumentParam checks that multiple instruments are sent as
// a genuine repeated "i" parameter.
//
// The original implementation did q.Set("i", strings.Join(keys, "&i=")), which
// url.Values.Encode() then percent-escaped into a single malformed key
// (i=NFO%3AA%26i%3DNFO%3AB). Single-symbol calls looked fine, which is why the
// bug survived: it only corrupts multi-symbol quotes.
func TestGetLTPRepeatsInstrumentParam(t *testing.T) {
	keys := []string{"NFO:NIFTY24AUG24500CE", "NFO:NIFTY24AUG24500PE", "NSE:INFY"}

	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()["i"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{}}`))
	}))
	defer srv.Close()

	c := New("key", "secret", "token", srv.URL, nil)
	if _, err := c.GetLTP(context.Background(), keys); err != nil {
		t.Fatalf("GetLTP: %v", err)
	}

	if len(got) != len(keys) {
		t.Fatalf("server saw %d instrument params %q, want %d (%q)", len(got), got, len(keys), keys)
	}
	for i, k := range keys {
		if got[i] != k {
			t.Errorf("instrument param %d = %q, want %q", i, got[i], k)
		}
	}
}
