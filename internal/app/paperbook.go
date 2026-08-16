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
