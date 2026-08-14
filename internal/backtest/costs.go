package backtest

import (
	"math"

	"kite-algo/internal/broker"
	"kite-algo/internal/charges"
)

// CostModel is the shared charge model plus the execution assumptions that only
// a simulation needs.
//
// The rates themselves live in internal/charges so a backtest and the live day's
// running total cannot drift apart — the one number that must not disagree is
// what a strategy costs to run.
type CostModel struct {
	charges.Model

	// SlippageTicks is how many ticks a MARKET order fills away from the quoted
	// price, adversely. Real fills are not free.
	SlippageTicks float64
	// TickSize is the instrument's minimum price increment (₹0.05 on NFO).
	TickSize float64
}

// DefaultNSEOptionCosts returns charges and execution assumptions for NSE index
// options. Verify the statutory rates against Zerodha's brokerage calculator;
// they change with every Indian budget.
func DefaultNSEOptionCosts() CostModel {
	return CostModel{
		Model:         charges.DefaultNSEOptions(),
		SlippageTicks: 1,
		TickSize:      0.05,
	}
}

// Charges is the itemised cost of one fill.
type Charges = charges.Breakdown

// Charge computes the cost of a single fill.
func (m CostModel) Charge(f broker.Fill) Charges { return m.Model.Charge(f) }

// CostOf returns just the total charge for a fill, matching analytics.CostFunc.
func (m CostModel) CostOf(f broker.Fill) float64 { return m.Model.CostOf(f) }

// SlippageFillModel fills market orders adversely by a fixed number of ticks.
//
// Without it every simulated market order transacts at the exact quoted price,
// which no real order does. The bias is one-directional — always against the
// trader — because that is how spread and impact actually behave.
type SlippageFillModel struct {
	Model CostModel
}

// FillPrice implements broker.FillModel.
func (s SlippageFillModel) FillPrice(o *broker.Order, marketPrice float64) float64 {
	switch o.OrderType {
	case broker.OrderTypeLimit:
		// A limit order fills at its limit or better; no slippage by definition.
		return o.Price
	case broker.OrderTypeSL:
		if o.Price > 0 {
			return o.Price
		}
	}

	slip := s.Model.SlippageTicks * s.Model.TickSize
	if slip <= 0 {
		return marketPrice
	}

	// Buying pays up, selling receives less.
	price := marketPrice + slip
	if o.Side == broker.SideSell {
		price = marketPrice - slip
	}
	// A price can never go negative, and an option can trade at its floor.
	return math.Max(price, s.Model.TickSize)
}
