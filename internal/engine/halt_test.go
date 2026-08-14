package engine

import (
	"context"
	"errors"
	"testing"

	"kite-algo/internal/broker"
	"kite-algo/internal/risk"
)

// TestHaltBlocksNewOrders is the kill switch's core promise. Trader is the only
// route a strategy has to the market, so blocking PlaceOrder blocks everything.
func TestHaltBlocksNewOrders(t *testing.T) {
	e, tick := tradingEngine(t, risk.Limits{MaxLotsPerTrade: 10})
	ctx := context.Background()
	tick("NIFTY24AUG24500CE", 100)

	if _, err := e.Halt(ctx, HaltOptions{Reason: "test", By: "operator"}); err != nil {
		t.Fatalf("Halt reported errors: %v", err)
	}
	if !e.IsHalted() {
		t.Fatal("engine does not report itself halted")
	}

	_, err := e.PlaceOrder(ctx, broker.OrderRequest{
		StrategyID: "manual", Exchange: "NFO", TradingSymbol: "NIFTY24AUG24500CE",
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideBuy, Quantity: 75,
	})
	var re *risk.RiskError
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want a *risk.RiskError so the UI renders it uniformly", err)
	}
	if re.Rule != "kill-switch" {
		t.Errorf("rule = %q, want kill-switch", re.Rule)
	}
}

// TestHaltStillAllowsSquareOff is the counterpart. Halting exists to stop new
// risk; if it also blocked the flatten, the operator would be frozen holding a
// position they explicitly asked to close.
func TestHaltStillAllowsSquareOff(t *testing.T) {
	e, tick := tradingEngine(t, risk.Limits{MaxLotsPerTrade: 10})
	ctx := context.Background()

	tick("NIFTY24AUG24500CE", 100)
	if _, err := e.PlaceOrder(ctx, broker.OrderRequest{
		StrategyID: "manual", Exchange: "NFO", TradingSymbol: "NIFTY24AUG24500CE",
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideBuy, Quantity: 75,
	}); err != nil {
		t.Fatalf("opening order: %v", err)
	}
	e.refreshPositions(ctx)

	if _, errs := e.Halt(ctx, HaltOptions{Reason: "test", By: "operator"}); len(errs) > 0 {
		t.Fatalf("Halt: %v", errs)
	}

	if _, err := e.SquareOff(ctx, "manual", "NIFTY24AUG24500CE"); err != nil {
		t.Fatalf("SQUARE-OFF BLOCKED BY THE KILL SWITCH: %v\n"+
			"halting must stop new risk, not trap the operator in an open position", err)
	}
}

// TestPanicHaltFlattensEverything covers the "halt and flatten" button.
func TestPanicHaltFlattensEverything(t *testing.T) {
	e, tick := tradingEngine(t, risk.Limits{MaxLotsPerTrade: 10})
	ctx := context.Background()

	for _, sym := range []string{"NIFTY24AUG24500CE", "NIFTY24AUG24500PE"} {
		tick(sym, 100)
		if _, err := e.PlaceOrder(ctx, broker.OrderRequest{
			StrategyID: "manual", Exchange: "NFO", TradingSymbol: sym,
			Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
			Side: broker.SideSell, Quantity: 75,
		}); err != nil {
			t.Fatalf("open %s: %v", sym, err)
		}
	}
	e.refreshPositions(ctx)

	state, errs := e.Halt(ctx, HaltOptions{
		Reason: "panic", By: "operator", StopStrategies: true, SquareOffAll: true,
	})
	if len(errs) > 0 {
		t.Fatalf("panic halt reported errors: %v", errs)
	}
	if !state.SquaredOff {
		t.Error("halt did not report the book as squared off")
	}

	e.refreshPositions(ctx)
	for _, p := range e.Positions() {
		if p.IsOpen() {
			t.Errorf("%s still open with qty %d after a panic halt",
				p.TradingSymbol, p.NetQuantity)
		}
	}
}

func TestResumeLiftsTheHalt(t *testing.T) {
	e, tick := tradingEngine(t, risk.Limits{MaxLotsPerTrade: 10})
	ctx := context.Background()
	tick("NIFTY24AUG24500CE", 100)

	e.Halt(ctx, HaltOptions{Reason: "test", By: "operator"})
	e.Resume("operator")

	if e.IsHalted() {
		t.Fatal("still halted after Resume")
	}
	if _, err := e.PlaceOrder(ctx, broker.OrderRequest{
		StrategyID: "manual", Exchange: "NFO", TradingSymbol: "NIFTY24AUG24500CE",
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideBuy, Quantity: 75,
	}); err != nil {
		t.Errorf("order rejected after resume: %v", err)
	}
}

// TestHaltStopsStrategies confirms the kill switch does not merely block orders
// but takes the strategies out of the loop entirely.
func TestHaltStopsStrategies(t *testing.T) {
	e, _, built := lifecycleEngine(t)
	ctx := context.Background()

	if _, err := e.StartStrategy(ctx, StrategySpec{Type: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, errs := e.Halt(ctx, HaltOptions{
		Reason: "test", By: "operator", StopStrategies: true,
	}); len(errs) > 0 {
		t.Fatalf("Halt: %v", errs)
	}

	st, _ := e.StrategyStatusByID("fake")
	if st.State != StateStopped {
		t.Errorf("strategy state = %s, want stopped", st.State)
	}
	if !built["fake"].stopped.Load() {
		t.Error("Stop was not called on the strategy")
	}
	if n := len(e.activeStrategies()); n != 0 {
		t.Errorf("%d strategies still in the fan-out after a halt", n)
	}
}
