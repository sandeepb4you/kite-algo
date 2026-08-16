package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/events"
	"kite-algo/internal/history"
	"kite-algo/internal/risk"
)

// Live risk is not the same kind of object as paper risk.
//
// Paper limits are a dial: the operator tunes them from /risk while a strategy
// is being evaluated, and getting one wrong costs nothing. Live limits guard
// real capital, so they are DERIVED and LOCKED — computed from the account's
// own opening balance, unreachable from the UI, and changeable only by editing
// config and restarting. A limit you can loosen from a browser at the moment it
// starts hurting is not a limit.

// liveLockoutKey is the settings row recording the date live trading was
// locked out on. Persisted rather than held in memory: a lockout that a restart
// clears is not a lockout, and the restart is exactly what an operator reaches
// for after a bad morning.
const liveLockoutKey = "live_lockout_date"

// LiveRisk owns the real book's limits and the day lockout.
type LiveRisk struct {
	mu sync.RWMutex

	// maxLossPct is the fraction of opening balance allowed as a daily loss.
	maxLossPct float64
	// openingBalance is snapshotted per trading day. Snapshotted rather than
	// read live because available margin FALLS as a position moves against you:
	// a limit derived from it would tighten exactly when the position is
	// already hurting, moving the goalposts mid-trade.
	openingBalance float64
	balanceDay     string

	// lockedDay is the IST date live entries are barred on, "" when open.
	lockedDay string
	reason    string
}

// NewLiveRisk builds the live policy from config.
func NewLiveRisk(maxLossPct float64) *LiveRisk {
	if maxLossPct <= 0 {
		maxLossPct = 1.0
	}
	return &LiveRisk{maxLossPct: maxLossPct}
}

// Restore reads a persisted lockout so a restart cannot clear it.
func (l *LiveRisk) Restore(ctx context.Context, store interface {
	GetSetting(context.Context, string) (string, bool, error)
}) {
	if store == nil {
		return
	}
	v, ok, err := store.GetSetting(ctx, liveLockoutKey)
	if err != nil || !ok || v == "" {
		return
	}
	l.mu.Lock()
	l.lockedDay = v
	l.reason = "daily loss limit reached earlier today"
	l.mu.Unlock()
}

// ObserveBalance records the day's opening balance, snapshotting once per day.
func (l *LiveRisk) ObserveBalance(opening float64, now time.Time) {
	if opening <= 0 {
		return
	}
	today := now.In(history.IST).Format("2006-01-02")

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.balanceDay == today {
		return
	}
	l.balanceDay = today
	l.openingBalance = opening
}

// MaxDailyLoss is the rupee limit derived from the opening balance.
//
// Returns 0 — meaning "unknown", not "unlimited" — before a balance is seen.
// Callers must treat that as a reason to refuse live entries rather than to
// allow them; see Allow.
func (l *LiveRisk) MaxDailyLoss() float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.openingBalance * l.maxLossPct / 100
}

// OpeningBalance reports the snapshotted balance and the day it belongs to.
func (l *LiveRisk) OpeningBalance() (float64, string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.openingBalance, l.balanceDay
}

// MaxLossPct reports the configured percentage.
func (l *LiveRisk) MaxLossPct() float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.maxLossPct
}

// Allow reports whether a new live ENTRY may be placed now.
//
// Exits are never consulted here — the caller exempts them. Every rule in this
// file caps risk being taken on; applied to a flatten they would trap the
// operator in the position the rule was written to protect them from.
func (l *LiveRisk) Allow(now time.Time) (bool, string) {
	today := now.In(history.IST).Format("2006-01-02")

	l.mu.RLock()
	locked, reason, opening := l.lockedDay, l.reason, l.openingBalance
	l.mu.RUnlock()

	if locked == today {
		return false, "live trading is locked out for the rest of today: " + reason
	}
	if opening <= 0 {
		// Fail closed. An unknown balance means the 1% limit cannot be computed,
		// and trading real money against a limit nobody can evaluate is worse
		// than not trading.
		return false, "account balance not yet known, so the daily-loss limit " +
			"cannot be computed; live entries are refused until Zerodha reports it"
	}
	return true, ""
}

// Trip locks live entries for the rest of the day and persists it.
func (l *LiveRisk) Trip(ctx context.Context, now time.Time, reason string, store interface {
	SetSetting(context.Context, string, string) error
}) bool {
	today := now.In(history.IST).Format("2006-01-02")

	l.mu.Lock()
	if l.lockedDay == today {
		l.mu.Unlock()
		return false
	}
	l.lockedDay = today
	l.reason = reason
	l.mu.Unlock()

	if store != nil {
		if err := store.SetSetting(ctx, liveLockoutKey, today); err != nil {
			return true // in-memory lockout still holds; the log records the failure
		}
	}
	return true
}

// Lockout reports the current lockout state for display.
func (l *LiveRisk) Lockout() (day, reason string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lockedDay, l.reason
}

// Limits renders the live policy as risk.Limits for the engine's risk manager.
//
// Everything except the daily loss comes from config and never moves; the daily
// loss is recomputed from the snapshotted balance.
func (l *LiveRisk) Limits(base risk.Limits) risk.Limits {
	out := base
	out.MaxDailyLoss = l.MaxDailyLoss()
	return out
}

// Describe renders the policy for the UI, which shows it read-only.
func (l *LiveRisk) Describe() string {
	opening, day := l.OpeningBalance()
	if opening <= 0 {
		return fmt.Sprintf("%.2f%% of opening balance (balance not yet known)", l.MaxLossPct())
	}
	return fmt.Sprintf("%.2f%% of %.2f opening balance on %s = %.2f",
		l.MaxLossPct(), opening, day, l.MaxDailyLoss())
}

// WatchRealPnL trips the day lockout as soon as the real book's loss reaches
// the limit, rather than waiting for the next order to be rejected.
//
// Tripping on the P&L rather than on a rejection matters: the operator may
// place nothing for an hour after the breach, and the lockout should already be
// in force — and visible — by then, not discovered when they next try to trade.
func (a *App) WatchRealPnL(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.checkRealLoss(ctx)
		}
	}
}

// checkRealLoss trips the lockout if the real book has breached its cap.
func (a *App) checkRealLoss(ctx context.Context) {
	if a.LiveRisk == nil {
		return
	}
	limit := a.LiveRisk.MaxDailyLoss()
	if limit <= 0 {
		return
	}
	pnl := a.Engine.BookPnL(broker.BookReal)
	if pnl > -limit {
		return
	}

	reason := fmt.Sprintf("real P&L %.2f reached the %.2f limit (%s)",
		pnl, limit, a.LiveRisk.Describe())
	if !a.LiveRisk.Trip(ctx, time.Now(), reason, a.Store) {
		return // already locked today
	}

	if a.Log != nil {
		a.Log.Error("LIVE TRADING LOCKED OUT FOR THE DAY", "reason", reason)
	}
	a.Bus.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelError,
		Message: "Live trading locked out for the rest of today — " + reason,
		Fields:  map[string]any{"live_lockout": true},
	})
}
