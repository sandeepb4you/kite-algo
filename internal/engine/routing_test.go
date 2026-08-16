package engine

import (
	"context"
	"testing"

	"kite-algo/internal/broker"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
)

// recordingBroker counts what was routed to it.
type recordingBroker struct {
	mode   string
	placed []broker.OrderRequest
	closed []string
}

func (b *recordingBroker) PlaceOrder(_ context.Context, req broker.OrderRequest) (*broker.Order, error) {
	b.placed = append(b.placed, req)
	return &broker.Order{
		ID: b.mode + "-" + req.TradingSymbol, StrategyID: req.StrategyID,
		TradingSymbol: req.TradingSymbol, Side: req.Side, Quantity: req.Quantity,
		Status: broker.StatusComplete, Mode: b.mode,
	}, nil
}
func (b *recordingBroker) ModifyOrder(context.Context, string, broker.ModifyRequest) error {
	return nil
}
func (b *recordingBroker) CancelOrder(_ context.Context, id string) error {
	b.closed = append(b.closed, id)
	return nil
}
func (b *recordingBroker) GetOpenOrders(context.Context) ([]broker.Order, error) { return nil, nil }
func (b *recordingBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return nil, nil
}
func (b *recordingBroker) Mode() string { return b.mode }

func routingEngine(t *testing.T) (*Engine, *broker.PaperBroker, *recordingBroker) {
	t.Helper()
	paper := broker.NewPaperBroker(nil, nil)
	live := &recordingBroker{mode: "live"}
	eng := New(paper, nullStore{}, risk.NewManager(risk.Limits{}), false, nil)
	return eng, paper, live
}

func req(strategyID, symbol string) broker.OrderRequest {
	return broker.OrderRequest{
		StrategyID: strategyID, Exchange: "NFO", TradingSymbol: symbol,
		Product: broker.ProductMIS, OrderType: broker.OrderTypeMarket,
		Side: broker.SideSell, Quantity: 65, Validity: broker.ValidityDay,
	}
}

// wantReal is req plus the explicit opt-in to the real book.
func wantReal(strategyID, symbol string) broker.OrderRequest {
	r := req(strategyID, symbol)
	r.Book = broker.BookReal
	return r
}

// With no live broker installed, nothing can reach the exchange.
func TestRoutingDefaultsEverythingToPaper(t *testing.T) {
	eng, _, _ := routingEngine(t)

	if got := eng.RouteMode(); got != RouteAllPaper {
		t.Errorf("RouteMode() = %q, want %q", got, RouteAllPaper)
	}
	if eng.LiveManualActive() {
		t.Error("live manual reported active with no live broker")
	}
	for _, id := range []string{ManualStrategyID, "short-straddle"} {
		if got := eng.bookFor(req(id, "X")); got != broker.BookPaper {
			t.Errorf("bookFor(%s) = %q, want paper", id, got)
		}
	}
}

// The whole point: manual goes live, strategies do not.
func TestRoutingSendsOnlyManualOrdersLive(t *testing.T) {
	eng, _, live := routingEngine(t)
	eng.SetLiveBroker(live)

	if got := eng.RouteMode(); got != RouteManualLive {
		t.Errorf("RouteMode() = %q, want %q", got, RouteManualLive)
	}
	if got := eng.bookFor(wantReal(ManualStrategyID, "X")); got != broker.BookReal {
		t.Errorf("manual order that asked for the real book routed to %q, want real", got)
	}
	// A strategy cannot reach the exchange even by asking for it explicitly.
	for _, id := range []string{"short-straddle", "short-straddle-2", "", "anything"} {
		if got := eng.bookFor(wantReal(id, "X")); got != broker.BookPaper {
			t.Errorf("strategy %q routed to %q — a strategy must never reach the exchange, "+
				"even when the request asks for the real book", id, got)
		}
	}
}

// End to end through PlaceOrder: the live broker must see the manual order and
// nothing else.
func TestPlaceOrderRoutesByBook(t *testing.T) {
	ctx := context.Background()
	eng, paper, live := routingEngine(t)
	eng.SetLiveBroker(live)
	paper.OnPrice("NIFTY24350CE", 100)

	if _, err := eng.PlaceOrder(ctx, wantReal(ManualStrategyID, "NIFTY24350CE")); err != nil {
		t.Fatalf("manual order: %v", err)
	}
	if _, err := eng.PlaceOrder(ctx, req("short-straddle", "NIFTY24350CE")); err != nil {
		t.Fatalf("strategy order: %v", err)
	}

	if len(live.placed) != 1 {
		t.Fatalf("live broker got %d orders, want exactly 1", len(live.placed))
	}
	if live.placed[0].StrategyID != ManualStrategyID {
		t.Errorf("live broker received a %q order — only manual may go live",
			live.placed[0].StrategyID)
	}
}

// A cancel must reach the broker that actually holds the order. Sent to the
// wrong one it succeeds against nothing and leaves a real working order at the
// exchange that the UI reports as cancelled.
func TestCancelRoutesToTheOwningBroker(t *testing.T) {
	ctx := context.Background()
	eng, paper, live := routingEngine(t)
	eng.SetLiveBroker(live)
	paper.OnPrice("NIFTY24350CE", 100)

	o, err := eng.PlaceOrder(ctx, wantReal(ManualStrategyID, "NIFTY24350CE"))
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if err := eng.CancelOrder(ctx, o.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(live.closed) != 1 || live.closed[0] != o.ID {
		t.Errorf("live cancels = %v, want [%s]", live.closed, o.ID)
	}
}

// Removing the live broker must return manual orders to simulation, not error.
func TestDisarmReturnsManualToPaper(t *testing.T) {
	eng, _, live := routingEngine(t)
	eng.SetLiveBroker(live)
	eng.SetLiveBroker(nil)

	if eng.LiveManualActive() {
		t.Error("still live after disarm")
	}
	if got := eng.bookFor(wantReal(ManualStrategyID, "X")); got != broker.BookPaper {
		t.Errorf("manual routed to %q after disarm, want paper", got)
	}
}

// If the live broker vanishes mid-session the manual order must be simulated,
// never silently dropped or routed somewhere unexpected. A missed trade is
// recoverable; an unauthorised real one is not.
func TestManualFallsBackToPaperWhenLiveIsAbsent(t *testing.T) {
	eng, _, _ := routingEngine(t)
	b, book := eng.brokerFor(wantReal(ManualStrategyID, "X"))
	if book != broker.BookPaper {
		t.Errorf("book = %q, want paper", book)
	}
	if b == nil {
		t.Fatal("no broker returned")
	}
	if b.Mode() == "live" {
		t.Error("routed live with no live broker installed")
	}
}

// Risk is evaluated per book: the two hold different money.
func TestRiskManagerIsSelectedPerBook(t *testing.T) {
	eng, _, live := routingEngine(t)
	eng.SetLiveBroker(live)

	paperRisk := risk.NewManager(risk.Limits{MaxLotsPerTrade: 99})
	eng.SetPaperRisk(paperRisk)

	if got := eng.riskFor(broker.BookPaper); got != paperRisk {
		t.Error("paper book did not use the paper risk manager")
	}
	if got := eng.riskFor(broker.BookReal); got == paperRisk {
		t.Error("real book used the PAPER risk manager — real capital would be " +
			"governed by the simulated limits")
	}
}

// Falling back to the real limits when no paper manager is set is the safe
// direction: an unset paper manager must not mean "no limits".
func TestPaperRiskFallsBackToRealLimits(t *testing.T) {
	eng, _, _ := routingEngine(t)
	if got := eng.riskFor(broker.BookPaper); got != eng.risk {
		t.Error("paper book had no limits at all when no paper manager was set")
	}
}

var _ storage.Store = nullStore{}

// The point of the third condition: with live armed, a manual order that does
// NOT ask for the real book stays simulated.
//
// This is what lets an operator keep testing by hand on the same screen while
// strategies are evaluated on paper. It also means the failure mode of
// forgetting to choose is a simulated order, never a real one.
func TestManualOrderStaysPaperUnlessItAsksForReal(t *testing.T) {
	ctx := context.Background()
	eng, paper, live := routingEngine(t)
	eng.SetLiveBroker(live)
	paper.OnPrice("NIFTY24350CE", 100)

	// Book unset — the zero value.
	if got := eng.bookFor(req(ManualStrategyID, "X")); got != broker.BookPaper {
		t.Errorf("manual order with no book routed to %q, want paper", got)
	}
	// Explicitly paper.
	r := req(ManualStrategyID, "X")
	r.Book = broker.BookPaper
	if got := eng.bookFor(r); got != broker.BookPaper {
		t.Errorf("manual order asking for paper routed to %q", got)
	}

	if _, err := eng.PlaceOrder(ctx, req(ManualStrategyID, "NIFTY24350CE")); err != nil {
		t.Fatalf("place: %v", err)
	}
	if len(live.placed) != 0 {
		t.Errorf("live broker received %d orders from a manual order that never "+
			"asked for the real book", len(live.placed))
	}
}

// A liquidation must cover SHORT legs before selling long ones.
//
// Selling the long leg of a spread first removes the hedge: for the moment
// between the two orders the book is naked short, margin spikes, and the broker
// can reject the second leg — leaving the operator holding the exact position
// they were trying to escape.
func TestLiquidationCoversShortsFirst(t *testing.T) {
	in := []broker.Position{
		{TradingSymbol: "LONG-A", NetQuantity: 65},
		{TradingSymbol: "SHORT-A", NetQuantity: -65},
		{TradingSymbol: "FLAT", NetQuantity: 0},
		{TradingSymbol: "LONG-B", NetQuantity: 130},
		{TradingSymbol: "SHORT-B", NetQuantity: -130},
	}

	got := liquidationOrder(in)

	if len(got) != 4 {
		t.Fatalf("got %d positions, want 4 (the flat one dropped)", len(got))
	}
	// Every short must precede every long.
	seenLong := false
	for _, p := range got {
		if p.IsLong() {
			seenLong = true
			continue
		}
		if seenLong {
			t.Errorf("short %s sequenced after a long — the hedge would be released first",
				p.TradingSymbol)
		}
	}
	// Stable within each leg, so a liquidation is reproducible.
	if got[0].TradingSymbol != "SHORT-A" || got[1].TradingSymbol != "SHORT-B" {
		t.Errorf("shorts reordered: %s, %s", got[0].TradingSymbol, got[1].TradingSymbol)
	}
	if got[2].TradingSymbol != "LONG-A" || got[3].TradingSymbol != "LONG-B" {
		t.Errorf("longs reordered: %s, %s", got[2].TradingSymbol, got[3].TradingSymbol)
	}
}
