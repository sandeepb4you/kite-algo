package app

import (
	"context"
	"fmt"
	"time"

	"kite-algo/internal/config"
	"kite-algo/internal/history"
	"kite-algo/internal/kite"
	"kite-algo/internal/storage"
)

// Capture is the daily option-candle capture job, or nil when disabled.
func (a *App) Capture() *history.CaptureScheduler { return a.capture }

// CaptureOnce runs a single capture for one day and returns when it is done.
//
// This is the headless entry point — `trading -capture` and anything driving it
// from cron. It deliberately bypasses the scheduler: there is no timer to
// respect, no once-per-day flag to consult, and no web session to authenticate.
// The caller has already decided which day it wants.
func (a *App) CaptureOnce(ctx context.Context, day time.Time) (history.CaptureReport, error) {
	store, ok := a.Store.(storage.HistoryStore)
	if !ok {
		return history.CaptureReport{}, fmt.Errorf("this storage backend cannot store history")
	}
	if !a.Kite.Snapshot().Connected() || a.Kite.Client() == nil || a.Kite.Instruments() == nil {
		return history.CaptureReport{}, fmt.Errorf(
			"no Zerodha session — capture reads through the live API; " +
				"log in via the web UI first so a token is persisted")
	}
	return a.captureDay(ctx, store, day)
}

// startCapture builds and launches the capture scheduler.
//
// The scheduler is created even before a Zerodha session exists, because the
// interesting case is precisely the one where there is no session yet: it waits,
// re-checks every minute, and captures as soon as the operator logs in. Building
// it lazily on login would mean a login at 16:00 produced no capture at all.
func (a *App) startCapture(ctx context.Context) {
	if !a.Cfg.Capture.Enabled {
		if a.Log != nil {
			a.Log.Warn("daily option capture is DISABLED; expired-contract data " +
				"cannot be recovered later — set capture.enabled: true to record it")
		}
		return
	}
	store, ok := a.Store.(storage.HistoryStore)
	if !ok {
		if a.Log != nil {
			a.Log.Error("capture enabled but this storage backend cannot store history")
		}
		return
	}

	sched, err := history.NewCaptureScheduler(
		a.Cfg.Capture.RunAt,
		func(ctx context.Context, day time.Time) (history.CaptureReport, error) {
			return a.captureDay(ctx, store, day)
		},
		// Ready means a live session AND a loaded master. Capture resolves
		// contracts through the master and fetches with the session's token;
		// without either it would fail every request and burn the rate limit.
		func() bool {
			return a.Kite.Snapshot().Connected() &&
				a.Kite.Client() != nil &&
				a.Kite.Instruments() != nil
		},
		a.Log,
	)
	if err != nil {
		if a.Log != nil {
			a.Log.Error("could not start option capture", "err", err)
		}
		return
	}

	a.capture = sched
	if a.Log != nil {
		a.Log.Info("daily option capture scheduled",
			"run_at", a.Cfg.Capture.RunAt+" IST",
			"underlyings", captureNames(a.Cfg.Capture.Underlyings),
			"strikes", a.Cfg.Capture.Strikes,
			"expiries", a.Cfg.Capture.Expiries,
			"interval", a.Cfg.Capture.Interval)
	}
	go sched.Run(ctx)
}

// captureDay assembles a capturer against the current session and runs one day.
//
// It is rebuilt per run rather than held, because the instrument master and the
// authenticated client are both replaced on every login. A capturer captured at
// startup would hold yesterday's expired token and a master with no knowledge of
// this week's contracts.
func (a *App) captureDay(ctx context.Context, store storage.HistoryStore, day time.Time) (history.CaptureReport, error) {
	client := a.Kite.Client()
	instruments := a.Kite.Instruments()

	// Kite first, recorded ticks behind it, the cache in front of both. The
	// cache is what makes a re-run cheap and a missed day recoverable.
	upstream := history.NewChain(
		history.NewKiteProvider(client, instruments, a.Log),
		history.NewTickProvider(store, a.Log),
	)
	provider := history.NewCacheProvider(store, upstream, a.Log)

	cal := history.NSE()
	cal.SetHolidays(a.Cfg.Capture.Holidays)
	// Both layers must agree on the calendar, or the capturer skips a holiday
	// while the cache underneath it still requests the window.
	provider.SetCalendar(cal)

	interval, ok := kite.ParseInterval(a.Cfg.Capture.Interval)
	if !ok {
		interval = kite.Interval5Minute
	}

	targets := make([]history.CaptureTarget, 0, len(a.Cfg.Capture.Underlyings))
	for _, u := range a.Cfg.Capture.Underlyings {
		targets = append(targets, history.CaptureTarget{Underlying: u.Name, Index: u.Index})
	}

	cap := history.NewCapturer(provider, instruments, cal, history.CaptureOptions{
		Interval:    interval,
		Strikes:     a.Cfg.Capture.Strikes,
		Expiries:    a.Cfg.Capture.Expiries,
		Lookback:    time.Duration(a.Cfg.Capture.LookbackDays) * 24 * time.Hour,
		Underlyings: targets,
	}, a.Log)

	return cap.CaptureDay(ctx, day)
}

// captureNames lists the configured underlyings for a log line.
func captureNames(us []config.CaptureUnderlying) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.Name)
	}
	return out
}
