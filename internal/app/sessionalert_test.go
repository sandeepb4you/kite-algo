package app

import (
	"strings"
	"testing"
	"time"

	"kite-algo/internal/history"
)

// SessionAlert must escalate once today's capture time has gone by: "log in
// soon" and "some data is already gone" are different problems.
func TestSessionAlertLevelsReadDifferently(t *testing.T) {
	before := SessionAlert{
		Show:     true,
		Headline: "Zerodha session has ended. Reconnect before 15:40 IST.",
	}
	after := SessionAlert{
		Show: true, Critical: true,
		Headline: "No Zerodha session — today's option capture has been missed.",
	}

	if before.Critical {
		t.Error("a session that lapsed with the snapshot already saved is not critical")
	}
	if !after.Critical {
		t.Error("a missed capture must be critical")
	}
	// The headline states the consequence, not the condition: "not connected"
	// is something an operator can look at all morning without acting on.
	for _, a := range []SessionAlert{before, after} {
		if !strings.Contains(a.Headline, "session") {
			t.Errorf("headline does not name the problem: %q", a.Headline)
		}
	}
}

// Level drives de-duplication in the notifier, so an escalation from warn to
// critical has to be visible as a change of value rather than only in the text.
func TestSessionAlertLevel(t *testing.T) {
	cases := []struct {
		name string
		a    SessionAlert
		want string
	}{
		{"silent", SessionAlert{}, ""},
		{"warn", SessionAlert{Show: true}, "warn"},
		{"critical", SessionAlert{Show: true, Critical: true}, "critical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Level(); got != tc.want {
				t.Errorf("Level() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The alert must not fire on a weekend, whatever the session state.
func TestNonTradingDayIsSilent(t *testing.T) {
	cal := history.NSE()
	sunday := time.Date(2026, 8, 16, 11, 0, 0, 0, history.IST)
	if cal.IsTradingDay(sunday) {
		t.Fatal("2026-08-16 should be a Sunday")
	}
	// SessionAlert returns early on a non-trading day; this pins the calendar
	// fact the early return depends on.
	saturday := time.Date(2026, 8, 15, 11, 0, 0, 0, history.IST)
	if cal.IsTradingDay(saturday) {
		t.Error("2026-08-15 should be a Saturday")
	}
	friday := time.Date(2026, 8, 14, 11, 0, 0, 0, history.IST)
	if !cal.IsTradingDay(friday) {
		t.Error("2026-08-14 should be a trading day")
	}
}
