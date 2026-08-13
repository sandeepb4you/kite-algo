package broker

import "context"

// ModifyRequest holds the mutable fields of a pending order.
type ModifyRequest struct {
	Quantity      int
	Price         float64
	TriggerPrice  float64
	OrderType     OrderType
	Validity      Validity
}

// Broker is the order-execution abstraction. The live and paper brokers both
// implement it; strategies and the engine code against this interface so the
// execution venue is a config-time choice.
type Broker interface {
	// PlaceOrder submits a new order and returns the resulting Order (typically
	// in a PENDING/OPEN state; fills arrive via order updates or polling).
	PlaceOrder(ctx context.Context, req OrderRequest) (*Order, error)

	// ModifyOrder amends a pending order. Implementations that don't support
	// modification (e.g. a very simple paper broker) may cancel-and-replace.
	ModifyOrder(ctx context.Context, orderID string, mod ModifyRequest) error

	// CancelOrder cancels a pending order.
	CancelOrder(ctx context.Context, orderID string) error

	// GetOpenOrders returns currently pending/open orders.
	GetOpenOrders(ctx context.Context) ([]Order, error)

	// GetPositions returns current net positions.
	GetPositions(ctx context.Context) ([]Position, error)

	// Mode reports "paper" or "live" — recorded on every order/fill for audit.
	Mode() string
}
