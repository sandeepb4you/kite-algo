package analytics

import (
	"math"
	"testing"
	"time"

	"kite-algo/internal/broker"
)

var base = time.Date(2024, 8, 1, 9, 15, 0, 0, time.FixedZone("IST", 5*3600+30*60))

func fill(sym string, side broker.Side, qty int, price float64, at time.Time) broker.Fill {
	return broker.Fill{
		StrategyID: "s", TradingSymbol: sym, Side: side,
		Quantity: qty, Price: price, Timestamp: at,
	}
}

func TestBuildTradesLongRoundTrip(t *testing.T) {
	fills := []broker.Fill{
		fill("X", broker.SideBuy, 75, 100, base),
		fill("X", broker.SideSell, 75, 110, base.Add(time.Hour)),
	}

	trades := BuildTrades(fills, nil)
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	tr := trades[0]
	if tr.Direction != broker.SideBuy {
		t.Errorf("direction = %s, want BUY", tr.Direction)
	}
	if want := 10.0 * 75; tr.GrossPnL != want {
		t.Errorf("gross P&L = %.2f, want %.2f", tr.GrossPnL, want)
	}
	if tr.Holding != time.Hour {
		t.Errorf("holding = %s, want 1h", tr.Holding)
	}
}

// TestBuildTradesShortRoundTrip checks the sign convention for shorts, which is
// the whole basis of this platform's sample strategy: selling first and buying
// back lower is a profit.
func TestBuildTradesShortRoundTrip(t *testing.T) {
	fills := []broker.Fill{
		fill("X", broker.SideSell, 75, 120, base),
		fill("X", broker.SideBuy, 75, 100, base.Add(time.Hour)),
	}

	trades := BuildTrades(fills, nil)
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	if trades[0].Direction != broker.SideSell {
		t.Errorf("direction = %s, want SELL", trades[0].Direction)
	}
	if want := 20.0 * 75; trades[0].GrossPnL != want {
		t.Errorf("short P&L = %.2f, want %.2f — a short that fell must profit",
			trades[0].GrossPnL, want)
	}
}

// TestBuildTradesMatchesFIFO covers scaling in and out. The total is unaffected
// by match order, but the per-trade distribution — win rate, average win,
// holding periods — is not.
func TestBuildTradesMatchesFIFO(t *testing.T) {
	fills := []broker.Fill{
		fill("X", broker.SideBuy, 50, 100, base),
		fill("X", broker.SideBuy, 50, 110, base.Add(time.Minute)),
		fill("X", broker.SideSell, 50, 120, base.Add(2*time.Minute)),
		fill("X", broker.SideSell, 50, 130, base.Add(3*time.Minute)),
	}

	trades := BuildTrades(fills, nil)
	if len(trades) != 2 {
		t.Fatalf("got %d trades, want 2", len(trades))
	}
	// FIFO: the first sell closes the 100 lot, not the 110 lot.
	if trades[0].EntryPrice != 100 {
		t.Errorf("first trade entry = %.2f, want 100 (FIFO)", trades[0].EntryPrice)
	}
	if trades[1].EntryPrice != 110 {
		t.Errorf("second trade entry = %.2f, want 110", trades[1].EntryPrice)
	}
}

func TestBuildTradesPartialClose(t *testing.T) {
	fills := []broker.Fill{
		fill("X", broker.SideBuy, 100, 100, base),
		fill("X", broker.SideSell, 40, 110, base.Add(time.Minute)),
	}
	trades := BuildTrades(fills, nil)
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	if trades[0].Quantity != 40 {
		t.Errorf("quantity = %d, want the 40 that actually closed", trades[0].Quantity)
	}
}

func TestBuildTradesSeparatesSymbols(t *testing.T) {
	fills := []broker.Fill{
		fill("CE", broker.SideSell, 75, 120, base),
		fill("PE", broker.SideSell, 75, 100, base),
		fill("CE", broker.SideBuy, 75, 100, base.Add(time.Hour)),
		fill("PE", broker.SideBuy, 75, 90, base.Add(time.Hour)),
	}
	trades := BuildTrades(fills, nil)
	if len(trades) != 2 {
		t.Fatalf("got %d trades, want 2 (one per leg)", len(trades))
	}
	for _, tr := range trades {
		if tr.NetPnL <= 0 {
			t.Errorf("%s: both legs fell and should profit, got %.2f", tr.TradingSymbol, tr.NetPnL)
		}
	}
}

// TestBuildTradesAppliesCosts confirms charges reach the ledger and reduce net.
func TestBuildTradesAppliesCosts(t *testing.T) {
	fills := []broker.Fill{
		fill("X", broker.SideBuy, 75, 100, base),
		fill("X", broker.SideSell, 75, 110, base.Add(time.Hour)),
	}
	trades := BuildTrades(fills, func(broker.Fill) float64 { return 20 })

	tr := trades[0]
	if tr.Costs != 40 {
		t.Errorf("costs = %.2f, want 40 (₹20 on each of two fills)", tr.Costs)
	}
	if tr.NetPnL != tr.GrossPnL-40 {
		t.Errorf("net %.2f != gross %.2f - costs %.2f", tr.NetPnL, tr.GrossPnL, tr.Costs)
	}
}

// --- metrics --------------------------------------------------------------

func mkTrade(seq int, net float64, exit time.Time) Trade {
	return Trade{Seq: seq, NetPnL: net, GrossPnL: net, ExitTime: exit, EntryTime: exit.Add(-time.Hour)}
}

func TestComputeBasicMetrics(t *testing.T) {
	trades := []Trade{
		mkTrade(1, 100, base),
		mkTrade(2, -50, base.AddDate(0, 0, 1)),
		mkTrade(3, 200, base.AddDate(0, 0, 2)),
		mkTrade(4, -25, base.AddDate(0, 0, 3)),
	}

	m := Compute(trades, 100000, 0.06)

	if m.TradeCount != 4 {
		t.Errorf("trade count = %d, want 4", m.TradeCount)
	}
	if m.WinCount != 2 || m.LossCount != 2 {
		t.Errorf("wins/losses = %d/%d, want 2/2", m.WinCount, m.LossCount)
	}
	if m.WinRate != 50 {
		t.Errorf("win rate = %.1f, want 50", m.WinRate)
	}
	if want := 225.0; m.NetPnL != want {
		t.Errorf("net P&L = %.2f, want %.2f", m.NetPnL, want)
	}
	if want := 150.0; m.AvgWin != want {
		t.Errorf("avg win = %.2f, want %.2f", m.AvgWin, want)
	}
	if want := 37.5; m.AvgLoss != want {
		t.Errorf("avg loss = %.2f, want %.2f (a positive magnitude)", m.AvgLoss, want)
	}
	if want := 300.0 / 75.0; math.Abs(m.ProfitFactor-want) > 1e-9 {
		t.Errorf("profit factor = %.4f, want %.4f", m.ProfitFactor, want)
	}
	if want := 56.25; m.Expectancy != want {
		t.Errorf("expectancy = %.2f, want %.2f", m.Expectancy, want)
	}
}

// TestProfitFactorWithNoLosses covers the divide-by-zero that would otherwise
// serialize as +Inf and break every downstream format.
func TestProfitFactorWithNoLosses(t *testing.T) {
	m := Compute([]Trade{mkTrade(1, 100, base), mkTrade(2, 50, base)}, 100000, 0.06)
	if !m.ProfitFactorInfinite {
		t.Error("a run with no losing trades should flag an infinite profit factor")
	}
	if math.IsInf(m.ProfitFactor, 0) || math.IsNaN(m.ProfitFactor) {
		t.Errorf("profit factor = %v; must stay JSON-encodable", m.ProfitFactor)
	}
}

// TestMaxDrawdown checks the peak-to-trough measure on a curve that recovers.
func TestMaxDrawdown(t *testing.T) {
	trades := []Trade{
		mkTrade(1, 1000, base),                   // equity 101000, peak 101000
		mkTrade(2, -3000, base.AddDate(0, 0, 1)), // equity 98000, drawdown 3000
		mkTrade(3, 500, base.AddDate(0, 0, 2)),   // equity 98500, drawdown 2500
	}
	m := Compute(trades, 100000, 0.06)

	if m.MaxDrawdown != 3000 {
		t.Errorf("max drawdown = %.2f, want 3000", m.MaxDrawdown)
	}
	if want := 3000.0 / 101000 * 100; math.Abs(m.MaxDrawdownPct-want) > 1e-9 {
		t.Errorf("max drawdown %% = %.4f, want %.4f (against the peak)", m.MaxDrawdownPct, want)
	}
}

// TestSharpeNeedsTwoDays guards against reporting an impressive-looking ratio
// derived from a single observation.
func TestSharpeNeedsTwoDays(t *testing.T) {
	oneDay := Compute([]Trade{mkTrade(1, 500, base)}, 100000, 0.06)
	if oneDay.Sharpe != 0 {
		t.Errorf("Sharpe = %.4f from one trading day, want 0", oneDay.Sharpe)
	}

	multi := Compute([]Trade{
		mkTrade(1, 500, base),
		mkTrade(2, 300, base.AddDate(0, 0, 1)),
		mkTrade(3, -200, base.AddDate(0, 0, 2)),
	}, 100000, 0.06)
	if multi.Sharpe == 0 {
		t.Error("Sharpe should be computable across three trading days")
	}
}

func TestConsecutiveStreaks(t *testing.T) {
	trades := []Trade{
		mkTrade(1, 10, base), mkTrade(2, 10, base), mkTrade(3, 10, base),
		mkTrade(4, -5, base), mkTrade(5, -5, base),
		mkTrade(6, 10, base),
	}
	m := Compute(trades, 100000, 0.06)
	if m.MaxConsecutiveWins != 3 {
		t.Errorf("max consecutive wins = %d, want 3", m.MaxConsecutiveWins)
	}
	if m.MaxConsecutiveLosses != 2 {
		t.Errorf("max consecutive losses = %d, want 2", m.MaxConsecutiveLosses)
	}
}

func TestComputeEmpty(t *testing.T) {
	m := Compute(nil, 100000, 0.06)
	if m.TradeCount != 0 || m.NetPnL != 0 || m.Sharpe != 0 {
		t.Errorf("empty ledger produced %+v", m)
	}
}

// TestDailyBucketingUsesIST matters because a trade closing at 15:30 IST is
// 10:00 UTC — bucketing by UTC would split a single trading day in two and
// distort every daily statistic.
func TestDailyBucketingUsesIST(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+30*60)
	morning := time.Date(2024, 8, 1, 9, 30, 0, 0, ist)
	afternoon := time.Date(2024, 8, 1, 15, 20, 0, 0, ist)

	m := Compute([]Trade{mkTrade(1, 100, morning), mkTrade(2, 200, afternoon)}, 100000, 0.06)
	if m.TradingDays != 1 {
		t.Errorf("trading days = %d, want 1 — both trades closed on the same IST day", m.TradingDays)
	}
	if m.BestDay != 300 {
		t.Errorf("best day = %.2f, want 300 (both trades combined)", m.BestDay)
	}
}
