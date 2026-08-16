package app

import (
	"context"
	"strings"
	"sync"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/history"
	"kite-algo/internal/options"
)

// Expiry-day square-off for the REAL book.
//
// An option position that looked small at noon on expiry day can be very large
// by the close: gamma rises without bound as time to expiry goes to zero, so
// the delta of a near-the-money contract swings between 0 and 1 on moves that
// would barely have registered a week earlier. Holding one into the close is a
// different trade from the one that was put on.
//
// This flattens them at a configured time. It touches the REAL book only —
// simulated positions are the point of a simulation and are left to run so the
// strategy under evaluation is measured on what it actually did.

// expirySweeper flattens expiring real positions once per day.
type expirySweeper struct {
	app   *App
	runAt time.Duration

	mu   sync.Mutex
	done string // IST date the sweep last completed on
}

// startExpirySweeper launches the sweeper unless it is disabled.
func (a *App) startExpirySweeper(ctx context.Context) {
	raw := strings.TrimSpace(a.Cfg.Risk.Live.ExpirySquareOffTime)
	if raw == "" || strings.EqualFold(raw, "off") {
		if a.Log != nil {
			a.Log.Warn("expiry-day square-off is DISABLED; real positions in " +
				"contracts expiring today will be held into the close")
		}
		return
	}
	t, err := time.Parse("15:04", raw)
	if err != nil {
		if a.Log != nil {
			a.Log.Error("expiry_square_off_time is not HH:MM; expiry square-off disabled",
				"value", raw, "err", err)
		}
		return
	}
	s := &expirySweeper{
		app:   a,
		runAt: time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute,
	}
	if a.Log != nil {
		a.Log.Info("expiry-day square-off scheduled", "run_at", raw+" IST", "book", "real")
	}
	go s.run(ctx)
}

// run ticks each minute, the same reason the capture scheduler does: a laptop
// resuming from suspend or a clock correction converges within a minute rather
// than missing a timer set against the old wall clock.
func (s *expirySweeper) run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.tick(ctx, now)
		}
	}
}

func (s *expirySweeper) tick(ctx context.Context, now time.Time) {
	local := now.In(history.IST)
	today := local.Format("2006-01-02")

	since := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute
	if since < s.runAt {
		return
	}
	s.mu.Lock()
	if s.done == today {
		s.mu.Unlock()
		return
	}
	s.done = today
	s.mu.Unlock()

	s.sweep(ctx, local)
}

// sweep flattens every real position expiring today.
func (s *expirySweeper) sweep(ctx context.Context, now time.Time) {
	expiring := expiringToday(s.app.Engine.Positions(), now)
	if len(expiring) == 0 {
		return
	}

	if s.app.Log != nil {
		s.app.Log.Warn("expiry-day square-off: flattening real positions expiring today",
			"count", len(expiring), "at", now.Format("15:04"))
	}

	// Shorts first. Selling the long leg of a spread before covering the short
	// leaves the book naked short with a margin spike, and the second order can
	// then be rejected outright — see engine.liquidationOrder, which sequences
	// this. Squaring off one symbol at a time here would lose that ordering, so
	// the positions are handed over pre-sorted.
	for _, p := range s.app.Engine.LiquidationOrder(expiring) {
		o, err := s.app.Engine.SquareOff(ctx, p.StrategyID, p.TradingSymbol)
		if err != nil {
			if s.app.Log != nil {
				s.app.Log.Error("expiry square-off failed", "symbol", p.TradingSymbol, "err", err)
			}
			continue
		}
		if s.app.Log != nil {
			s.app.Log.Warn("expiry square-off placed",
				"symbol", p.TradingSymbol, "side", o.Side, "qty", o.Quantity)
		}
	}
}

// expiringToday returns the REAL open positions whose contract expires today.
//
// The expiry comes from the trading symbol rather than the instrument master:
// the master is reloaded on every login and a contract expiring today is
// exactly the one most likely to have just left it, which would make the
// position unreadable on the day it matters most.
func expiringToday(positions []broker.Position, now time.Time) []broker.Position {
	today := now.In(history.IST).Format("2006-01-02")

	var out []broker.Position
	for _, p := range positions {
		if !p.IsOpen() || !p.Book.IsReal() {
			continue
		}
		spec, ok := options.ParseSymbol(p.TradingSymbol)
		if !ok || spec.Expiry.IsZero() {
			continue
		}
		if spec.Expiry.In(history.IST).Format("2006-01-02") == today {
			out = append(out, p)
		}
	}
	return out
}
