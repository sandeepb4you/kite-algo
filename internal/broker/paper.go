package broker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// FillCallback is invoked by PaperBroker whenever an order fills (partially or
// fully). The engine uses it to persist fills and update strategy state.
type FillCallback func(Fill)

// PaperBroker simulates order execution against live market prices. It supports
// MARKET (instant fill), LIMIT (fill when price crosses), and SL/SL-M (fill when
// trigger is hit). No real orders are placed.
//
// The engine calls OnPrice on every tick so the broker can fill pending orders.
type PaperBroker struct {
	mu        sync.Mutex
	prices    map[string]float64        // trading symbol -> last price
	orders    map[string]*Order         // order id -> order
	pending   map[string]*Order         // order id -> pending (unfilled)
	positions map[positionKey]*Position // key -> position
	onFill    FillCallback
	logger    *slog.Logger
	now       func() time.Time // injectable; a backtest supplies simulated time
	fillModel FillModel        // injectable; a backtest supplies slippage
}

type positionKey struct {
	strategy string
	symbol   string
	product  ProductType
}

// NewPaperBroker returns a PaperBroker. onFill may be nil and set later via
// SetOnFill — handy because the engine (which owns the fill handler) is
// constructed after the broker it wraps.
func NewPaperBroker(onFill FillCallback, logger *slog.Logger) *PaperBroker {
	return &PaperBroker{
		prices:    make(map[string]float64),
		orders:    make(map[string]*Order),
		pending:   make(map[string]*Order),
		positions: make(map[positionKey]*Position),
		onFill:    onFill,
		logger:    logger,
		now:       time.Now,
		fillModel: defaultFillModel{},
	}
}

// SetOnFill attaches (or replaces) the fill callback. Use it to wire the engine
// after both are constructed.
func (b *PaperBroker) SetOnFill(cb FillCallback) {
	b.mu.Lock()
	b.onFill = cb
	b.mu.Unlock()
}

// SetClock replaces the broker's time source.
//
// A backtest injects simulated time so order and fill timestamps belong to the
// period being replayed rather than to today. Without this, every fill in a
// backtest of last month would be stamped with the wall clock, and the trade
// ledger would be unusable.
func (b *PaperBroker) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	b.mu.Lock()
	b.now = now
	b.mu.Unlock()
}

// FillModel decides the price a marketable order transacts at.
//
// The default reproduces this broker's original behaviour exactly. Backtests
// substitute a model with slippage, so a simulated fill is not systematically
// better than a real one would have been.
type FillModel interface {
	FillPrice(o *Order, marketPrice float64) float64
}

// SetFillModel replaces the execution price model.
func (b *PaperBroker) SetFillModel(m FillModel) {
	if m == nil {
		return
	}
	b.mu.Lock()
	b.fillModel = m
	b.mu.Unlock()
}

// defaultFillModel fills at the market price, or at the limit for LIMIT orders.
type defaultFillModel struct{}

func (defaultFillModel) FillPrice(o *Order, marketPrice float64) float64 {
	switch o.OrderType {
	case OrderTypeMarket, OrderTypeSLM:
		return marketPrice
	case OrderTypeSL:
		// SL (limit stop): fill at the limit price when one is set.
		if o.Price > 0 {
			return o.Price
		}
		return marketPrice
	case OrderTypeLimit:
		return o.Price
	}
	return marketPrice
}

// Mode reports "paper".
func (b *PaperBroker) Mode() string { return "paper" }

// OnPrice updates the last price for a symbol and fills any pending orders
// whose conditions are now met. Called by the engine on every tick.
func (b *PaperBroker) OnPrice(symbol string, price float64) {
	b.mu.Lock()
	b.prices[symbol] = price
	// Snapshot the pending orders for this symbol to evaluate for fills.
	var toFill []*Order
	for _, o := range b.pending {
		if o.TradingSymbol == symbol {
			toFill = append(toFill, o)
		}
	}
	b.mu.Unlock()

	for _, o := range toFill {
		b.tryFill(o, price)
	}
}

// PlaceOrder records a new order and attempts an immediate fill (market orders
// fill at once; limit/SL orders wait for the right price).
func (b *PaperBroker) PlaceOrder(ctx context.Context, req OrderRequest) (*Order, error) {
	if req.Quantity <= 0 {
		return nil, fmt.Errorf("paper broker: quantity must be positive")
	}
	now := b.now()
	o := &Order{
		ID:            uuid.NewString(),
		ClientOrderID: firstNonEmpty(req.ClientOrderID, uuid.NewString()),
		StrategyID:    req.StrategyID,
		Exchange:      req.Exchange,
		TradingSymbol: req.TradingSymbol,
		Product:       req.Product,
		OrderType:     req.OrderType,
		Side:          req.Side,
		Quantity:      req.Quantity,
		Price:         req.Price,
		TriggerPrice:  req.TriggerPrice,
		Validity:      req.Validity,
		Status:        StatusPending,
		Tag:           req.Tag,
		Mode:          "paper",
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	b.mu.Lock()
	b.orders[o.ID] = o
	price, hasPrice := b.prices[o.TradingSymbol]
	// Every new order starts pending; it leaves pending when it fills or is
	// cancelled. tryFill is a no-op on non-pending orders, so this guards double fills.
	b.pending[o.ID] = o
	b.mu.Unlock()

	// If we already know a price, attempt an immediate fill (market orders fill
	// at once; limit/SL orders fill only if already marketable). Otherwise the
	// order waits for the first OnPrice to bring a price.
	if hasPrice && price > 0 {
		b.tryFill(o, price)
	}
	return o, nil
}

// ModifyOrder updates a pending order's mutable fields.
func (b *PaperBroker) ModifyOrder(ctx context.Context, orderID string, mod ModifyRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.pending[orderID]
	if !ok {
		return fmt.Errorf("paper broker: order %s not pending", orderID)
	}
	if mod.Quantity > 0 {
		o.Quantity = mod.Quantity
	}
	if mod.Price > 0 {
		o.Price = mod.Price
	}
	if mod.TriggerPrice > 0 {
		o.TriggerPrice = mod.TriggerPrice
	}
	if mod.OrderType != "" {
		o.OrderType = mod.OrderType
	}
	if mod.Validity != "" {
		o.Validity = mod.Validity
	}
	o.UpdatedAt = b.now()
	return nil
}

// CancelOrder removes a pending order and marks it cancelled.
func (b *PaperBroker) CancelOrder(ctx context.Context, orderID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.orders[orderID]
	if !ok {
		return fmt.Errorf("paper broker: unknown order %s", orderID)
	}
	if o.Status == StatusComplete || o.Status == StatusCancelled {
		return fmt.Errorf("paper broker: order %s already %s", orderID, o.Status)
	}
	delete(b.pending, orderID)
	o.Status = StatusCancelled
	o.PendingQuantity = 0
	o.UpdatedAt = b.now()
	return nil
}

// GetOrder returns a copy of an order by id, filled or not.
//
// Needed because this broker fills synchronously from inside PlaceOrder, so a
// fill reaches the engine before the engine holds the order it belongs to. The
// engine uses this to persist the parent row first — see Engine.handleFill.
func (b *PaperBroker) GetOrder(id string) (Order, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.orders[id]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// GetOpenOrders returns pending orders.
func (b *PaperBroker) GetOpenOrders(ctx context.Context) ([]Order, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Order, 0, len(b.pending))
	for _, o := range b.pending {
		out = append(out, *o)
	}
	return out, nil
}

// GetPositions returns all position rows, including flat ones (which still
// carry realized P&L for the day). Callers that only want genuinely open
// positions should filter on NetQuantity != 0.
func (b *PaperBroker) GetPositions(ctx context.Context) ([]Position, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Position, 0, len(b.positions))
	for _, p := range b.positions {
		out = append(out, *p)
	}
	return out, nil
}

// tryFill evaluates one pending order against `price`, filling it if conditions
// are met. Safe to call repeatedly; it's a no-op once the order is filled.
func (b *PaperBroker) tryFill(o *Order, price float64) {
	b.mu.Lock()
	if _, ok := b.pending[o.ID]; !ok {
		b.mu.Unlock()
		return // already filled or cancelled
	}
	if !b.isMarketable(o, price) {
		b.mu.Unlock()
		return
	}
	delete(b.pending, o.ID)
	fillPrice := b.fillModel.FillPrice(o, price)
	o.FilledQuantity = o.Quantity
	o.PendingQuantity = 0
	o.Status = StatusComplete
	o.UpdatedAt = b.now()
	fill := Fill{
		ID:            uuid.NewString(),
		OrderID:       o.ID,
		StrategyID:    o.StrategyID,
		Exchange:      o.Exchange,
		TradingSymbol: o.TradingSymbol,
		Side:          o.Side,
		Quantity:      o.Quantity,
		Price:         fillPrice,
		Mode:          "paper",
		Timestamp:     b.now(),
	}
	b.applyFillLocked(fill)
	onFill := b.onFill
	b.mu.Unlock()

	if b.logger != nil {
		b.logger.Info("paper fill",
			"symbol", o.TradingSymbol, "side", o.Side,
			"qty", o.Quantity, "price", fillPrice, "strategy", o.StrategyID)
	}
	if onFill != nil {
		onFill(fill)
	}
}

// isMarketable reports whether `price` satisfies the order's conditions.
func (b *PaperBroker) isMarketable(o *Order, price float64) bool {
	switch o.OrderType {
	case OrderTypeMarket:
		return true
	case OrderTypeLimit:
		if o.Side == SideBuy {
			return price <= o.Price // buy limit: market at or below limit
		}
		return price >= o.Price // sell limit: market at or above limit
	case OrderTypeSL, OrderTypeSLM:
		// Stop-loss: triggers when price falls to (sell) or rises to (buy) trigger.
		if o.Side == SideSell {
			return price <= o.TriggerPrice
		}
		return price >= o.TriggerPrice
	}
	return false
}

// applyFillLocked updates positions for a fill. Caller holds b.mu.
func (b *PaperBroker) applyFillLocked(f Fill) {
	key := positionKey{f.StrategyID, f.TradingSymbol, ProductNRML}
	// Inherit product from the originating order when known.
	if o, ok := b.orders[f.OrderID]; ok && o.Product != "" {
		key.product = o.Product
	}
	p, ok := b.positions[key]
	if !ok {
		p = &Position{
			StrategyID:    f.StrategyID,
			Exchange:      f.Exchange,
			TradingSymbol: f.TradingSymbol,
			Product:       key.product,
		}
		b.positions[key] = p
	}
	ApplyFill(p, f)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
