// Package charges models Indian broking transaction costs.
//
// It lives outside the backtester on purpose: the same arithmetic has to serve a
// backtest, a paper session, and the live day's running total. Two
// implementations would eventually disagree, and the one place that must not
// disagree is the number telling you what a strategy actually costs to run.
//
// ⚠️ These are ESTIMATES. Zerodha publishes no real-time charges API — the
// authoritative figures arrive on the contract note after the close. Statutory
// rates also change with every Indian budget, so verify against Zerodha's
// brokerage calculator before relying on them.
package charges

import "kite-algo/internal/broker"

// Model holds the rates applied to a fill.
type Model struct {
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
}

// DefaultNSEOptions returns charges for NSE index options under the
// post-October-2024 regime, when options STT rose from 0.0625% to 0.1% of
// sell-side premium.
func DefaultNSEOptions() Model {
	return Model{
		BrokerageFlat:       20,
		BrokerageCap:        20,
		STTSellPercent:      0.001,     // 0.1% of sell-side premium
		ExchangeTxnPercent:  0.0003503, // NSE F&O options, on premium
		SEBIPercent:         0.000001,  // ₹10 per crore
		StampDutyBuyPercent: 0.00003,   // 0.003% on the buy side
		GSTPercent:          0.18,
	}
}

// Breakdown is what one fill cost, itemised.
type Breakdown struct {
	Brokerage   float64 `json:"brokerage"`
	STT         float64 `json:"stt"`
	ExchangeTxn float64 `json:"exchange_txn"`
	SEBI        float64 `json:"sebi"`
	StampDuty   float64 `json:"stamp_duty"`
	GST         float64 `json:"gst"`
	Total       float64 `json:"total"`
}

// Add accumulates another breakdown into this one.
func (b *Breakdown) Add(o Breakdown) {
	b.Brokerage += o.Brokerage
	b.STT += o.STT
	b.ExchangeTxn += o.ExchangeTxn
	b.SEBI += o.SEBI
	b.StampDuty += o.StampDuty
	b.GST += o.GST
	b.Total += o.Total
}

// Charge computes the cost of a single fill.
func (m Model) Charge(f broker.Fill) Breakdown {
	turnover := f.Price * float64(f.Quantity)
	if turnover <= 0 {
		return Breakdown{}
	}

	var c Breakdown

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

// CostOf returns just the total for a fill.
func (m Model) CostOf(f broker.Fill) float64 { return m.Charge(f).Total }
