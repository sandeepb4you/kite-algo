package kite

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCandleUnmarshalPositionalArray covers Kite's wire format. Candles arrive
// as heterogeneous arrays, not objects, so a naive struct decode silently yields
// zero-valued bars — every price 0, which a backtest would happily "trade".
func TestCandleUnmarshalPositionalArray(t *testing.T) {
	raw := `["2024-08-01T09:15:00+0530",24500.1,24512.3,24495,24505.2,187500,91234]`

	var c HistoricalCandle
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantTime := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)
	if !c.Time.Equal(wantTime) {
		t.Errorf("time = %s, want %s", c.Time, wantTime)
	}
	if c.Open != 24500.1 || c.High != 24512.3 || c.Low != 24495 || c.Close != 24505.2 {
		t.Errorf("OHLC = %v/%v/%v/%v", c.Open, c.High, c.Low, c.Close)
	}
	if c.Volume != 187500 {
		t.Errorf("volume = %d, want 187500", c.Volume)
	}
	if c.OI != 91234 {
		t.Errorf("open interest = %d, want 91234", c.OI)
	}
}

func TestCandleUnmarshalWithoutOI(t *testing.T) {
	var c HistoricalCandle
	if err := json.Unmarshal([]byte(`["2024-08-01T09:15:00+0530",1,2,0.5,1.5,100]`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.OI != 0 {
		t.Errorf("OI = %d, want 0 when the column is absent", c.OI)
	}
}

func TestCandleUnmarshalRejectsMalformed(t *testing.T) {
	bad := []string{
		`{"time":"x"}`,                       // object, not array
		`["2024-08-01T09:15:00+0530",1,2,3]`, // too few fields
		`["not-a-time",1,2,3,4,5]`,           // unparseable timestamp
	}
	for _, in := range bad {
		var c HistoricalCandle
		if err := json.Unmarshal([]byte(in), &c); err == nil {
			t.Errorf("malformed candle %s was accepted", in)
		}
	}
}

func TestIntervalMaxDays(t *testing.T) {
	// Finer intervals must have tighter caps, or chunking would request more
	// data than Kite will return.
	prev := 0
	for _, i := range Intervals {
		got := i.MaxDays()
		if got <= 0 {
			t.Errorf("%s has a non-positive cap %d", i, got)
		}
		if got < prev {
			t.Errorf("%s cap %d is smaller than the previous interval's %d", i, got, prev)
		}
		prev = got
		if i.Duration() <= 0 {
			t.Errorf("%s has no duration", i)
		}
	}
}

func TestParseInterval(t *testing.T) {
	if _, ok := ParseInterval("minute"); !ok {
		t.Error("minute should be a valid interval")
	}
	if _, ok := ParseInterval("2minute"); ok {
		t.Error("2minute is not a Kite interval and should be rejected")
	}
}

// TestGetHistoricalSendsISTAndPath checks the request Kite actually receives.
// The from/to parameters are interpreted in exchange-local time, so sending UTC
// would silently shift every bar by five and a half hours.
func TestGetHistoricalSendsISTAndPath(t *testing.T) {
	var gotPath, gotFrom, gotTo, gotOI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFrom = r.URL.Query().Get("from")
		gotTo = r.URL.Query().Get("to")
		gotOI = r.URL.Query().Get("oi")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"candles":[]}}`))
	}))
	defer srv.Close()

	c := New("k", "s", "tok", srv.URL, nil)
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)
	to := time.Date(2024, 8, 1, 15, 30, 0, 0, IST)

	if _, err := c.GetHistorical(context.Background(), HistoricalRequest{
		InstrumentToken: 256265, Interval: IntervalMinute,
		From: from.UTC(), To: to.UTC(), OI: true, // pass UTC deliberately
	}); err != nil {
		t.Fatalf("GetHistorical: %v", err)
	}

	if want := "/instruments/historical/256265/minute"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if want := "2024-08-01 09:15:00"; gotFrom != want {
		t.Errorf("from = %q, want %q (IST, even though UTC was passed in)", gotFrom, want)
	}
	if want := "2024-08-01 15:30:00"; gotTo != want {
		t.Errorf("to = %q, want %q", gotTo, want)
	}
	if gotOI != "1" {
		t.Errorf("oi = %q, want 1", gotOI)
	}
}

func TestGetHistoricalRejectsOversizedRange(t *testing.T) {
	c := New("k", "s", "tok", "http://unused", nil)
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, IST)

	_, err := c.GetHistorical(context.Background(), HistoricalRequest{
		InstrumentToken: 1, Interval: IntervalMinute,
		From: from, To: from.AddDate(0, 0, 90), // 90 > the 60-day minute cap
	})
	if err == nil {
		t.Fatal("oversized range was accepted; Kite would have rejected it opaquely")
	}
	if !errors.Is(err, ErrRangeTooLarge) {
		t.Errorf("err = %v, want ErrRangeTooLarge", err)
	}
}

// TestGetHistoricalRangeChunks is the core of long backfills: a range longer
// than the per-interval cap must be split, and the pieces must tile the range
// without gaps.
func TestGetHistoricalRangeChunks(t *testing.T) {
	type window struct{ from, to string }
	var windows []window

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		windows = append(windows, window{q.Get("from"), q.Get("to")})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"candles":[]}}`))
	}))
	defer srv.Close()

	c := New("k", "s", "tok", srv.URL, nil)
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, IST)
	to := from.AddDate(0, 0, 150) // 150 days at a 60-day cap → 3 requests

	if _, err := c.GetHistoricalRange(context.Background(), HistoricalRequest{
		InstrumentToken: 256265, Interval: IntervalMinute, From: from, To: to,
	}); err != nil {
		t.Fatalf("GetHistoricalRange: %v", err)
	}

	if len(windows) != 3 {
		t.Fatalf("issued %d requests for 150 days at a 60-day cap, want 3: %+v",
			len(windows), windows)
	}
	// Each chunk must start where the previous ended: a gap would silently omit
	// candles from the backfill.
	for i := 1; i < len(windows); i++ {
		if windows[i].from != windows[i-1].to {
			t.Errorf("chunk %d starts at %s but the previous ended at %s — gap in coverage",
				i, windows[i].from, windows[i-1].to)
		}
	}
	if windows[0].from != "2024-01-01 00:00:00" {
		t.Errorf("first chunk starts at %s", windows[0].from)
	}
	if want := to.Format("2006-01-02 15:04:05"); windows[len(windows)-1].to != want {
		t.Errorf("last chunk ends at %s, want %s", windows[len(windows)-1].to, want)
	}
}

// TestGetHistoricalRangeDedupes covers the inclusive-boundary case, where the
// same candle can be returned by two adjacent chunks.
func TestGetHistoricalRangeDedupes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Every chunk returns the same bar.
		_, _ = w.Write([]byte(`{"status":"success","data":{"candles":[
			["2024-01-15T09:15:00+0530",1,2,0.5,1.5,100]]}}`))
	}))
	defer srv.Close()

	c := New("k", "s", "tok", srv.URL, nil)
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, IST)

	got, err := c.GetHistoricalRange(context.Background(), HistoricalRequest{
		InstrumentToken: 1, Interval: IntervalMinute,
		From: from, To: from.AddDate(0, 0, 150),
	})
	if err != nil {
		t.Fatalf("GetHistoricalRange: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d candles, want 1 after dedupe", len(got))
	}
}

// TestIsPermissionError distinguishes "you have not bought the historical data
// subscription" from a transient failure. Retrying the former never helps.
func TestIsPermissionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":"error","message":"insufficient permission","error_type":"PermissionException"}`))
	}))
	defer srv.Close()

	c := New("k", "s", "tok", srv.URL, nil)
	from := time.Date(2024, 8, 1, 9, 15, 0, 0, IST)

	_, err := c.GetHistorical(context.Background(), HistoricalRequest{
		InstrumentToken: 1, Interval: IntervalDay, From: from, To: from.AddDate(0, 0, 1),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsPermissionError(err) {
		t.Errorf("IsPermissionError(%v) = false, want true", err)
	}
}

func TestHistoricalRequestValidation(t *testing.T) {
	c := New("k", "s", "tok", "http://unused", nil)
	now := time.Now()
	cases := map[string]HistoricalRequest{
		"no token":    {Interval: IntervalDay, From: now, To: now.Add(time.Hour)},
		"no interval": {InstrumentToken: 1, From: now, To: now.Add(time.Hour)},
		"to before from": {InstrumentToken: 1, Interval: IntervalDay,
			From: now, To: now.Add(-time.Hour)},
	}
	for name, req := range cases {
		if _, err := c.GetHistorical(context.Background(), req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
