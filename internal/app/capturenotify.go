package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"kite-algo/internal/history"
)

// The daily capture summary.
//
// The capture is the one job here whose output cannot be reconstructed. It runs
// unattended at 15:40, and its result today is either "the day's option data is
// safe" or "there is a permanent hole in it" — a distinction the operator
// currently learns by reading a container log, or never.
//
// So this reports both outcomes rather than only failures. A daily confirmation
// that the data landed is not noise here: it is the only positive evidence the
// backtester will have anything to run on, and its ABSENCE one afternoon is
// itself the signal that something stopped.

// captureNotifyKey is where the last reported capture outcome is persisted.
const captureNotifyKey = "notify.capture"

// captureNotifyState is the last outcome announced, per day.
//
// Needed because a FAILED capture retries every minute until midnight
// (CaptureScheduler.tick only records success), so without this a broken
// afternoon would send four hundred identical messages and the channel would be
// muted by the operator before the next real alert arrived.
type captureNotifyState struct {
	Day     string `json:"day"`
	Outcome string `json:"outcome"`
}

// Capture outcomes, in the order they matter.
const (
	captureOK      = "ok"
	capturePartial = "partial"
	captureFailed  = "failed"
)

// notifyCaptureDone announces the result of one capture run.
//
// Silent when the run was skipped: a weekend or a holiday has nothing to capture,
// and "nothing happened, as designed" every Saturday is exactly the traffic that
// teaches an operator to ignore the channel.
func (a *App) notifyCaptureDone(ctx context.Context, rep history.CaptureReport, capErr error) {
	if a.alerts == nil || rep.Skipped != "" {
		return
	}

	outcome := captureOK
	switch {
	case capErr != nil:
		outcome = captureFailed
	case rep.Failures > 0:
		outcome = capturePartial
	}

	// The report's own day, not today: a manual backfill of last Friday is a
	// different event from this afternoon's run and gets its own message.
	day := rep.Day.In(history.IST).Format("2006-01-02")
	if day == "0001-01-01" {
		day = time.Now().In(history.IST).Format("2006-01-02")
	}

	state := a.loadCaptureNotifyState(ctx)
	if state.Day == day && state.Outcome == outcome {
		return // already said this about this day
	}

	if err := a.alerts.Send(ctx, captureMessage(rep, capErr, outcome)); err != nil {
		// Not recorded, so the next tick retries — the same reasoning as the
		// session alert. A failed capture that also fails to notify is the worst
		// case and must not be silently dropped.
		a.logAlertFailure("capture "+outcome, err)
		return
	}
	a.saveCaptureNotifyState(ctx, captureNotifyState{Day: day, Outcome: outcome})
}

// captureMessage renders a report as one message.
//
// Per-underlying lines, because "6,240 candles" hides the failure that matters:
// SENSEX silently contributing nothing looks identical to a good day at the
// total, and BSE contracts are the ones that were being missed entirely before
// the instrument master was loaded per-exchange.
func captureMessage(rep history.CaptureReport, capErr error, outcome string) string {
	var b strings.Builder

	day := rep.Day
	if day.IsZero() {
		day = time.Now()
	}
	dayStr := day.In(history.IST).Format("02 Jan 2006")

	switch outcome {
	case captureFailed:
		b.WriteString("[CRITICAL] Option capture FAILED — " + dayStr)
		b.WriteString("\n\n")
		if capErr != nil {
			b.WriteString(capErr.Error())
			b.WriteString("\n\n")
		}
		b.WriteString("Contracts expiring today take their price history with them " +
			"and Kite will not serve it again. Re-run the capture from the options " +
			"page as soon as the cause is fixed.")
	case capturePartial:
		fmt.Fprintf(&b, "Option capture finished with %d failure(s) — %s", rep.Failures, dayStr)
	default:
		b.WriteString("Option capture complete — " + dayStr)
	}

	if len(rep.Underlying) > 0 {
		b.WriteString("\n")
		for _, u := range rep.Underlying {
			b.WriteString("\n")
			if u.Err != "" {
				fmt.Fprintf(&b, "%s: FAILED — %s", u.Underlying, u.Err)
				continue
			}
			fmt.Fprintf(&b, "%s: %d contracts, %d candles", u.Underlying, u.Contracts, u.Candles)
			if u.Spot > 0 {
				fmt.Fprintf(&b, " (spot %.0f", u.Spot)
				if u.Low > 0 && u.High > 0 {
					fmt.Fprintf(&b, ", strikes %.0f–%.0f", u.Low, u.High)
				}
				b.WriteString(")")
			}
			if u.Failures > 0 {
				fmt.Fprintf(&b, " — %d failed", u.Failures)
			}
		}
	}

	if outcome != captureFailed {
		fmt.Fprintf(&b, "\n\nTotal %d contracts, %d candles in %s",
			rep.Contracts, rep.Candles, rep.Duration.Round(time.Second))
	}
	return b.String()
}

func (a *App) loadCaptureNotifyState(ctx context.Context) captureNotifyState {
	var st captureNotifyState
	raw, ok, err := a.Store.GetSetting(ctx, captureNotifyKey)
	if err != nil || !ok {
		return st
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		// Fail towards announcing: a duplicate summary is harmless, a swallowed
		// failure notice is not.
		return captureNotifyState{}
	}
	return st
}

func (a *App) saveCaptureNotifyState(ctx context.Context, st captureNotifyState) {
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := a.Store.SetSetting(ctx, captureNotifyKey, string(raw)); err != nil && a.Log != nil {
		a.Log.Warn("could not persist capture-alert state; the summary may repeat", "err", err)
	}
}
