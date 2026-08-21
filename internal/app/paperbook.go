package app

import (
	"context"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/history"
)

// The paper broker keeps its book in memory, so a restart emptied it.
//
// The rows were never lost — positions are persisted on every fill — they
// simply stopped being read back, and nothing in the codebase called
// storage.GetOpenPositions at all. From the operator's side that is
// indistinguishable from data loss: the trades were there before lunch and gone
// after a redeploy.
//
// Only the SIMULATED book is restored here. Real positions come from Zerodha,
// which is the authority on them; seeding those from our own last-known state
// would invent a position the exchange might have closed while we were down.

// restorePaperBook reloads simulated positions from storage at startup.
func (a *App) restorePaperBook(ctx context.Context) {
	if a.Store == nil || a.paper == nil {
		return
	}
	stored, err := a.Store.GetOpenPositions(ctx)
	if err != nil {
		if a.Log != nil {
			a.Log.Warn("could not restore the paper book; open simulated positions "+
				"will not appear until they next trade", "err", err)
		}
		return
	}

	keep, skipped := restorablePaperPositions(stored, time.Now())
	if len(keep) == 0 && skipped == 0 {
		return
	}

	n := a.paper.RestorePositions(keep)
	if a.Log != nil && (n > 0 || skipped > 0) {
		a.Log.Info("paper book restored from storage",
			"positions", n, "skipped_stale_intraday", skipped)
	}
}

// startPaperBookRollover drops yesterday's paper rows once the IST date turns.
//
// restorePaperBook applies the same rule, but only at startup, so it fixed the
// problem only for a process that happened to restart overnight. The server
// normally does not: it runs for days, the paper broker holds its book in
// memory, and every position it has ever seen is still in that map the next
// morning. So the positions tab opened with yesterday's closed trades listed
// above today's, and the operator scanned a screen of finished business looking
// for what they were actually holding.
//
// A minute ticker rather than a timer set for midnight, for the same reason the
// capture scheduler uses one: a machine that was suspended over the rollover, or
// whose clock was corrected, converges within a minute instead of waiting for a
// deadline that has already passed.
func (a *App) startPaperBookRollover(ctx context.Context) {
	if a.paper == nil {
		return
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				a.pruneStalePaperPositions(now)
			}
		}
	}()
}

// pruneStalePaperPositions forgets simulated rows left over from a previous day.
//
// Idempotent, so running it every minute is harmless: after the first pass there
// is nothing older than the current IST day left to drop. Today's flat rows are
// deliberately kept — they carry today's realised P&L, which the day's figures
// are built from.
func (a *App) pruneStalePaperPositions(now time.Time) {
	if a.paper == nil {
		return
	}
	local := now.In(history.IST)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, history.IST)

	if n := a.paper.DropStalePositions(dayStart); n > 0 && a.Log != nil {
		a.Log.Info("paper book rolled over; dropped positions from previous days",
			"dropped", n, "day", local.Format("2006-01-02"))
	}
}

// restorablePaperPositions selects which stored positions to seed.
//
// Intraday (MIS) positions from a previous day are dropped. The exchange closes
// them out at the end of their session, so resurrecting one the next morning
// would display a position that does not exist anywhere and would let the
// operator "square off" something already gone. Overnight products (NRML, CNC)
// are meant to survive the close and are restored whatever their age.
func restorablePaperPositions(in []broker.Position, now time.Time) (keep []broker.Position, skipped int) {
	today := now.In(history.IST).Format("2006-01-02")

	for _, p := range in {
		if p.NetQuantity == 0 || p.Book.IsReal() {
			continue
		}
		if p.Product == broker.ProductMIS &&
			p.Updated.In(history.IST).Format("2006-01-02") != today {
			skipped++
			continue
		}
		keep = append(keep, p)
	}
	return keep, skipped
}
