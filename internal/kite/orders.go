package kite

import (
	"context"
	"fmt"
	"net/url"
)

// Variety is the Kite order variety.
const (
	VarietyRegular = "regular"
	VarietyAMO     = "amo"
	VarietyBO      = "bo"
	VarietyCO      = "co"
)

// PlaceOrderParams mirrors Kite's place-order form fields. The LiveBroker
// builds this from the platform's broker.OrderRequest.
type PlaceOrderParams struct {
	Variety           string // regular | amo | bo | co
	Exchange          string // NFO, NSE, ...
	Tradingsymbol     string
	TransactionType   string // BUY | SELL
	Quantity          int
	Product           string  // MIS | NRML | CNC
	OrderType         string  // MARKET | LIMIT | SL | SL-M
	Price             float64 // limit price
	TriggerPrice      float64 // stop-loss trigger
	Validity          string  // DAY | IOC
	DisclosedQuantity int
	Tag               string

	// MarketProtection caps how far from the last price a MARKET order may fill,
	// as a percentage. -1 asks Zerodha for its own automatic band.
	//
	// Not optional in practice. The exchanges require it on algo MARKET orders,
	// and Kite rejects a MARKET order that arrives without one — "Market orders
	// not allowed without market protection" — so omitting this field means no
	// market order can be placed at all on the real book. Zero must never be
	// sent: Kite rejects that explicitly, which is why setFloat skipping zero is
	// load-bearing here rather than incidental.
	MarketProtection float64
}

// OrderResult is the minimal response from place/modify/cancel.
type OrderResult struct {
	OrderID string `json:"order_id"`
}

// PlaceOrder places a new order and returns the Kite order id.
func (c *Client) PlaceOrder(ctx context.Context, p PlaceOrderParams) (OrderResult, error) {
	if p.Variety == "" {
		p.Variety = VarietyRegular
	}
	form := paramsToForm(p)
	var out OrderResult
	if err := c.postForm(ctx, "/orders/"+p.Variety, form, &out); err != nil {
		return OrderResult{}, err
	}
	return out, nil
}

// ModifyOrderParams mirrors the mutable fields of an order.
type ModifyOrderParams struct {
	Variety           string
	OrderID           string
	Quantity          int
	OrderType         string
	Price             float64
	TriggerPrice      float64
	Validity          string
	DisclosedQuantity int
}

// ModifyOrder modifies an existing pending order.
func (c *Client) ModifyOrder(ctx context.Context, p ModifyOrderParams) (OrderResult, error) {
	if p.Variety == "" {
		p.Variety = VarietyRegular
	}
	form := url.Values{}
	setInt(form, "quantity", p.Quantity)
	setStr(form, "order_type", p.OrderType)
	setFloat(form, "price", p.Price)
	setFloat(form, "trigger_price", p.TriggerPrice)
	setStr(form, "validity", p.Validity)
	setInt(form, "disclosed_quantity", p.DisclosedQuantity)

	var out OrderResult
	if err := c.putForm(ctx, fmt.Sprintf("/orders/%s/%s", p.Variety, p.OrderID), form, &out); err != nil {
		return OrderResult{}, err
	}
	return out, nil
}

// CancelOrder cancels a pending order. For BO/CO orders parent_order_id may be
// needed; that's handled by passing variety correctly.
func (c *Client) CancelOrder(ctx context.Context, variety, orderID string, parentOrderID string) (OrderResult, error) {
	if variety == "" {
		variety = VarietyRegular
	}
	q := url.Values{}
	if parentOrderID != "" {
		q.Set("parent_order_id", parentOrderID)
	}
	var out OrderResult
	if err := c.delete(ctx, fmt.Sprintf("/orders/%s/%s", variety, orderID), q, &out); err != nil {
		return OrderResult{}, err
	}
	return out, nil
}

// KiteOrder is the full order record from GET /orders.
type KiteOrder struct {
	OrderID           string  `json:"order_id"`
	ExchangeOrderID   string  `json:"exchange_order_id"`
	Tradingsymbol     string  `json:"tradingsymbol"`
	Exchange          string  `json:"exchange"`
	TransactionType   string  `json:"transaction_type"` // BUY | SELL
	Product           string  `json:"product"`
	OrderType         string  `json:"order_type"`
	Quantity          float64 `json:"quantity"`
	FilledQuantity    float64 `json:"filled_quantity"`
	PendingQuantity   float64 `json:"pending_quantity"`
	Price             float64 `json:"price"`
	TriggerPrice      float64 `json:"trigger_price"`
	Validity          string  `json:"validity"`
	Status            string  `json:"status"` // COMPLETE, REJECTED, CANCELLED, OPEN...
	Tag               string  `json:"tag"`
	StatusMessage     string  `json:"status_message"`
	AveragePrice      float64 `json:"average_price"`
	OrderTimestampRaw string  `json:"order_timestamp"`
}

// GetOrders returns today's order book.
func (c *Client) GetOrders(ctx context.Context) ([]KiteOrder, error) {
	var out []KiteOrder
	if err := c.get(ctx, "/orders", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetOrderHistory returns the state history for a single order.
func (c *Client) GetOrderHistory(ctx context.Context, orderID string) ([]KiteOrder, error) {
	var out []KiteOrder
	if err := c.get(ctx, "/orders/"+orderID, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// paramsToForm converts PlaceOrderParams to url.Values for form submission.
func paramsToForm(p PlaceOrderParams) url.Values {
	f := url.Values{}
	setStr(f, "exchange", p.Exchange)
	setStr(f, "tradingsymbol", p.Tradingsymbol)
	setStr(f, "transaction_type", p.TransactionType)
	setInt(f, "quantity", p.Quantity)
	setStr(f, "product", p.Product)
	setStr(f, "order_type", p.OrderType)
	setFloat(f, "price", p.Price)
	setFloat(f, "trigger_price", p.TriggerPrice)
	setStr(f, "validity", p.Validity)
	setInt(f, "disclosed_quantity", p.DisclosedQuantity)
	setStr(f, "tag", p.Tag)
	// Only MARKET and SL-M take market protection. LIMIT and SL carry their own
	// price bound, and Kite documents the parameter as having no effect on them;
	// gated here rather than at the call site so no caller can get it wrong.
	if p.OrderType == "MARKET" || p.OrderType == "SL-M" {
		setFloat(f, "market_protection", p.MarketProtection)
	}
	return f
}

func setStr(f url.Values, k, v string) {
	if v != "" {
		f.Set(k, v)
	}
}
func setInt(f url.Values, k string, v int) {
	if v != 0 {
		f.Set(k, fmt.Sprintf("%d", v))
	}
}
func setFloat(f url.Values, k string, v float64) {
	if v != 0 {
		f.Set(k, fmt.Sprintf("%g", v))
	}
}
