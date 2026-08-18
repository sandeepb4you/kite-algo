package app

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kite-algo/internal/config"
)

// The send rules, exhaustively. This is where an alert channel is won or lost:
// too quiet and the operator misses the morning the capture dies, too loud and
// the channel gets muted and every later alert is lost with it.
func TestSessionAlertSendReason(t *testing.T) {
	const today = "2026-08-19"
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	const repeat = 30 * time.Minute

	sent := func(day, level string, ago time.Duration, resolved bool) sessionWatchState {
		return sessionWatchState{
			Day: day, Level: level, SentAt: now.Add(-ago), Resolved: resolved,
		}
	}

	cases := []struct {
		name  string
		level string
		st    sessionWatchState
		want  sendReason
	}{
		{"nothing wrong stays silent", "", sent(today, "warn", time.Minute, false), reasonNone},
		{"no state at all is the first of the day", "warn", sessionWatchState{}, reasonFirst},
		{"yesterday's state does not suppress today", "warn",
			sent("2026-08-18", "critical", time.Minute, false), reasonFirst},
		{"a fresh warn is not repeated", "warn", sent(today, "warn", time.Minute, false), reasonNone},
		{"a stale warn is repeated", "warn", sent(today, "warn", 31*time.Minute, false), reasonRepeat},
		{"exactly at the interval repeats", "warn", sent(today, "warn", repeat, false), reasonRepeat},

		// The escalation must not wait for the timer: "the capture has now been
		// missed" is new information and the operator can still salvage the
		// contracts that have not expired yet.
		{"escalation beats the timer", "critical",
			sent(today, "warn", time.Minute, false), reasonEscalated},
		{"a fresh critical is not repeated", "critical",
			sent(today, "critical", time.Minute, false), reasonNone},
		{"critical does not de-escalate to a resend", "warn",
			sent(today, "critical", time.Minute, false), reasonNone},

		// Lapsing twice in one day is two events.
		{"recurrence after resolution sends again", "warn",
			sent(today, "warn", time.Minute, true), reasonRecurred},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionAlertSendReason(tc.level, today, tc.st, now, repeat)
			if got != tc.want {
				t.Errorf("sessionAlertSendReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

// repeat_every: 0 means "tell me once", not "tell me every minute".
func TestSessionAlertZeroRepeatMeansOnce(t *testing.T) {
	const today = "2026-08-19"
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	st := sessionWatchState{Day: today, Level: "warn", SentAt: now.Add(-6 * time.Hour)}

	if got := sessionAlertSendReason("warn", today, st, now, 0); got != reasonNone {
		t.Errorf("with repeat 0 after six hours: got %q, want silence", got)
	}
	// An escalation still gets through, because it is not a repeat.
	if got := sessionAlertSendReason("critical", today, st, now, 0); got != reasonEscalated {
		t.Errorf("an escalation was suppressed by repeat 0: got %q", got)
	}
}

// recordingAlerter is an alerter that captures what it was asked to send, and
// can be made to fail.
type recordingAlerter struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (r *recordingAlerter) Configured() bool { return true }
func (r *recordingAlerter) Send(_ context.Context, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, text)
	return nil
}
func (r *recordingAlerter) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

// notifyTestApp builds an app whose config carries a public URL and an enabled
// Telegram section, over a real sqlite store so the persisted state is exercised.
func notifyTestApp(t *testing.T) *App {
	t.Helper()
	a := riskTestApp(t, filepath.Join(t.TempDir(), "notify.db"), configDefaults)
	a.Cfg.Web.PublicURL = "https://trade.example.com/"
	a.Cfg.Capture.Enabled = true
	a.Cfg.Capture.RunAt = "15:40"
	a.Cfg.Notify.Telegram = config.TelegramConfig{
		Enabled: true, BotToken: "t", ChatID: "1",
		RepeatEvery: config.Interval{D: 30 * time.Minute, Set: true},
	}
	return a
}

// The message has to carry the fix, not just the diagnosis. An operator reading
// it on a phone should be one tap from the login page.
func TestSessionAlertMessageCarriesTheLoginLink(t *testing.T) {
	a := notifyTestApp(t)

	msg := a.sessionAlertMessage(SessionAlert{
		Show: true, Critical: true,
		Headline: "No Zerodha session — today's option capture has been missed.",
		Detail:   "Contracts expiring today have taken their price history with them.",
	})

	if !strings.Contains(msg, "https://trade.example.com/connect") {
		t.Errorf("no login link in the alert:\n%s", msg)
	}
	// The trailing slash on public_url must not produce a double slash.
	if strings.Contains(msg, "com//connect") {
		t.Errorf("malformed login link:\n%s", msg)
	}
	if !strings.Contains(msg, "CRITICAL") {
		t.Error("a critical alert is not marked as one")
	}
	if !strings.Contains(msg, "capture has been missed") {
		t.Error("the headline is missing")
	}
}

// Without a public URL there is no link to give, and the message must still be
// worth reading rather than ending in a dangling label.
func TestSessionAlertMessageWithoutAPublicURL(t *testing.T) {
	a := notifyTestApp(t)
	a.Cfg.Web.PublicURL = ""

	msg := a.sessionAlertMessage(SessionAlert{Show: true, Headline: "Session ended."})
	if strings.Contains(msg, "Log in:") {
		t.Errorf("offered a login link with no URL behind it:\n%s", msg)
	}
	if !strings.Contains(msg, "Session ended.") {
		t.Error("the headline is missing")
	}
}

// A failed send must NOT be recorded as sent. Telegram being briefly unreachable
// at 09:15 must not consume the day's alert and leave the operator uninformed.
func TestFailedSendIsRetriedOnTheNextTick(t *testing.T) {
	ctx := context.Background()
	a := notifyTestApp(t)

	// State from a send that never landed would look like this if the failure
	// had been recorded; prove it is absent instead.
	out := &recordingAlerter{err: context.DeadlineExceeded}
	a.checkSessionAlert(ctx, out)

	st := a.loadSessionWatchState(ctx)
	if st.Level != "" || !st.SentAt.IsZero() {
		t.Errorf("a failed send was recorded as sent: %+v", st)
	}
}

// Corrupt persisted state must fail towards alerting. A duplicate message costs
// attention; a swallowed one costs a day of option data that cannot be refetched.
func TestCorruptStateFailsTowardsAlerting(t *testing.T) {
	ctx := context.Background()
	a := notifyTestApp(t)

	if err := a.Store.SetSetting(ctx, sessionWatchKey, "{not json"); err != nil {
		t.Fatal(err)
	}
	st := a.loadSessionWatchState(ctx)
	if st.Level != "" || st.Day != "" {
		t.Errorf("corrupt state was trusted: %+v", st)
	}
	if got := sessionAlertSendReason("warn", "2026-08-19", st,
		time.Now(), 30*time.Minute); got != reasonFirst {
		t.Errorf("corrupt state suppressed an alert: got %q, want %q", got, reasonFirst)
	}
}

// The resolution message closes the loop. Silence after an alert reads exactly
// like a notifier that died, so recovery is stated rather than implied.
func TestResolutionMessageNamesTheCaptureDeadline(t *testing.T) {
	a := notifyTestApp(t)

	msg := a.sessionResolvedMessage()
	if !strings.Contains(msg, "connected again") {
		t.Errorf("resolution does not say the session is back:\n%s", msg)
	}
	if !strings.Contains(msg, "15:40") {
		t.Errorf("resolution does not mention the capture time:\n%s", msg)
	}
}

// With capture disabled the resolution must not promise a capture that will
// never run.
func TestResolutionSaysNothingAboutCaptureWhenItIsOff(t *testing.T) {
	a := notifyTestApp(t)
	a.Cfg.Capture.Enabled = false

	if msg := a.sessionResolvedMessage(); strings.Contains(msg, "capture") {
		t.Errorf("promised a capture that is switched off:\n%s", msg)
	}
}

// A disabled or unconfigured channel must not start a loop that fails every
// minute for the life of the process.
func TestSessionWatchDoesNotStartWhenUnusable(t *testing.T) {
	ctx := context.Background()
	a := notifyTestApp(t)

	// Nil alerter, enabled config: must be refused rather than panicking on the
	// first tick.
	a.startSessionWatch(ctx, nil)

	a.Cfg.Notify.Telegram.Enabled = false
	a.startSessionWatch(ctx, &recordingAlerter{})
	// Nothing to assert beyond not having started anything; the value is that
	// neither call panics and neither sends.
}
