package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"kite-algo/internal/broker"
)

// ManualStrategyID attributes orders placed by hand in the web UI.
//
// Positions are keyed by strategy + symbol + product, so giving manual trades
// their own identity keeps them from merging into a running strategy's book —
// which would corrupt that strategy's P&L and confuse its exit logic.
const ManualStrategyID = "manual"

// ErrNoPosition is returned when asked to close something that is already flat.
var ErrNoPosition = errors.New("no open position in that symbol")

// ErrNoLiveBroker is returned when the real book must be reached and cannot be.
//
// Only a square-off can produce this: closing a real position requires the live
// broker, and quietly using the paper one instead would leave the real exposure
// open while reporting success.
var ErrNoLiveBroker = errors.New(
	"no live broker: the real position cannot be squared off until the Zerodha session is active")

// ErrLiveNotArmed is returned when a NEW real order is attempted while live
// routing is disarmed.
//
// It exists because a disarm no longer removes the live broker — it is kept so
// open real positions can still be closed. That leaves the real book reachable,
// so the ban on opening new real risk has to be stated somewhere rather than
// falling out of there being no broker to reach. Closing orders (IntentClose)
// are exempt: refusing those is the trap the change exists to remove.
var ErrLiveNotArmed = errors.New(
	"live routing is not armed: only closing orders may reach the real book")

// PlaceManualOrder submits an operator's order from the web UI.
//
// It deliberately routes through PlaceOrder rather than the broker directly, so
// a hand-typed order gets the same risk checks, persistence, and event
// publication as one from a strategy. A manual order is not a trusted order.
func (e *Engine) PlaceManualOrder(ctx context.Context, req broker.OrderRequest) (*broker.Order, error) {
	req.StrategyID = ManualStrategyID
	if req.Validity == "" {
		req.Validity = broker.ValidityDay
	}
	if req.Exchange == "" {
		req.Exchange = e.exchangeFor(req.TradingSymbol)
	}
	if req.Tag == "" {
		req.Tag = "manual"
	}
	return e.PlaceOrder(ctx, req)
}

// SquareOff flattens the open position in one symbol with a market order.
//
// The order carries IntentClose, which exempts it from the exposure limits in
// the risk manager. Without that, flattening would be refused on exactly the
// day the daily-loss limit had tripped.
func (e *Engine) SquareOff(ctx context.Context, strategyID, symbol string) (*broker.Order, error) {
	var target *broker.Position
	for _, p := range e.Positions() {
		if p.TradingSymbol != symbol || !p.IsOpen() {
			continue
		}
		if strategyID != "" && p.StrategyID != strategyID {
			continue
		}
		found := p
		target = &found
		break
	}
	if target == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoPosition, symbol)
	}
	return e.flatten(ctx, *target)
}

// SquareOffAll flattens every open position. It returns the orders it placed
// and any per-position failures, having attempted all of them: one symbol
// failing must not leave the rest of the book open.
func (e *Engine) SquareOffAll(ctx context.Context) ([]*broker.Order, []error) {
	return e.squareOff(ctx, e.Positions())
}

// SquareOffBook flattens one book, leaving the other alone.
//
// Split from SquareOffAll because the two are different actions with different
// stakes. Flattening the simulated book costs nothing and should be one click;
// flattening the real one spends money and must be deliberate. A single control
// doing both forces the careful treatment onto the harmless case, which is how
// an operator learns to click through the prompt that matters.
func (e *Engine) SquareOffBook(ctx context.Context, book broker.Book) ([]*broker.Order, []error) {
	var want []broker.Position
	for _, p := range e.Positions() {
		if p.Book.IsReal() == book.IsReal() {
			want = append(want, p)
		}
	}
	return e.squareOff(ctx, want)
}

// squareOff flattens the given positions, shorts first.
func (e *Engine) squareOff(ctx context.Context, positions []broker.Position) ([]*broker.Order, []error) {
	var (
		placed []*broker.Order
		errs   []error
	)
	for _, p := range liquidationOrder(positions) {
		o, err := e.flatten(ctx, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.TradingSymbol, err))
			continue
		}
		placed = append(placed, o)
	}
	return placed, errs
}

// liquidationOrder sequences a flatten so SHORT legs are covered first.
//
// Closing a short means BUYING it back, and that is the order that must go
// first. Consider a spread — short 24350 CE, long 24500 CE. Sell the long leg
// first and the hedge is gone: for the moment between the two orders the book
// is NAKED SHORT, margin spikes, and the broker can reject the second leg
// outright, leaving the operator holding exactly the position they were trying
// to escape. Buying the short back first walks the other way: the interim state
// is long-only, risk is capped, and margin falls rather than rises.
//
// Flat positions are dropped here rather than at the call site so every
// liquidation path gets the same ordering.
func liquidationOrder(positions []broker.Position) []broker.Position {
	out := make([]broker.Position, 0, len(positions))
	for _, p := range positions {
		if p.IsOpen() {
			out = append(out, p)
		}
	}
	// Stable, so positions within a leg keep their incoming order and a
	// liquidation is reproducible.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].IsShort() && !out[j].IsShort()
	})
	return out
}

// flatten places the opposing market order for one position.
func (e *Engine) flatten(ctx context.Context, p broker.Position) (*broker.Order, error) {
	side := broker.SideSell
	qty := p.NetQuantity
	if qty < 0 {
		side = broker.SideBuy
		qty = -qty
	}

	exchange := p.Exchange
	if exchange == "" {
		exchange = e.exchangeFor(p.TradingSymbol)
	}

	if e.logger != nil {
		e.logger.Warn("squaring off position",
			"symbol", p.TradingSymbol, "strategy", p.StrategyID,
			"qty", qty, "side", side)
	}

	// placeOrderInternal, NOT PlaceOrder: a square-off must bypass the kill
	// switch. Halting exists to stop opening new risk, and the panic button
	// halts before it flattens — routing this through the public path would
	// make the kill switch block its own square-off, leaving the operator
	// frozen holding everything they just asked to close.
	//
	// The risk manager still runs; IntentClose exempts the exposure limits.
	//
	// placeOrderIn, with the position's OWN book, not placeOrderInternal. A
	// square-off closes a specific position in a specific book, and that book is
	// a fact about the position rather than something to re-derive from the
	// request being synthesised here. Inferring it sent every real square-off to
	// the paper broker: the real position stayed open, a phantom paper position
	// appeared facing the other way, and the UI reported the close as done.
	return e.placeOrderIn(ctx, broker.OrderRequest{
		StrategyID:    p.StrategyID,
		Intent:        broker.IntentClose,
		Exchange:      exchange,
		TradingSymbol: p.TradingSymbol,
		Product:       p.Product,
		OrderType:     broker.OrderTypeMarket,
		Side:          side,
		Quantity:      qty,
		Validity:      broker.ValidityDay,
		Tag:           "square-off",
		// Carried for the audit trail. The routing decision is the explicit book
		// below; this makes the persisted order agree with where it went.
		Book: p.Book,
	}, p.Book)
}

// OpenOrders returns the currently pending orders from the active broker.
func (e *Engine) OpenOrders(ctx context.Context) ([]broker.Order, error) {
	br := e.currentBroker()
	if br == nil {
		return nil, nil
	}
	return br.GetOpenOrders(ctx)
}

// OpenOrdersFor returns the pending orders held in ONE book.
//
// The desks need this because open orders live in whichever broker accepted
// them, and OpenOrders only ever asks the active (simulated) one. On the live
// desk that was wrong twice over: it listed simulated orders on the page whose
// whole purpose is real money, and — worse — a REAL working order sitting at the
// exchange did not appear on the desk that placed it. An order you cannot see is
// an order you cannot cancel.
//
// A missing live broker returns no orders rather than an error: the real book
// simply has none to show, and an error banner on a polled panel would be noise
// on every refresh.
func (e *Engine) OpenOrdersFor(ctx context.Context, book broker.Book) ([]broker.Order, error) {
	if book.IsReal() {
		live := e.liveBrokerOrNil()
		if live == nil {
			return nil, nil
		}
		return live.GetOpenOrders(ctx)
	}
	br := e.paperBrokerOrCurrent()
	if br == nil {
		return nil, nil
	}
	return br.GetOpenOrders(ctx)
}

// ExchangeFor resolves a symbol's exchange from the instrument master, falling
// back to NFO — the only segment this platform currently loads.
func (e *Engine) ExchangeFor(symbol string) string { return e.exchangeFor(symbol) }

func (e *Engine) exchangeFor(symbol string) string {
	e.cmu.RLock()
	instruments := e.instruments
	e.cmu.RUnlock()
	if instruments != nil {
		if inst, ok := instruments.Lookup(symbol); ok {
			return inst.Exchange
		}
	}
	return "NFO"
}

// SearchInstruments returns tradable symbols matching a query, for the order
// ticket's typeahead. Matching is a case-insensitive substring on the trading
// symbol; results are capped because an option chain has thousands of entries.
func (e *Engine) SearchInstruments(query string, limit int) []Instrument {
	e.cmu.RLock()
	instruments := e.instruments
	e.cmu.RUnlock()
	if instruments == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	if limit <= 0 {
		limit = 20
	}

	matches := instruments.Search(query, limit)
	out := make([]Instrument, 0, len(matches))
	for _, inst := range matches {
		v := Instrument{
			TradingSymbol: inst.TradingSymbol,
			Exchange:      inst.Exchange,
			LotSize:       inst.LotSize,
			Type:          inst.InstrumentType,
			Strike:        inst.Strike,
		}
		if !inst.Expiry.IsZero() {
			v.Expiry = inst.Expiry.Format("02 Jan 2006")
		}
		out = append(out, v)
	}
	return out
}

// Instrument is the UI-facing view of a tradable instrument.
type Instrument struct {
	TradingSymbol string  `json:"symbol"`
	Exchange      string  `json:"exchange"`
	LotSize       int     `json:"lot_size"`
	Type          string  `json:"type"`
	Strike        float64 `json:"strike,omitempty"`
	Expiry        string  `json:"expiry,omitempty"`
}
