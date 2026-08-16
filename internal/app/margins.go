package app

import (
	"context"
	"sync"
	"time"

	"kite-algo/internal/kite"
)

// marginSegment is the Kite margin bucket that covers equity and F&O.
// Commodity (MCX) is a separate bucket this platform does not trade.
const marginSegment = "equity"

// marginRefreshInterval is how often the balance is re-fetched.
//
// Margins move only when an order is placed or a position is squared off, so
// polling faster buys nothing and spends a rate-limit budget that order
// placement needs. A refresh is also triggered on demand after a fill.
const marginRefreshInterval = 30 * time.Second

// Margins is the account's cash position, as reported by Zerodha.
type Margins struct {
	// Available is cash free to deploy.
	Available float64 `json:"available"`
	// Used is margin currently blocked by open positions and orders.
	Used float64 `json:"used"`
	// OpeningBalance is the balance at the start of the day.
	OpeningBalance float64 `json:"opening_balance"`

	UpdatedAt time.Time `json:"updated_at"`
	// Err records why the last refresh failed, so the UI can show a stale
	// figure honestly rather than a confident wrong one.
	Err string `json:"err,omitempty"`
}

// Known reports whether a figure has ever been fetched.
func (m Margins) Known() bool { return !m.UpdatedAt.IsZero() }

// Stale reports whether the figure is old enough to distrust.
func (m Margins) Stale() bool {
	return m.Known() && time.Since(m.UpdatedAt) > 3*marginRefreshInterval
}

// marginCache holds the most recent reading.
type marginCache struct {
	mu sync.RWMutex
	m  Margins
}

func (c *marginCache) get() Margins {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.m
}

func (c *marginCache) set(m Margins) {
	c.mu.Lock()
	c.m = m
	c.mu.Unlock()
}

// setErr records a failure without discarding the last good reading — a stale
// balance clearly marked stale is more useful than a blank.
func (c *marginCache) setErr(err error) {
	c.mu.Lock()
	c.m.Err = err.Error()
	c.mu.Unlock()
}

// Margins returns the cached account balance.
func (a *App) Margins() Margins { return a.margins.get() }

// RefreshMargins fetches the balance now.
//
// These are the REAL account figures from Zerodha in every mode. Paper trading
// simulates fills locally and never touches the account, so the balance will not
// move while paper trading — which is correct, and worth remembering before
// reading it as a measure of a paper session's performance.
func (a *App) RefreshMargins(ctx context.Context) {
	if !a.Kite.Snapshot().Connected() {
		return
	}
	raw, err := a.Kite.Client().GetMargins(ctx, marginSegment)
	if err != nil {
		a.margins.setErr(err)
		if a.Log != nil {
			a.Log.Debug("refresh margins failed", "err", err)
		}
		return
	}
	a.margins.set(toMargins(raw))

	// Feed the live policy: the 1% daily-loss cap is a percentage of the day's
	// OPENING balance, and this is where that number arrives.
	if a.LiveRisk != nil {
		m := a.margins.get()
		a.LiveRisk.ObserveBalance(m.OpeningBalance, time.Now())
		if lim := a.LiveRisk.MaxDailyLoss(); lim > 0 {
			cur := a.Risk.Limits()
			if cur.MaxDailyLoss != lim {
				cur.MaxDailyLoss = lim
				a.Risk.SetLimits(cur)
				if a.Log != nil {
					a.Log.Info("live daily-loss limit derived from opening balance",
						"policy", a.LiveRisk.Describe())
				}
			}
		}
	}
}

func toMargins(raw kite.Margin) Margins {
	available := raw.Available.LiveBalance
	if available == 0 {
		// Kite populates live_balance during market hours and cash outside it;
		// falling back keeps the figure meaningful after the close.
		available = raw.Available.Cash
	}
	return Margins{
		Available:      available,
		Used:           raw.Used.Debits,
		OpeningBalance: raw.Available.OpeningBalance,
		UpdatedAt:      time.Now(),
	}
}

// marginLoop keeps the balance fresh while a Zerodha session is up.
func (a *App) marginLoop(ctx context.Context) {
	t := time.NewTicker(marginRefreshInterval)
	defer t.Stop()

	// Fetch once as soon as a session appears, rather than waiting a full
	// interval to show anything at all.
	ready := time.NewTicker(2 * time.Second)
	defer ready.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ready.C:
			if a.Kite.Snapshot().Connected() && !a.Margins().Known() {
				a.RefreshMargins(ctx)
			}
		case <-t.C:
			a.RefreshMargins(ctx)
		}
	}
}
