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

	// marketProtection is the band sent with MARKET orders. -1 means Zerodha
	// picks. See NewLiveBroker for why this cannot be zero.
	marketProtection float64

	mu       sync.Mutex
	idToExch map[string]string // internal order id -> Kite exchange order id
}

// NewLiveBroker wraps an authenticated kite.Client.
//
// marketProtection is the percentage band applied to MARKET orders; pass -1 for
// Zerodha's automatic band, which is what the Kite web UI uses. A zero is
// corrected to -1 rather than sent: the exchanges mandate market protection on
// algo market orders and Kite refuses a MARKET order that arrives without one
// ("Market orders not allowed without market protection"), so a zero here would
// silently disable every market order on the real book — including the
// square-offs behind the panic button and the expiry-day sweep, which are the two
// places a refusal is least affordable.
func NewLiveBroker(client *kite.Client, logger *slog.Logger, marketProtection float64) *LiveBroker {
	if marketProtection == 0 || marketProtection < -1 || marketProtection > 100 {
		marketProtection = -1
	}
	return &LiveBroker{
		client:           client,
		logger:           logger,
		now:              time.Now,
		marketProtection: marketProtection,
		idToExch:         make(map[string]string),
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
		// Ignored by Kite for LIMIT and SL; required for MARKET and SL-M.
		MarketProtection: b.marketProtection,
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
//
// Flat rows are KEPT, and PnL is the REALIZED figure only. Both are required by
// the Broker contract, and getting either wrong corrupts the day P&L that the
// daily-loss limit reads — see the two notes below.
func (b *LiveBroker) GetPositions(ctx context.Context) ([]Position, error) {
	views, err := b.client.GetPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("live broker get positions: %w", err)
	}
	net := views["net"]
	out := make([]Position, 0, len(net))
	for _, k := range net {
		// Flat rows (quantity 0) are a fully-closed position and still carry the
		// day's realized P&L. Kite keeps them in `net`; this used to drop them,
		// so closing a live position made its realized loss VANISH from the day
		// P&L — and that is the number risk.Check compares against the
		// daily-loss limit. A day of round trips therefore under-reported its
		// losses and the cap failed to trip. Every consumer that wants only
		// genuinely open positions filters on IsOpen(), the same way it already
		// does for the paper broker, which has always returned these rows.
		out = append(out, Position{
			Exchange:      k.Exchange,
			TradingSymbol: k.Tradingsymbol,
			Product:       ProductType(k.Product),
			NetQuantity:   k.Quantity,
			AveragePrice:  k.AveragePrice,
			LastPrice:     k.LastPrice,
			PnL:           realizedPnL(k),
			Updated:       b.now(),
		})
	}
	return out, nil
}

// realizedPnL extracts the realized-only P&L from a Kite position row.
//
// The engine keeps broker figures as the realized BASELINE and adds unrealized
// on top of it on every tick (engine.markPositionsToMarket). Kite's own `pnl`
// is the TOTAL — `(sell_value - buy_value) + quantity * last_price *
// multiplier` — so passing it through counted the unrealized part twice, once
// from Kite and once from the mark. An open live position reported roughly
// double its true gain or loss, which tripped the daily-loss limit early in the
// same code path where a closed position tripped it late.
//
// Backing the mark-to-market term out of Kite's formula leaves the realized
// part: substituting average_price for last_price prices the still-open
// quantity at cost, which is what "nothing realized yet" means.
//
//	realized = (sell_value - buy_value) + quantity * average_price * multiplier
//
// Verified against the cases in TestRealizedPnL: untouched long or short -> 0,
// partial close -> the closed portion only, fully closed -> sell minus buy.
func realizedPnL(k kite.KitePosition) float64 {
	multiplier := k.Multiplier
	if multiplier == 0 {
		// Absent or zero means 1 for the index options this trades. Defaulting
		// to 0 would silently zero the open leg's cost basis and report the
		// whole unrealized move as realized.
		multiplier = 1
	}
	return (k.SellValue - k.BuyValue) + float64(k.Quantity)*k.AveragePrice*multiplier
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
