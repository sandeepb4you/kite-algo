package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"kite-algo/internal/config"
	"kite-algo/internal/history"
)

// A manual trigger pressed on a Sunday must target Friday. Capture skips
// non-trading days, so defaulting to "today" would report success having
// captured nothing — the exact failure this subsystem exists to prevent.
func TestCaptureTargetDayDefaultsToLastTradingDay(t *testing.T) {
	cal := history.NSE()

	day, err := captureTargetDay(cal, "")
	if err != nil {
		t.Fatalf("captureTargetDay: %v", err)
	}
	switch day.Weekday() {
	case time.Saturday, time.Sunday:
		t.Errorf("defaulted to %s, a non-trading day", day.Weekday())
	}
	if day.After(time.Now().In(history.IST)) {
		t.Errorf("defaulted to %s, which is in the future", day.Format("2006-01-02"))
	}
}

func TestCaptureTargetDayRejectsNonTradingDays(t *testing.T) {
	cal := history.NSE()

	// 2026-08-16 is a Sunday.
	if _, err := captureTargetDay(cal, "2026-08-16"); err == nil {
		t.Error("accepted a Sunday")
	} else if !strings.Contains(err.Error(), "Sunday") {
		t.Errorf("unhelpful error: %v", err)
	}

	// A configured holiday is equally uncapturable.
	cal.SetHolidays([]string{"2026-08-14"})
	if _, err := captureTargetDay(cal, "2026-08-14"); err == nil {
		t.Error("accepted a configured holiday")
	}
}

func TestCaptureTargetDayAcceptsATradingDay(t *testing.T) {
	// 2026-08-14 is a Friday.
	day, err := captureTargetDay(history.NSE(), "2026-08-14")
	if err != nil {
		t.Fatalf("rejected a valid trading day: %v", err)
	}
	if got := day.Format("2006-01-02"); got != "2026-08-14" {
		t.Errorf("got %s, want 2026-08-14", got)
	}
}

func TestCaptureTargetDayRejectsTheFuture(t *testing.T) {
	future := time.Now().In(history.IST).AddDate(0, 0, 30)
	// Step to a weekday so the rejection is about the date, not the weekend.
	for {
		if wd := future.Weekday(); wd != time.Saturday && wd != time.Sunday {
			break
		}
		future = future.AddDate(0, 0, 1)
	}
	if _, err := captureTargetDay(history.NSE(), future.Format("2006-01-02")); err == nil {
		t.Error("accepted a future date")
	}
}

func TestCaptureTargetDayRejectsGarbage(t *testing.T) {
	if _, err := captureTargetDay(history.NSE(), "last friday"); err == nil {
		t.Error("accepted an unparseable date")
	}
}

// With no Zerodha session the run cannot proceed, and the button must say so
// rather than starting a job that fails on every request.
func TestCaptureRunRefusesWithoutASession(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)

	if a.Kite.Snapshot().Connected() {
		t.Fatal("test server unexpectedly has a live session")
	}
	// Capture is disabled in the test config, which is the first gate.
	resp, err := c.PostForm(ts.URL+"/api/capture/run", url.Values{
		"_csrf": {csrfFor(t, ts, c)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)
	if !strings.Contains(body, "disabled") && !strings.Contains(body, "log in") {
		t.Errorf("no clear refusal; got: %s", truncate(body, 300))
	}
}

// The panel must show the target day so the operator knows what the button
// will actually do before pressing it.
func TestOptionsPageShowsCapturePanel(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	seedOptionDay(t, a, time.Date(2026, 8, 14, 0, 0, 0, 0, history.IST))

	body := getBody(t, c, ts.URL+"/options?date=2026-08-14")
	if !strings.Contains(body, "Capture") {
		t.Error("capture panel missing from the options page")
	}
}

func TestCaptureFragmentRenders(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loginClient(t, ts)

	code, body := getStatusBody(t, c, ts.URL+"/partials/capture")
	if code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", code, truncate(body, 300))
	}
}

// TestCapturePanelOffersAnEditableDay pins the day as a real date input rather
// than the hidden field it used to be.
//
// Backfilling a day that was missed is the main reason to run a capture by
// hand, and the panel previously only offered the most recent trading day —
// any other date meant stopping the container and running the CLI. The server
// already accepted an arbitrary date; only the form withheld it.
func TestCapturePanelOffersAnEditableDay(t *testing.T) {
	ts, _ := newTestServerWith(t, func(cfg *config.Config) {
		cfg.Capture.Enabled = true
		cfg.Capture.RunAt = "15:40"
		cfg.Capture.Interval = "5minute"
	})
	c := loginClient(t, ts)

	_, body := getStatusBody(t, c, ts.URL+"/partials/capture")

	if strings.Contains(body, `type="hidden" name="date"`) {
		t.Error("the capture day is still a hidden field; it cannot be changed from the UI")
	}
	if !strings.Contains(body, `name="date" type="date"`) {
		t.Errorf("no date input in the capture panel:\n%s", truncate(body, 400))
	}
	// A future day has no data and the server refuses it; the form should not
	// offer it in the first place.
	if !strings.Contains(body, "max=") {
		t.Error("the date input has no max, so it offers days that cannot be captured")
	}
}
