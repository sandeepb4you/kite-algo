package app

import (
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/history"
)

func pos(sym string, qty int, book broker.Book) broker.Position {
	return broker.Position{TradingSymbol: sym, NetQuantity: qty, Book: book}
}

// Only REAL positions expiring TODAY are swept.
func TestExpiringTodaySelectsOnlyRealSameDayExpiry(t *testing.T) {
	// NIFTY2681824350CE expires 2026-08-18.
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, history.IST)

	in := []broker.Position{
		pos("NIFTY2681824350CE", -65, broker.BookReal),  // expires today, real
		pos("NIFTY2681824350PE", -65, broker.BookPaper), // expires today, simulated
		pos("NIFTY2682524350CE", -65, broker.BookReal),  // next week, real
		pos("NIFTY2681824400CE", 0, broker.BookReal),    // flat
	}

	got := expiringToday(in, now)

	if len(got) != 1 {
		t.Fatalf("got %d positions, want 1: %+v", len(got), got)
	}
	if got[0].TradingSymbol != "NIFTY2681824350CE" {
		t.Errorf("selected %s", got[0].TradingSymbol)
	}
}

// Simulated positions are the point of a simulation and must be left to run,
// or the strategy under evaluation is measured on something it did not do.
func TestExpirySweepLeavesPaperPositionsAlone(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, history.IST)
	in := []broker.Position{pos("NIFTY2681824350CE", -65, broker.BookPaper)}

	if got := expiringToday(in, now); len(got) != 0 {
		t.Errorf("swept %d simulated positions, want 0", len(got))
	}
}

// The day before expiry, nothing is swept.
func TestExpiringTodayIgnoresFutureExpiries(t *testing.T) {
	now := time.Date(2026, 8, 17, 15, 0, 0, 0, history.IST)
	in := []broker.Position{pos("NIFTY2681824350CE", -65, broker.BookReal)}

	if got := expiringToday(in, now); len(got) != 0 {
		t.Errorf("swept %d positions the day before expiry, want 0", len(got))
	}
}

// An unparseable symbol must not be swept on a guess.
func TestExpiringTodaySkipsUnparseableSymbols(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 0, 0, 0, history.IST)
	in := []broker.Position{pos("SOMETHING-ODD", -65, broker.BookReal)}

	if got := expiringToday(in, now); len(got) != 0 {
		t.Errorf("swept %d unparseable symbols, want 0", len(got))
	}
}
