package backtest

import (
	"math"

	"kite-algo/internal/broker"
)

// CostModel holds the transaction charges applied to simulated fills.
//
// A short straddle enters and exits two legs every day; at roughly ₹120–200 per
// round trip, ignoring costs turns a losing strategy into a winning one on
// paper. Modelling them approximately is far better than modelling them at zero.
//
// ⚠️ VERIFY THESE RATES against Zerodha's brokerage calculator before trusting a
// backtest. Indian statutory charges change with every budget — options STT was
// raised from 0.0625% to 0.1% of sell-side premium effective 1 October 2024, and
// the defaults here reflect the post-change regime.
type CostModel struct {
	// BrokerageFlat is charged per executed order (₹20 on Zerodha F&O).
	BrokerageFlat float64
	// BrokeragePercent applies to turnover, capped at BrokerageCap. Zero for
	// options, which are flat-rate.
	BrokeragePercent float64
	BrokerageCap     float64

	// STTSellPercent is Securities Transaction Tax, charged on the SELL side
	// only, as a fraction of option premium.
	STTSellPercent float64
	// ExchangeTxnPercent is the exchange transaction charge on premium turnover.
	ExchangeTxnPercent float64
	// SEBIPercent is the SEBI turnover fee (₹10 per crore).
	SEBIPercent float64
	// StampDutyBuyPercent applies on the BUY side only.
	StampDutyBuyPercent float64
	// GSTPercent applies to (brokerage + exchange txn + SEBI), not to STT.
	GSTPercent float64

	// SlippageTicks is how many ticks a MARKET order fills away from the quoted
	// price, adversely. Real fills are not free.
	SlippageTicks float64
	// TickSize is the instrument's minimum price increment (₹0.05 on NFO).
	TickSize float64
}

// DefaultNSEOptionCosts returns charges for NSE index options as of the
// post-October-2024 regime. Verify before relying on them.
func DefaultNSEOptionCosts() CostModel {
	return CostModel{
		BrokerageFlat:       20,
		BrokerageCap:        20,
		STTSellPercent:      0.001,     // 0.1% of sell-side premium
		ExchangeTxnPercent:  0.0003503, // NSE F&O options, on premium
		SEBIPercent:         0.000001,  // ₹10 per crore
		StampDutyBuyPercent: 0.00003,   // 0.003% on the buy side
		GSTPercent:          0.18,
		SlippageTicks:       1,
		TickSize:            0.05,
	}
}

// Charges is the breakdown of what one fill cost.
type Charges struct {
	Brokerage   float64 `json:"brokerage"`
	STT         float64 `json:"stt"`
	ExchangeTxn float64 `json:"exchange_txn"`
	SEBI        float64 `json:"sebi"`
	StampDuty   float64 `json:"stamp_duty"`
	GST         float64 `json:"gst"`
	Total       float64 `json:"total"`
}

// Charge computes the cost of a single fill.
func (m CostModel) Charge(f broker.Fill) Charges {
	turnover := f.Price * float64(f.Quantity)
	if turnover <= 0 {
		return Charges{}
	}

	var c Charges

	c.Brokerage = m.BrokerageFlat
	if m.BrokeragePercent > 0 {
		pct := turnover * m.BrokeragePercent
		if m.BrokerageCap > 0 && pct > m.BrokerageCap {
			pct = m.BrokerageCap
		}
		c.Brokerage = pct
	}

	// STT is sell-side only; stamp duty is buy-side only. Applying either to
	// both sides would roughly double that component.
	if f.Side == broker.SideSell {
		c.STT = turnover * m.STTSellPercent
	} else {
		c.StampDuty = turnover * m.StampDutyBuyPercent
	}

	c.ExchangeTxn = turnover * m.ExchangeTxnPercent
	c.SEBI = turnover * m.SEBIPercent
	// GST applies to the service charges, not to the statutory taxes.
	c.GST = (c.Brokerage + c.ExchangeTxn + c.SEBI) * m.GSTPercent

	c.Total = c.Brokerage + c.STT + c.ExchangeTxn + c.SEBI + c.StampDuty + c.GST
	return c
}

// CostOf returns just the total charge for a fill, matching analytics.CostFunc.
func (m CostModel) CostOf(f broker.Fill) float64 { return m.Charge(f).Total }

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
