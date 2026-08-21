package engine

import (
	"context"
	"errors"
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
	// positions is what this broker reports holding, so a test can give the
	// engine a real position to find.
	positions []broker.Position
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
	return b.positions, nil
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

	// bookFor is where the fallback happens, and it must happen there: a NEW
	// order with nowhere real to go is simulated rather than refused.
	book := eng.bookFor(wantReal(ManualStrategyID, "X"))
	if book != broker.BookPaper {
		t.Errorf("book = %q, want paper", book)
	}
	b, err := eng.brokerForBook(book)
	if err != nil {
		t.Fatalf("brokerForBook(paper): %v", err)
	}
	if b == nil {
		t.Fatal("no broker returned")
	}
	if b.Mode() == "live" {
		t.Error("routed live with no live broker installed")
	}
}

// Asking for the REAL book with no live broker must fail, not fall back.
//
// The opposite direction from the test above, and deliberately so. A new order
// with nowhere real to go should be simulated; a request to CLOSE a real position
// must never be, because the paper broker cannot close a position it does not
// hold — it would open an unrelated one and report success while the real
// exposure stayed on.
func TestRealBookRefusesWhenLiveIsAbsent(t *testing.T) {
	eng, _, _ := routingEngine(t)

	b, err := eng.brokerForBook(broker.BookReal)
	if !errors.Is(err, ErrNoLiveBroker) {
		t.Errorf("err = %v, want ErrNoLiveBroker", err)
	}
	if b != nil {
		t.Errorf("a broker was returned for the real book with none installed: %v", b.Mode())
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

// Closing a REAL position must reach the live broker.
//
// This is the bug that left a real position open. flatten synthesises the closing
// order, and bookFor derives the book from the REQUEST — but a synthesised close
// carries neither thing bookFor looks for: positions reconciled from Kite have no
// StrategyID, and nothing sets Book on an order the operator never typed. So the
// close looked exactly like a strategy order, went to the paper broker, opened a
// phantom paper position facing the other way, and reported success while the real
// exposure stayed on. On the panic button and the expiry-day sweep, both of which
// close real positions, that is the whole safety mechanism doing nothing.
func TestSquaringOffARealPositionReachesTheLiveBroker(t *testing.T) {
	ctx := context.Background()
	eng, paper, live := routingEngine(t)
	eng.SetLiveBroker(live)

	// A real short, as refreshPositions would have tagged it.
	eng.mu.Lock()
	eng.positions = []broker.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY24350CE", Product: broker.ProductMIS,
		NetQuantity: -75, AveragePrice: 120, Book: broker.BookReal,
	}}
	eng.mu.Unlock()

	if _, err := eng.SquareOff(ctx, "", "NIFTY24350CE"); err != nil {
		t.Fatalf("SquareOff: %v", err)
	}

	if len(live.placed) != 1 {
		t.Fatalf("the live broker received %d orders, want 1 — the real position was not closed",
			len(live.placed))
	}
	got := live.placed[0]
	if got.Side != broker.SideBuy {
		t.Errorf("side = %q, want BUY to close a short", got.Side)
	}
	if got.Quantity != 75 {
		t.Errorf("quantity = %d, want 75", got.Quantity)
	}
	if got.Intent != broker.IntentClose {
		t.Error("the square-off did not carry IntentClose, so the risk limits could refuse an exit")
	}
	// And nothing may have leaked into the simulated book.
	if pos, _ := paper.GetPositions(ctx); len(pos) != 0 {
		t.Errorf("closing a real position created %d paper position(s): %+v", len(pos), pos)
	}
}

// The mirror: closing a PAPER position must stay in the paper book even while
// live routing is armed.
func TestSquaringOffAPaperPositionStaysSimulated(t *testing.T) {
	ctx := context.Background()
	eng, _, live := routingEngine(t)
	eng.SetLiveBroker(live)

	eng.mu.Lock()
	eng.positions = []broker.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY24350CE", Product: broker.ProductMIS,
		NetQuantity: -75, AveragePrice: 120, Book: broker.BookPaper,
		StrategyID: "short-straddle",
	}}
	eng.mu.Unlock()

	if _, err := eng.SquareOff(ctx, "short-straddle", "NIFTY24350CE"); err != nil {
		t.Fatalf("SquareOff: %v", err)
	}
	if len(live.placed) != 0 {
		t.Errorf("closing a SIMULATED position sent %d order(s) to the exchange: %+v",
			len(live.placed), live.placed)
	}
}

// With the live broker gone, closing a real position must FAIL rather than quietly
// place a paper order. The operator has to learn the position is still open.
func TestSquaringOffARealPositionFailsWithoutALiveBroker(t *testing.T) {
	ctx := context.Background()
	eng, paper, _ := routingEngine(t)

	eng.mu.Lock()
	eng.positions = []broker.Position{{
		Exchange: "NFO", TradingSymbol: "NIFTY24350CE", Product: broker.ProductMIS,
		NetQuantity: -75, AveragePrice: 120, Book: broker.BookReal,
	}}
	eng.mu.Unlock()

	_, err := eng.SquareOff(ctx, "", "NIFTY24350CE")
	if !errors.Is(err, ErrNoLiveBroker) {
		t.Errorf("err = %v, want ErrNoLiveBroker", err)
	}
	if pos, _ := paper.GetPositions(ctx); len(pos) != 0 {
		t.Errorf("a phantom paper position was opened instead of failing: %+v", pos)
	}
}

// Standing down must not trap the operator in a real position.
//
// A disarm used to remove the live broker outright, which took away the only
// route to the exchange: the real book vanished from every screen (the engine
// polls it only while the broker is installed) and every attempt to close a
// position failed with ErrNoLiveBroker. The way out was to arm live again —
// phrase, password and all — purely in order to get flat. So a disarm now keeps
// the broker and closes it to entries instead.
func TestDisarmKeepsTheRealBookClosableButRefusesEntries(t *testing.T) {
	ctx := context.Background()
	eng, _, live := routingEngine(t)
	live.positions = []broker.Position{{
		StrategyID: ManualStrategyID, Exchange: "NFO",
		TradingSymbol: "NIFTY24350CE", Product: broker.ProductMIS,
		NetQuantity: -65, AveragePrice: 100,
	}}
	eng.SetLiveBroker(live)
	eng.RefreshPositions(ctx)

	eng.DisarmLiveEntries()

	// Entries are closed, in every way the rest of the system can ask.
	if eng.LiveManualActive() {
		t.Error("live manual still reported active after a disarm")
	}
	if got := eng.RouteMode(); got != RouteRealExitOnly {
		t.Errorf("RouteMode() = %q, want %q", got, RouteRealExitOnly)
	}
	if got := eng.bookFor(wantReal(ManualStrategyID, "NIFTY24350CE")); got != broker.BookPaper {
		t.Errorf("a new manual order routed to %q after a disarm, want paper", got)
	}
	// And refused outright when the book arrives already chosen, which is the
	// path bookFor cannot cover.
	if _, err := eng.placeOrderIn(ctx, wantReal(ManualStrategyID, "NIFTY24350CE"), broker.BookReal); !errors.Is(err, ErrLiveNotArmed) {
		t.Errorf("a real entry placed straight into the real book returned %v, want ErrLiveNotArmed", err)
	}
	if len(live.placed) != 0 {
		t.Fatalf("the live broker took %d order(s) while disarmed", len(live.placed))
	}

	// Exits are open. The position is still visible...
	if !eng.RealExitOnly() {
		t.Error("RealExitOnly() = false while holding a real position after a disarm")
	}
	real := 0
	for _, p := range eng.Positions() {
		if p.Book.IsReal() {
			real++
		}
	}
	if real != 1 {
		t.Fatalf("engine sees %d real positions after a disarm, want 1 — "+
			"a position the desk cannot show is one the operator cannot close", real)
	}

	// ...and still closable, at the live broker rather than quietly on paper.
	o, err := eng.SquareOff(ctx, ManualStrategyID, "NIFTY24350CE")
	if err != nil {
		t.Fatalf("square off after a disarm: %v", err)
	}
	if len(live.placed) != 1 {
		t.Fatalf("live broker got %d closing orders, want 1", len(live.placed))
	}
	if got := live.placed[0].Intent; got != broker.IntentClose {
		t.Errorf("closing order intent = %q, want %q", got, broker.IntentClose)
	}
	if o.Mode != "live" {
		t.Errorf("the close was routed to the %q broker, want live — closing a real "+
			"position on paper leaves the exposure open and reports success", o.Mode)
	}
}

// Removing the broker entirely is still available, and is not exit-only.
func TestRemovingTheLiveBrokerIsNotExitOnly(t *testing.T) {
	eng, _, live := routingEngine(t)
	eng.SetLiveBroker(live)
	eng.SetLiveBroker(nil)

	if eng.RealExitOnly() {
		t.Error("RealExitOnly() = true with no live broker installed")
	}
	if got := eng.RouteMode(); got != RouteAllPaper {
		t.Errorf("RouteMode() = %q, want %q", got, RouteAllPaper)
	}
}

// Arming again after a disarm must restore entries.
func TestReArmingAfterDisarmRestoresEntries(t *testing.T) {
	eng, _, live := routingEngine(t)
	eng.SetLiveBroker(live)
	eng.DisarmLiveEntries()
	eng.SetLiveBroker(live)

	if !eng.LiveManualActive() {
		t.Error("live manual not active after re-arming")
	}
	if got := eng.bookFor(wantReal(ManualStrategyID, "X")); got != broker.BookReal {
		t.Errorf("bookFor = %q after re-arming, want real", got)
	}
}
