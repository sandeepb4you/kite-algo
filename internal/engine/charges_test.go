package engine

import (
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/charges"
)

func chargeEngine() *Engine {
	e := newTestEngine()
	e.costModel = charges.DefaultNSEOptions()
	return e
}

func sellFill(qty int, price float64, at time.Time) broker.Fill {
	return broker.Fill{
		TradingSymbol: "NIFTY25AUG24500CE", Side: broker.SideSell,
		Quantity: qty, Price: price, Timestamp: at,
	}
}

// TestChargesAccrueAcrossFills checks the running total is the sum of the
// individual fills, so the header figure matches what the trades actually cost.
func TestChargesAccrueAcrossFills(t *testing.T) {
	e := chargeEngine()
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, ist)

	one := e.costModel.Charge(sellFill(75, 100, now)).Total
	if one <= 0 {
		t.Fatal("the cost model charged nothing for a real fill")
	}

	e.accrueCharges(sellFill(75, 100, now))
	e.accrueCharges(sellFill(75, 100, now.Add(time.Minute)))

	got := e.DayCharges()
	if want := one * 2; got.Total < want-0.01 || got.Total > want+0.01 {
		t.Errorf("total = %.4f, want %.4f (two identical fills)", got.Total, want)
	}
	if got.Brokerage <= 0 || got.STT <= 0 || got.GST <= 0 {
		t.Errorf("breakdown is incomplete: %+v", got)
	}
}

// TestChargesResetEachTradingDay stops the total growing for the lifetime of a
// long-running process — a server left up over a week would otherwise report a
// week of charges as today's.
func TestChargesResetEachTradingDay(t *testing.T) {
	e := chargeEngine()
	day1 := time.Date(2026, 8, 14, 10, 0, 0, 0, ist)

	e.accrueCharges(sellFill(75, 100, day1))
	first := e.DayCharges().Total
	if first <= 0 {
		t.Fatal("no charges accrued on day one")
	}

	e.accrueCharges(sellFill(75, 100, day1.AddDate(0, 0, 1)))

	second := e.DayCharges().Total
	if second >= first*2 {
		t.Errorf("total = %.4f; a new trading day must start from zero, not carry "+
			"yesterday forward", second)
	}
	if second <= 0 {
		t.Error("the new day's own fill was not counted")
	}
}

// TestChargesUseISTDayBoundary: a fill at 15:20 IST is 09:50 UTC, so bucketing
// by UTC would split a single trading session across two days.
func TestChargesUseISTDayBoundary(t *testing.T) {
	e := chargeEngine()
	morning := time.Date(2026, 8, 14, 9, 30, 0, 0, ist)
	afternoon := time.Date(2026, 8, 14, 15, 20, 0, 0, ist)

	e.accrueCharges(sellFill(75, 100, morning))
	one := e.DayCharges().Total
	e.accrueCharges(sellFill(75, 100, afternoon))
	two := e.DayCharges().Total

	if two <= one {
		t.Error("the afternoon fill was not added to the same session's total")
	}
}

// TestBuyAndSellChargeDifferently guards the one-sided levies: STT is charged on
// the sell side, stamp duty on the buy side. Applying either to both would
// roughly double that component.
func TestBuyAndSellChargeDifferently(t *testing.T) {
	m := charges.DefaultNSEOptions()
	now := time.Now()

	sell := m.Charge(broker.Fill{Side: broker.SideSell, Quantity: 75, Price: 100, Timestamp: now})
	buy := m.Charge(broker.Fill{Side: broker.SideBuy, Quantity: 75, Price: 100, Timestamp: now})

	if sell.STT <= 0 {
		t.Error("no STT on a sell")
	}
	if buy.STT != 0 {
		t.Errorf("STT = %.4f on a buy, want 0", buy.STT)
	}
	if buy.StampDuty <= 0 {
		t.Error("no stamp duty on a buy")
	}
	if sell.StampDuty != 0 {
		t.Errorf("stamp duty = %.4f on a sell, want 0", sell.StampDuty)
	}
}
