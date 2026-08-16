// Package broker defines the broker abstraction and the core trading domain
// types used across the platform. Only the types live here for now; the Broker
// interface and its live/paper implementations are added in a later phase.
//
// These types are broker-agnostic. The Kite-specific translation happens inside
// the live broker implementation, keeping the rest of the platform decoupled
// from Zerodha's wire format.
package broker

import "time"

// Side indicates buy or sell.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderType maps to Kite's order varieties.
type OrderType string

const (
	OrderTypeMarket OrderType = "MARKET"
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeSL     OrderType = "SL"   // stop-loss (market)
	OrderTypeSLM    OrderType = "SL-M" // stop-loss market
)

// ProductType maps to Kite's product codes.
type ProductType string

const (
	ProductMIS  ProductType = "MIS"  // intraday
	ProductNRML ProductType = "NRML" // overnight (futures/options)
	ProductCNC  ProductType = "CNC"  // equity delivery
)

// OrderStatus is the lifecycle state of an order.
type OrderStatus string

const (
	StatusPending   OrderStatus = "PENDING"
	StatusOpen      OrderStatus = "OPEN"     // with exchange
	StatusComplete  OrderStatus = "COMPLETE" // fully filled
	StatusRejected  OrderStatus = "REJECTED"
	StatusCancelled OrderStatus = "CANCELLED"
)

// Validity is the time-in-force.
type Validity string

const (
	ValidityDay Validity = "DAY"
	ValidityIOC Validity = "IOC"
)

// OrderIntent says whether an order opens exposure or reduces it.
//
// This is a risk-control input, not an exchange field. Kite has no such concept;
// it exists so the risk manager can tell "I want to take a new position" apart
// from "I want out of the one I have". Limits that exist to stop you digging
// deeper must never stop you climbing out.
type OrderIntent string

const (
	// IntentOpen is the default: the order may increase exposure.
	IntentOpen OrderIntent = ""
	// IntentClose marks an order that reduces or flattens an existing position —
	// a square-off, a stop, or a strategy unwinding a leg.
	IntentClose OrderIntent = "close"
)

// OrderRequest is what a strategy submits to place an order.
type OrderRequest struct {
	StrategyID    string      // which strategy placed this order (audit trail)
	Intent        OrderIntent // open (default) or close; see OrderIntent
	Exchange      string      // NFO, NSE, ...
	TradingSymbol string      // e.g. NIFTY24AUG24500CE
	Product       ProductType
	OrderType     OrderType
	Side          Side
	Quantity      int
	Price         float64 // limit price (0 for market)
	TriggerPrice  float64 // stop-loss trigger (0 if none)
	Validity      Validity
	Tag           string // free-form strategy tag
	ClientOrderID string // optional idempotency key (we generate one if empty)

	// Book requests which set of books this order belongs to.
	//
	// Only a MANUAL order may ask for BookReal, and only while live routing is
	// armed; see engine.bookFor, which is the single place that decides. The
	// zero value is BookPaper, so an order that forgets to say is simulated —
	// the only safe direction for a field that decides whether real money moves.
	//
	// It exists because "manual" and "real" are not the same thing. An operator
	// running strategies on paper still wants to place occasional manual orders
	// into the SIMULATED book to try something by hand, and routing every manual
	// order live the moment live is armed would make that impossible.
	Book Book
}

// Order is the persisted/returned representation of an order after submission.
type Order struct {
	ID              string // internal id
	ExchangeOrderID string // broker/exchange id
	ClientOrderID   string // idempotency key
	StrategyID      string
	Exchange        string
	TradingSymbol   string
	Product         ProductType
	OrderType       OrderType
	Side            Side
	Quantity        int
	FilledQuantity  int
	PendingQuantity int
	Price           float64
	TriggerPrice    float64
	Validity        Validity
	Status          OrderStatus
	Tag             string
	Mode            string // paper | live — which broker handled this
	CreatedAt       time.Time
	UpdatedAt       time.Time
	RejectReason    string
}

// Fill is a (possibly partial) execution against an order.
type Fill struct {
	ID              string
	OrderID         string // internal order id
	ExchangeOrderID string
	StrategyID      string
	Exchange        string
	TradingSymbol   string
	Side            Side
	Quantity        int
	Price           float64
	Mode            string // paper | live
	Timestamp       time.Time
}

// Position is a net open position in one instrument.
type Position struct {
	StrategyID    string
	Exchange      string
	TradingSymbol string
	Product       ProductType
	NetQuantity   int     // positive long, negative short
	AveragePrice  float64 // average entry price
	LastPrice     float64 // current market price
	// PnL is the unrealized + realized profit/loss in rupees at LastPrice.
	PnL     float64
	Updated time.Time
	// Book records whether this position is real money or simulated.
	//
	// Both can exist at once: manual orders route to the exchange while
	// strategies stay on the paper broker. Without this field a simulated
	// position is indistinguishable from a real one in every list, total and
	// P&L figure the operator reads — which is the single most expensive
	// confusion this platform could offer.
	Book Book
}

// Book identifies which set of books a position, order or fill belongs to.
type Book string

const (
	// BookReal is money at the exchange.
	BookReal Book = "real"
	// BookPaper is simulated.
	BookPaper Book = "paper"
)

// IsReal reports whether the book is real money. Defaults to false for the
// zero value, so anything unlabelled is treated as simulated — the safe way
// round for a flag that gates how a number is presented.
func (b Book) IsReal() bool { return b == BookReal }

// String satisfies fmt.Stringer, defaulting the zero value to paper.
func (b Book) String() string {
	if b == BookReal {
		return string(BookReal)
	}
	return string(BookPaper)
}

// IsLong reports whether the position is net long.
func (p Position) IsLong() bool { return p.NetQuantity > 0 }

// IsShort reports whether the position is net short.
func (p Position) IsShort() bool { return p.NetQuantity < 0 }

// IsOpen reports whether the position is non-zero.
func (p Position) IsOpen() bool { return p.NetQuantity != 0 }
