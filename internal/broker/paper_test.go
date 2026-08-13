package broker

import (
	"context"
	"testing"
)

// TestPaperMarketFill checks that a market order fills at the current price and
// opens a short position.
func TestPaperMarketFill(t *testing.T) {
	var fills []Fill
	b := NewPaperBroker(func(f Fill) { fills = append(fills, f) }, nil)

	// Establish a market price, then sell 1 lot (75) at market.
	b.OnPrice("NIFTY24AUG24500CE", 120.0)
	o, err := b.PlaceOrder(context.Background(), OrderRequest{
		StrategyID:    "s1",
		Exchange:      "NFO",
		TradingSymbol: "NIFTY24AUG24500CE",
		Product:       ProductMIS,
		OrderType:     OrderTypeMarket,
		Side:          SideSell,
		Quantity:      75,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusComplete {
		t.Errorf("status = %v, want COMPLETE", o.Status)
	}
	if len(fills) != 1 || fills[0].Price != 120.0 {
		t.Errorf("fill = %+v, want price 120", fills)
	}
	// Position should be short 75 at 120.
	positions, _ := b.GetPositions(context.Background())
	if len(positions) != 1 || positions[0].NetQuantity != -75 || positions[0].AveragePrice != 120.0 {
		t.Errorf("position = %+v, want net -75 @ 120", positions)
	}
}

// TestPaperLimitFillCrossDown checks a sell LIMIT order fills only once price
// rises back to the limit.
func TestPaperLimitFillCrossUp(t *testing.T) {
	b := NewPaperBroker(nil, nil)
	// Sell limit at 130: fills only when price >= 130.
	o, _ := b.PlaceOrder(context.Background(), OrderRequest{
		TradingSymbol: "X", OrderType: OrderTypeLimit, Side: SideSell,
		Quantity: 10, Price: 130.0, Product: ProductMIS,
	})
	if o.Status == StatusComplete {
		t.Fatal("limit order should not fill before limit is reached")
	}
	b.OnPrice("X", 125.0) // below limit → no fill
	if got, _ := b.GetOpenOrders(context.Background()); len(got) != 1 {
		t.Fatal("expected 1 pending order at 125")
	}
	b.OnPrice("X", 130.0) // at limit → fill
	positions, _ := b.GetPositions(context.Background())
	if len(positions) != 1 || positions[0].NetQuantity != -10 || positions[0].AveragePrice != 130.0 {
		t.Errorf("position after limit fill = %+v, want net -10 @ 130", positions)
	}
}

// TestPaperRealizedPnL checks that closing a position realizes PnL correctly:
// short 100 @ 150, buy back 100 @ 140 → +1000 realized.
func TestPaperRealizedPnL(t *testing.T) {
	b := NewPaperBroker(nil, nil)
	b.OnPrice("X", 150.0)
	b.PlaceOrder(context.Background(), OrderRequest{
		TradingSymbol: "X", OrderType: OrderTypeMarket, Side: SideSell,
		Quantity: 100, Product: ProductMIS,
	}) // short 100 @ 150
	b.OnPrice("X", 140.0)
	b.PlaceOrder(context.Background(), OrderRequest{
		TradingSymbol: "X", OrderType: OrderTypeMarket, Side: SideBuy,
		Quantity: 100, Product: ProductMIS,
	}) // close @ 140

	positions, _ := b.GetPositions(context.Background())
	// Position should be flat (net 0) with +1000 realized PnL.
	if len(positions) == 0 {
		t.Fatal("expected a (flat) position row to record realized pnl")
	}
	p := positions[0]
	if p.NetQuantity != 0 {
		t.Errorf("net = %d, want 0 (flat)", p.NetQuantity)
	}
	if p.PnL != 1000.0 {
		t.Errorf("realized pnl = %v, want 1000", p.PnL)
	}
}

// TestPaperCancel checks cancel removes a pending order.
func TestPaperCancel(t *testing.T) {
	b := NewPaperBroker(nil, nil)
	o, _ := b.PlaceOrder(context.Background(), OrderRequest{
		TradingSymbol: "X", OrderType: OrderTypeLimit, Side: SideBuy,
		Quantity: 10, Price: 100.0, Product: ProductMIS,
	})
	if err := b.CancelOrder(context.Background(), o.ID); err != nil {
		t.Fatal(err)
	}
	open, _ := b.GetOpenOrders(context.Background())
	if len(open) != 0 {
		t.Errorf("expected no open orders after cancel, got %d", len(open))
	}
}
