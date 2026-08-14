package backtest

import (
	"math"
	"testing"

	"kite-algo/internal/broker"
)

// TestSellSideChargesAreHandComputable pins the option-selling charge stack.
//
// Worked example — sell 75 × ₹100 premium = ₹7,500 turnover:
//
//	brokerage    ₹20.00              flat
//	STT           ₹7.50              0.1%     of 7,500  (sell side only)
//	exchange txn  ₹2.63              0.03503% of 7,500
//	SEBI          ₹0.0075            0.0001%  of 7,500
//	stamp duty    ₹0.00              buy side only
//	GST           ₹4.0727            18% of (20 + 2.6273 + 0.0075)
//	                        total ≈ ₹34.21
//
// Cross-check against Zerodha's brokerage calculator before trusting a backtest;
// Indian statutory rates change with every budget.
func TestSellSideChargesAreHandComputable(t *testing.T) {
	m := DefaultNSEOptionCosts()
	c := m.Charge(broker.Fill{Side: broker.SideSell, Quantity: 75, Price: 100})

	expect := func(name string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 0.01 {
			t.Errorf("%s = %.4f, want %.4f", name, got, want)
		}
	}

	expect("brokerage", c.Brokerage, 20)
	expect("STT", c.STT, 7.50)
	expect("exchange txn", c.ExchangeTxn, 2.6273)
	expect("SEBI", c.SEBI, 0.0075)
	expect("GST", c.GST, 4.0727)

	if c.StampDuty != 0 {
		t.Errorf("stamp duty = %.4f on a SELL, want 0 (buy side only)", c.StampDuty)
	}
	expect("total", c.Total, 34.2075)
}

// TestBuySideSwapsSTTForStampDuty checks the two one-sided charges are applied
// to the correct side. Applying either to both would roughly double it.
func TestBuySideSwapsSTTForStampDuty(t *testing.T) {
	m := DefaultNSEOptionCosts()
	c := m.Charge(broker.Fill{Side: broker.SideBuy, Quantity: 75, Price: 100})

	if c.STT != 0 {
		t.Errorf("STT = %.4f on a BUY, want 0 (sell side only)", c.STT)
	}
	if want := 7500 * 0.00003; math.Abs(c.StampDuty-want) > 0.0001 {
		t.Errorf("stamp duty = %.5f, want %.5f", c.StampDuty, want)
	}
}

// TestRoundTripCostIsMaterial is the sanity check behind modelling costs at all:
// a two-leg straddle in and out is meaningful money against a daily P&L target.
func TestRoundTripCostIsMaterial(t *testing.T) {
	m := DefaultNSEOptionCosts()

	// Sell two legs at ₹100, buy both back at ₹80.
	total := m.CostOf(broker.Fill{Side: broker.SideSell, Quantity: 75, Price: 100}) * 2
	total += m.CostOf(broker.Fill{Side: broker.SideBuy, Quantity: 75, Price: 80}) * 2

	if total < 50 {
		t.Errorf("round-trip cost = ₹%.2f; suspiciously low for four F&O legs", total)
	}
	t.Logf("straddle round trip costs ₹%.2f in charges", total)
}

func TestZeroCostModelCharges(t *testing.T) {
	var m CostModel
	if c := m.Charge(broker.Fill{Side: broker.SideBuy, Quantity: 75, Price: 100}); c.Total != 0 {
		t.Errorf("empty cost model charged %.4f, want 0", c.Total)
	}
}

func TestChargeIgnoresZeroTurnover(t *testing.T) {
	m := DefaultNSEOptionCosts()
	if c := m.Charge(broker.Fill{Side: broker.SideBuy, Quantity: 0, Price: 100}); c.Total != 0 {
		t.Errorf("zero-quantity fill charged %.4f, want 0", c.Total)
	}
}

// TestSlippageIsAlwaysAdverse is the directional property. Slippage that helped
// the trader would make a backtest optimistic in exactly the way slippage exists
// to prevent.
func TestSlippageIsAlwaysAdverse(t *testing.T) {
	m := DefaultNSEOptionCosts() // 1 tick of ₹0.05
	fm := SlippageFillModel{Model: m}

	buy := &broker.Order{OrderType: broker.OrderTypeMarket, Side: broker.SideBuy}
	if got := fm.FillPrice(buy, 100); got <= 100 {
		t.Errorf("buy filled at %.4f, want worse than the 100 quote", got)
	}

	sell := &broker.Order{OrderType: broker.OrderTypeMarket, Side: broker.SideSell}
	if got := fm.FillPrice(sell, 100); got >= 100 {
		t.Errorf("sell filled at %.4f, want worse than the 100 quote", got)
	}
}

// TestLimitOrdersDoNotSlip: a limit order fills at its limit or better by
// definition, so applying slippage would misrepresent how limits work.
func TestLimitOrdersDoNotSlip(t *testing.T) {
	fm := SlippageFillModel{Model: DefaultNSEOptionCosts()}
	o := &broker.Order{OrderType: broker.OrderTypeLimit, Side: broker.SideBuy, Price: 99}
	if got := fm.FillPrice(o, 95); got != 99 {
		t.Errorf("limit order filled at %.4f, want its limit price 99", got)
	}
}

// TestSlippageNeverGoesNegative covers a deep-OTM option quoted near zero.
func TestSlippageNeverGoesNegative(t *testing.T) {
	m := DefaultNSEOptionCosts()
	m.SlippageTicks = 100 // absurd, to force the floor
	fm := SlippageFillModel{Model: m}

	o := &broker.Order{OrderType: broker.OrderTypeMarket, Side: broker.SideSell}
	if got := fm.FillPrice(o, 0.05); got <= 0 {
		t.Errorf("fill price = %.4f; a price can never be zero or negative", got)
	}
}
