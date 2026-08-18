package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kite-algo/internal/history"
)

// Pushing the missing-session alert somewhere the operator will see it.
//
// SessionAlert already decides whether there is a problem. This decides whether
// to say it again, which is the harder half. An alert channel has two failure
// modes and they pull in opposite directions: say it once and it is missed, say
// it every minute and it is muted — and a muted channel is worse than none,
// because it still looks like it is working.
//
// The rules, in the order they are applied:
//
//   - Nothing before the exchange opens, nothing on a holiday or a weekend.
//     Both come free from SessionAlert.
//   - The first alert of the day goes out as soon as the condition appears.
//   - An escalation from warn to critical sends immediately, whatever the repeat
//     timer says. "The capture has now been missed" is new information, not a
//     repeat of "log in soon".
//   - Otherwise repeat no more than once per RepeatEvery.
//   - When the session comes back, send one resolution message and stop. Silence
//     after an alert is ambiguous — it reads the same as a notifier that died —
//     so the channel says so explicitly and then goes quiet.

// sessionWatchKey is where the notifier's per-day state is persisted.
const sessionWatchKey = "notify.session"

// sessionWatchTick is how often the condition is re-evaluated. Cheap: one
// snapshot read, one calendar check, and at most one indexed row lookup.
const sessionWatchTick = time.Minute

// sessionWatchState is what survives a restart.
//
// Persisted rather than held in memory because a redeploy is a normal event
// here — redeploy.sh recreates the container — and an in-memory flag would make
// every restart re-announce a condition the operator has already been told
// about, three times on a busy afternoon. Keyed by day so it self-clears.
type sessionWatchState struct {
	Day      string    `json:"day"`
	Level    string    `json:"level"`
	SentAt   time.Time `json:"sent_at"`
	Resolved bool      `json:"resolved"`
}

// alerter is the outbound channel. An interface so the watcher can be tested
// without a network, and so a second channel can be added without touching the
// decision logic.
type alerter interface {
	Send(ctx context.Context, text string) error
	Configured() bool
}

// startSessionWatch launches the alert loop, or explains why it did not.
//
// Silence here would be the worst outcome: an operator who has configured a bot
// token and gets nothing has no way to tell "no alerts because all is well" from
// "no alerts because it never started".
func (a *App) startSessionWatch(ctx context.Context, out alerter) {
	if !a.Cfg.Notify.Telegram.Enabled {
		return
	}
	if out == nil || !out.Configured() {
		if a.Log != nil {
			a.Log.Warn("telegram alerts are enabled but not configured; " +
				"set notify.telegram.bot_token in the secrets file and chat_id in config")
		}
		return
	}
	if a.Log != nil {
		a.Log.Info("telegram alerts active",
			"repeat_every", a.Cfg.Notify.Telegram.RepeatEvery.D,
			"capture_deadline", a.Cfg.Capture.RunAt+" IST")
	}
	go a.watchSession(ctx, out)
}

// watchSession is the alert loop.
func (a *App) watchSession(ctx context.Context, out alerter) {
	t := time.NewTicker(sessionWatchTick)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.checkSessionAlert(ctx, out)
		}
	}
}

// checkSessionAlert evaluates the condition once and sends if the rules say to.
func (a *App) checkSessionAlert(ctx context.Context, out alerter) {
	alert := a.SessionAlert(ctx)
	state := a.loadSessionWatchState(ctx)
	today := time.Now().In(history.IST).Format("2006-01-02")

	// Recovered.
	if !alert.Show {
		if state.Day == today && state.Level != "" && !state.Resolved {
			if err := out.Send(ctx, a.sessionResolvedMessage()); err != nil {
				a.logAlertFailure("session restored", err)
				return
			}
			state.Resolved = true
			a.saveSessionWatchState(ctx, state)
		}
		return
	}

	level := alert.Level()
	reason := sessionAlertSendReason(level, today, state,
		time.Now(), a.Cfg.Notify.Telegram.RepeatEvery.D)
	if reason == reasonNone {
		return
	}

	if err := out.Send(ctx, a.sessionAlertMessage(alert)); err != nil {
		// Deliberately NOT recorded as sent, so the next tick retries. A
		// transient outage at Telegram must not swallow the day's only alert.
		a.logAlertFailure(level, err)
		return
	}

	a.saveSessionWatchState(ctx, sessionWatchState{
		Day: today, Level: level, SentAt: time.Now(), Resolved: false,
	})
	if a.Log != nil {
		a.Log.Warn("session alert sent",
			"level", level, "reason", reason, "headline", alert.Headline)
	}
}

// sendReason is why a message is going out. Logged, so a repeat can be told from
// an escalation after the fact.
type sendReason string

const (
	reasonNone      sendReason = ""
	reasonFirst     sendReason = "first-of-day"
	reasonEscalated sendReason = "escalated-to-critical"
	reasonRecurred  sendReason = "recurred-after-resolution"
	reasonRepeat    sendReason = "repeat"
)

// sessionAlertSendReason applies the send rules.
//
// Pure, and separated from everything around it on purpose: this is where an
// alert channel is won or lost, and the inputs it depends on — the wall clock,
// the exchange calendar, a database row — are all things that make a test either
// flaky or silent. A suite that cannot run these rules on a Sunday is a suite
// that does not run them.
func sessionAlertSendReason(
	level, today string, st sessionWatchState, now time.Time, repeat time.Duration,
) sendReason {
	if level == "" {
		return reasonNone
	}
	// A different day, or no state at all: this is the first of the day.
	if st.Day != today || st.Level == "" {
		return reasonFirst
	}
	// New information, not a repeat — send regardless of the timer.
	if st.Level == "warn" && level == "critical" {
		return reasonEscalated
	}
	// Resolved earlier today and back again. A session lapsing twice in one day
	// is two events, and the second one is not a repeat of the first.
	if st.Resolved {
		return reasonRecurred
	}
	// A zero or negative repeat interval means "do not repeat". Treated as an
	// explicit choice rather than clamped to a default: an operator who wants
	// exactly one message a day is entitled to it, and silently repeating every
	// tick because they wrote 0 is the noise failure this whole file avoids.
	if repeat <= 0 {
		return reasonNone
	}
	if now.Sub(st.SentAt) >= repeat {
		return reasonRepeat
	}
	return reasonNone
}

// sessionAlertMessage renders the alert as the text of one message.
//
// The link is the point. The fix is a browser login, and an alert that names a
// problem without a route to the fix makes the operator go find a bookmark on a
// phone — which is exactly the friction that produces "I'll do it when I get
// back", which is how the capture gets missed.
func (a *App) sessionAlertMessage(alert SessionAlert) string {
	var b strings.Builder

	if alert.Critical {
		b.WriteString("[CRITICAL] ")
	}
	b.WriteString(alert.Headline)
	b.WriteString("\n\n")
	b.WriteString(alert.Detail)

	if url := a.connectURL(); url != "" {
		b.WriteString("\n\nLog in: ")
		b.WriteString(url)
	}
	b.WriteString("\n\n")
	b.WriteString(time.Now().In(history.IST).Format("02 Jan 15:04 IST"))
	return b.String()
}

// sessionResolvedMessage closes the loop after an alert.
func (a *App) sessionResolvedMessage() string {
	msg := "Zerodha session is connected again."
	if a.Cfg.Capture.Enabled {
		now := time.Now().In(history.IST)
		if a.CaptureWindowPassed(now) {
			msg += " Today's capture window (" + a.Cfg.Capture.RunAt +
				" IST) has already passed, so run a manual capture for today if you need it."
		} else {
			msg += " The option capture will run at " + a.Cfg.Capture.RunAt + " IST."
		}
	}
	return msg + "\n\n" + time.Now().In(history.IST).Format("02 Jan 15:04 IST")
}

// connectURL builds the deep link to the login page, or "" if this deployment
// does not know its own public address.
func (a *App) connectURL() string {
	base := strings.TrimRight(strings.TrimSpace(a.Cfg.Web.PublicURL), "/")
	if base == "" {
		return ""
	}
	return base + "/connect"
}

// logAlertFailure reports a send failure without letting it become noise of its
// own — this runs every minute while the condition holds.
func (a *App) logAlertFailure(what string, err error) {
	if a.Log != nil {
		a.Log.Error("could not send session alert", "alert", what, "err", err)
	}
}

func (a *App) loadSessionWatchState(ctx context.Context) sessionWatchState {
	var st sessionWatchState
	raw, ok, err := a.Store.GetSetting(ctx, sessionWatchKey)
	if err != nil || !ok {
		return st
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		// Corrupt state means "alert again", which is the safe direction: a
		// duplicate message costs attention, a swallowed one costs data.
		return sessionWatchState{}
	}
	return st
}

func (a *App) saveSessionWatchState(ctx context.Context, st sessionWatchState) {
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := a.Store.SetSetting(ctx, sessionWatchKey, string(raw)); err != nil && a.Log != nil {
		// Not fatal, but worth saying: without this the next tick re-sends.
		a.Log.Warn("could not persist session-alert state; the alert may repeat",
			"err", fmt.Sprintf("%v", err))
	}
}
