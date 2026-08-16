package app

import (
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/history"
)

func storedPos(sym string, qty int, product broker.ProductType, book broker.Book, updated time.Time) broker.Position {
	return broker.Position{
		StrategyID: "manual", TradingSymbol: sym, Product: product,
		NetQuantity: qty, AveragePrice: 100, Book: book, Updated: updated,
	}
}

// The bug: a restart emptied the in-memory paper book while the rows sat in
// storage, which reads as "my trades vanished".
func TestPaperPositionsAreRestored(t *testing.T) {
	now := time.Date(2026, 8, 17, 11, 0, 0, 0, history.IST)
	in := []broker.Position{
		storedPos("NIFTY2682524350CE", -65, broker.ProductMIS, broker.BookPaper, now),
		storedPos("NIFTY2682524350PE", -65, broker.ProductNRML, broker.BookPaper, now),
	}

	keep, skipped := restorablePaperPositions(in, now)

	if len(keep) != 2 {
		t.Fatalf("restored %d positions, want 2", len(keep))
	}
	if skipped != 0 {
		t.Errorf("skipped %d, want 0", skipped)
	}
}

// Real positions come from Zerodha, which is the authority. Seeding them from
// our own last-known state would invent a position the exchange may have closed
// while we were down.
func TestRealPositionsAreNotRestoredIntoThePaperBroker(t *testing.T) {
	now := time.Date(2026, 8, 17, 11, 0, 0, 0, history.IST)
	in := []broker.Position{
		storedPos("NIFTY2682524350CE", -65, broker.ProductMIS, broker.BookReal, now),
	}

	keep, _ := restorablePaperPositions(in, now)

	if len(keep) != 0 {
		t.Errorf("restored %d real positions into the paper broker, want 0", len(keep))
	}
}

// An intraday position from a previous day does not exist anywhere: the
// exchange closed it at the bell. Showing it would let the operator try to
// square off something already gone.
func TestStaleIntradayPositionsAreDropped(t *testing.T) {
	now := time.Date(2026, 8, 17, 11, 0, 0, 0, history.IST)
	yesterday := now.AddDate(0, 0, -1)

	in := []broker.Position{
		storedPos("OLD-MIS", -65, broker.ProductMIS, broker.BookPaper, yesterday),
		storedPos("OLD-NRML", -65, broker.ProductNRML, broker.BookPaper, yesterday),
		storedPos("TODAY-MIS", -65, broker.ProductMIS, broker.BookPaper, now),
	}

	keep, skipped := restorablePaperPositions(in, now)

	if skipped != 1 {
		t.Errorf("skipped %d, want 1 (the stale MIS)", skipped)
	}
	got := map[string]bool{}
	for _, p := range keep {
		got[p.TradingSymbol] = true
	}
	if got["OLD-MIS"] {
		t.Error("resurrected an intraday position from a previous day")
	}
	if !got["OLD-NRML"] {
		t.Error("dropped an overnight position, which is meant to survive the close")
	}
	if !got["TODAY-MIS"] {
		t.Error("dropped today's intraday position")
	}
}

func TestFlatPositionsAreNotRestored(t *testing.T) {
	now := time.Date(2026, 8, 17, 11, 0, 0, 0, history.IST)
	in := []broker.Position{storedPos("FLAT", 0, broker.ProductMIS, broker.BookPaper, now)}

	if keep, _ := restorablePaperPositions(in, now); len(keep) != 0 {
		t.Errorf("restored %d flat positions, want 0", len(keep))
	}
}

// The broker must take the rows on, and must not clobber anything already live.
func TestRestorePositionsSeedsTheBroker(t *testing.T) {
	now := time.Date(2026, 8, 17, 11, 0, 0, 0, history.IST)
	pb := broker.NewPaperBroker(nil, nil)

	n := pb.RestorePositions([]broker.Position{
		storedPos("NIFTY2682524350CE", -65, broker.ProductMIS, broker.BookPaper, now),
	})
	if n != 1 {
		t.Fatalf("RestorePositions returned %d, want 1", n)
	}

	got, err := pb.GetPositions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].NetQuantity != -65 {
		t.Fatalf("broker holds %+v", got)
	}
	if got[0].AveragePrice != 100 {
		t.Errorf("average price = %v, want the stored 100 — P&L would be wrong",
			got[0].AveragePrice)
	}

	// A second restore must not overwrite: by then the engine may have traded.
	again := pb.RestorePositions([]broker.Position{
		storedPos("NIFTY2682524350CE", -130, broker.ProductMIS, broker.BookPaper, now),
	})
	if again != 0 {
		t.Errorf("restored over a live position (%d)", again)
	}
	got, _ = pb.GetPositions(nil)
	if got[0].NetQuantity != -65 {
		t.Errorf("live position clobbered: qty = %d", got[0].NetQuantity)
	}
}
