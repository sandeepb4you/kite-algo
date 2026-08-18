package engine

import (
	"context"
	"testing"

	"kite-algo/internal/broker"
	"kite-algo/internal/risk"
)

// positionsBroker is a broker that reports a fixed position book.
type positionsBroker struct {
	mode      string
	positions []broker.Position
}

func (b *positionsBroker) PlaceOrder(context.Context, broker.OrderRequest) (*broker.Order, error) {
	return &broker.Order{Status: broker.StatusComplete, Mode: b.mode}, nil
}
func (b *positionsBroker) ModifyOrder(context.Context, string, broker.ModifyRequest) error {
	return nil
}
func (b *positionsBroker) CancelOrder(context.Context, string) error             { return nil }
func (b *positionsBroker) GetOpenOrders(context.Context) ([]broker.Order, error) { return nil, nil }
func (b *positionsBroker) Mode() string                                          { return b.mode }
func (b *positionsBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return b.positions, nil
}

// A position closed during the day keeps its realized P&L in the day figure.
//
// This is the number risk.Check compares against the daily-loss limit, and with
// the sizing caps switched off it is the only guardrail on the real book. When
// the live broker dropped flat rows, closing a losing trade erased that loss
// from the total, so a day of round trips reported a P&L far better than it had
// and the limit never tripped.
func TestDayPnLKeepsRealizedLossFromClosedPosition(t *testing.T) {
	paper := broker.NewPaperBroker(nil, nil)
	eng := New(paper, nullStore{}, risk.NewManager(risk.Limits{}), false, nil)

	// Two real positions: one still open, one closed at a 4,000 loss.
	eng.SetLiveBroker(&positionsBroker{mode: "live", positions: []broker.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY2681824350CE", Product: broker.ProductMIS,
		NetQuantity: -75, AveragePrice: 120, LastPrice: 120, PnL: 0,
	}, {
		Exchange: "NFO", TradingSymbol: "NIFTY2681824350PE", Product: broker.ProductMIS,
		NetQuantity: 0, AveragePrice: 0, LastPrice: 95, PnL: -4000,
	}}})

	eng.refreshPositions(context.Background())

	if got := eng.BookPnL(broker.BookReal); got != -4000 {
		t.Errorf("real book day PnL = %v, want -4000 (the closed position's realized loss)", got)
	}
	if got := eng.DayPnL(); got != -4000 {
		t.Errorf("DayPnL() = %v, want -4000", got)
	}

	// The flat row must not read as exposure, or it would consume the
	// max-open-positions allowance and be handed to a square-off.
	if got := eng.snapshotBookPositionCount(broker.BookReal); got != 1 {
		t.Errorf("open real positions = %d, want 1 (the flat row is not exposure)", got)
	}
	if eng.hasPosition("NIFTY2681824350PE") {
		t.Error("hasPosition() true for a flat position")
	}
	if !eng.hasPosition("NIFTY2681824350CE") {
		t.Error("hasPosition() false for the genuinely open position")
	}
	if got := liquidationOrder(eng.Positions()); len(got) != 1 {
		t.Errorf("liquidationOrder returned %d positions, want 1 — a flat row must never be squared off", len(got))
	}
}

// Marking to market must not disturb a closed position's realized P&L, however
// far the price of that contract moves afterwards.
func TestMarkToMarketLeavesClosedPositionAlone(t *testing.T) {
	paper := broker.NewPaperBroker(nil, nil)
	eng := New(paper, nullStore{}, risk.NewManager(risk.Limits{}), false, nil)

	eng.SetLiveBroker(&positionsBroker{mode: "live", positions: []broker.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY2681824350PE", Product: broker.ProductMIS,
		NetQuantity: 0, AveragePrice: 0, LastPrice: 95, PnL: -4000,
	}}})
	eng.refreshPositions(context.Background())

	// A later tick on that symbol, well away from where it was closed.
	eng.mu.Lock()
	eng.prices["NIFTY2681824350PE"] = 300
	eng.mu.Unlock()
	eng.markPositionsToMarket(true)

	if got := eng.DayPnL(); got != -4000 {
		t.Errorf("DayPnL() = %v after a tick, want -4000 — a closed position has no unrealized P&L", got)
	}
}
