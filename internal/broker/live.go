package broker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"kite-algo/internal/kite"
)

// LiveBroker places real orders through Zerodha Kite. Every call goes to the
// network, so the engine wraps it with a rate limiter (inside kite.Client) and
// the risk manager. Use ONLY when config mode is "live" with the double-gate.
type LiveBroker struct {
	client *kite.Client
	logger *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	idToExch map[string]string // internal order id -> Kite exchange order id
}

// NewLiveBroker wraps an authenticated kite.Client.
func NewLiveBroker(client *kite.Client, logger *slog.Logger) *LiveBroker {
	return &LiveBroker{
		client:   client,
		logger:   logger,
		now:      time.Now,
		idToExch: make(map[string]string),
	}
}

// Mode reports "live".
func (b *LiveBroker) Mode() string { return "live" }

// PlaceOrder submits a real order to Kite and returns it in PENDING/OPEN state.
// Fills arrive later via the ticker's order-update stream (wired by the engine).
func (b *LiveBroker) PlaceOrder(ctx context.Context, req OrderRequest) (*Order, error) {
	if req.Quantity <= 0 {
		return nil, fmt.Errorf("live broker: quantity must be positive")
	}
	params := kite.PlaceOrderParams{
		Variety:         kite.VarietyRegular,
		Exchange:        req.Exchange,
		Tradingsymbol:   req.TradingSymbol,
		TransactionType: string(req.Side),
		Quantity:        req.Quantity,
		Product:         string(req.Product),
		OrderType:       string(req.OrderType),
		Price:           req.Price,
		TriggerPrice:    req.TriggerPrice,
		Validity:        string(orValidity(req.Validity, ValidityDay)),
		Tag:             req.Tag,
	}
	res, err := b.client.PlaceOrder(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("live broker place order: %w", err)
	}

	now := b.now()
	o := &Order{
		ID:              uuid.NewString(),
		ExchangeOrderID: res.OrderID,
		ClientOrderID:   firstNonEmpty(req.ClientOrderID, uuid.NewString()),
		StrategyID:      req.StrategyID,
		Exchange:        req.Exchange,
		TradingSymbol:   req.TradingSymbol,
		Product:         req.Product,
		OrderType:       req.OrderType,
		Side:            req.Side,
		Quantity:        req.Quantity,
		Price:           req.Price,
		TriggerPrice:    req.TriggerPrice,
		Validity:        orValidity(req.Validity, ValidityDay),
		Status:          StatusOpen,
		Tag:             req.Tag,
		Mode:            "live",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	b.mu.Lock()
	b.idToExch[o.ID] = res.OrderID
	b.mu.Unlock()

	if b.logger != nil {
		b.logger.Info("live order placed",
			"symbol", o.TradingSymbol, "side", o.Side, "qty", o.Quantity,
			"price", o.Price, "exchange_id", res.OrderID, "strategy", o.StrategyID)
	}
	return o, nil
}

// ModifyOrder amends a pending live order.
func (b *LiveBroker) ModifyOrder(ctx context.Context, orderID string, mod ModifyRequest) error {
	exchID := b.exchangeID(orderID)
	if exchID == "" {
		return fmt.Errorf("live broker: unknown order %s", orderID)
	}
	_, err := b.client.ModifyOrder(ctx, kite.ModifyOrderParams{
		Variety:      kite.VarietyRegular,
		OrderID:      exchID,
		Quantity:     mod.Quantity,
		OrderType:    string(mod.OrderType),
		Price:        mod.Price,
		TriggerPrice: mod.TriggerPrice,
		Validity:     string(mod.Validity),
	})
	if err != nil {
		return fmt.Errorf("live broker modify order: %w", err)
	}
	return nil
}

// CancelOrder cancels a pending live order.
func (b *LiveBroker) CancelOrder(ctx context.Context, orderID string) error {
	exchID := b.exchangeID(orderID)
	if exchID == "" {
		return fmt.Errorf("live broker: unknown order %s", orderID)
	}
	_, err := b.client.CancelOrder(ctx, kite.VarietyRegular, exchID, "")
	if err != nil {
		return fmt.Errorf("live broker cancel order: %w", err)
	}
	return nil
}

// GetOpenOrders fetches today's order book and keeps only pending/open rows.
func (b *LiveBroker) GetOpenOrders(ctx context.Context) ([]Order, error) {
	ko, err := b.client.GetOrders(ctx)
	if err != nil {
		return nil, fmt.Errorf("live broker get orders: %w", err)
	}
	out := make([]Order, 0, len(ko))
	for _, k := range ko {
		st := mapStatus(k.Status)
		if st != StatusOpen && st != StatusPending {
			continue
		}
		out = append(out, Order{
			ExchangeOrderID: k.OrderID,
			TradingSymbol:   k.Tradingsymbol,
			Exchange:        k.Exchange,
			Product:         ProductType(k.Product),
			OrderType:       OrderType(k.OrderType),
			Side:            Side(k.TransactionType),
			Quantity:        int(k.Quantity),
			FilledQuantity:  int(k.FilledQuantity),
			PendingQuantity: int(k.PendingQuantity),
			Price:           k.Price,
			TriggerPrice:    k.TriggerPrice,
			Validity:        Validity(k.Validity),
			Status:          st,
			Tag:             k.Tag,
			RejectReason:    k.StatusMessage,
			Mode:            "live",
		})
	}
	return out, nil
}

// GetPositions maps Kite's net positions into broker.Position.
func (b *LiveBroker) GetPositions(ctx context.Context) ([]Position, error) {
	views, err := b.client.GetPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("live broker get positions: %w", err)
	}
	net := views["net"]
	out := make([]Position, 0, len(net))
	for _, k := range net {
		if k.Quantity == 0 {
			continue
		}
		out = append(out, Position{
			Exchange:      k.Exchange,
			TradingSymbol: k.Tradingsymbol,
			Product:       ProductType(k.Product),
			NetQuantity:   k.Quantity,
			AveragePrice:  k.AveragePrice,
			LastPrice:     k.LastPrice,
			PnL:           k.PnL,
			Updated:       b.now(),
		})
	}
	return out, nil
}

// exchangeID looks up the Kite exchange order id for an internal order id.
func (b *LiveBroker) exchangeID(internalID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.idToExch[internalID]
}

// mapStatus converts a Kite status string to our OrderStatus.
func mapStatus(s string) OrderStatus {
	switch s {
	case "COMPLETE":
		return StatusComplete
	case "REJECTED":
		return StatusRejected
	case "CANCELLED", "CANCELLED ":
		return StatusCancelled
	case "PENDING":
		return StatusPending
	default: // OPEN, TRIGGER PENDING, AFTER MARKET ORDER, etc.
		return StatusOpen
	}
}

// orValidity returns a if set, else b.
func orValidity(a, b Validity) Validity {
	if a != "" {
		return a
	}
	return b
}
