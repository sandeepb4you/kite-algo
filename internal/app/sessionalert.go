package app

import (
	"context"
	"strings"
	"time"

	"kite-algo/internal/history"
	"kite-algo/internal/storage"
)

// The missing-session warning.
//
// Zerodha invalidates access tokens daily around 06:00 IST and they can only be
// renewed through an interactive browser login, so every trading day begins with
// no session. Forgetting is the single most expensive routine mistake available
// here, and it is a quiet one: the platform keeps serving pages, the strategies
// page still lists strategies, and nothing looks broken.
//
// What is actually lost is not recoverable. The instrument snapshot and the
// daily option capture both run off that session, and Kite drops expired
// contracts from its historical feed entirely — so a contract that expires
// before the next login takes its price history with it, permanently.
//
// The decision lives here, in app, rather than in the web layer that first grew
// it. Two things consume it now — the sticky banner and the Telegram alert — and
// a second copy of "is this worth warning about, and how loudly" would drift
// from the first within a release. The web layer renders what this returns.

// SessionAlert is the alert state for a missing Zerodha session.
type SessionAlert struct {
	Show bool
	// Critical marks the point after which today's capture has already been
	// missed, which is a different problem from "log in soon".
	Critical bool
	Headline string
	Detail   string
}

// Level names the alert for logs and de-duplication. Empty when not showing.
func (a SessionAlert) Level() string {
	switch {
	case !a.Show:
		return ""
	case a.Critical:
		return "critical"
	default:
		return "warn"
	}
}

// SessionAlert decides whether to warn about a missing Zerodha session, and how
// loudly.
//
// Silent on non-trading days: there is nothing to capture on a Sunday, and an
// alert that cries wolf all weekend is one nobody reads on Monday.
func (a *App) SessionAlert(ctx context.Context) SessionAlert {
	if a.Kite.Snapshot().Connected() {
		return SessionAlert{}
	}

	cal := history.NSE()
	cal.SetHolidays(a.Cfg.Capture.Holidays)
	now := time.Now().In(history.IST)
	if !cal.IsTradingDay(now) {
		return SessionAlert{}
	}

	// Before the exchange opens there is still plenty of time and no data at
	// risk yet; nagging from midnight would train the operator to ignore this.
	session, ok := cal.SessionFor(now)
	if !ok || now.Before(session.From) {
		return SessionAlert{}
	}

	haveSnapshot := a.haveSnapshotToday(ctx)
	captureMissed := a.CaptureWindowPassed(now)

	switch {
	case captureMissed:
		// The 15:40 job needed a session and did not get one. Anything expiring
		// today is now unrecoverable, which no later login can undo.
		return SessionAlert{
			Show: true, Critical: true,
			Headline: "No Zerodha session — today's option capture has been missed.",
			Detail: "The capture runs at " + a.Cfg.Capture.RunAt +
				" IST and needs a live session. Contracts expiring today have " +
				"taken their price history with them; Kite cannot supply it again. " +
				"Connect now to salvage the contracts that have not expired yet.",
		}

	case !haveSnapshot:
		return SessionAlert{
			Show: true, Critical: true,
			Headline: "No Zerodha session — today has no instrument snapshot.",
			Detail: "Nothing traded today can be resolved for a future backtest " +
				"until you connect, and option candles are only captured while a " +
				"session is live. Neither can be recovered after the contracts expire.",
		}

	default:
		// The snapshot is safe — they connected earlier and the token lapsed.
		// Still worth saying, because the capture has not run yet.
		return SessionAlert{
			Show: true,
			Headline: "Zerodha session has ended. Reconnect before " +
				a.Cfg.Capture.RunAt + " IST.",
			Detail: "Today's instrument snapshot is already saved, so that is safe. " +
				"The option capture still needs a live session, and no live data " +
				"or trading is possible until you reconnect.",
		}
	}
}

// haveSnapshotToday reports whether today's instrument master is already stored.
func (a *App) haveSnapshotToday(ctx context.Context) bool {
	store, ok := a.Store.(storage.HistoryStore)
	if !ok {
		return false
	}
	have, err := store.HasInstrumentSnapshot(ctx, time.Now().In(history.IST))
	return err == nil && have
}

// CaptureWindowPassed reports whether the daily capture time has gone by.
//
// Only meaningful when capture is enabled: with it off, nothing was scheduled
// to be missed and claiming otherwise would be false alarm.
func (a *App) CaptureWindowPassed(now time.Time) bool {
	if !a.Cfg.Capture.Enabled {
		return false
	}
	t, err := time.Parse("15:04", strings.TrimSpace(a.Cfg.Capture.RunAt))
	if err != nil {
		return false
	}
	local := now.In(history.IST)
	runAt := time.Date(local.Year(), local.Month(), local.Day(),
		t.Hour(), t.Minute(), 0, 0, history.IST)
	return local.After(runAt)
}
