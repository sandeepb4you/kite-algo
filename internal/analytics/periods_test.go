package analytics

import (
	"testing"
	"time"
)

func tradeAt(exit time.Time, net float64) Trade {
	return Trade{
		StrategyID: "s", TradingSymbol: "X", Quantity: 1,
		EntryTime: exit.Add(-time.Hour), ExitTime: exit,
		GrossPnL: net, NetPnL: net,
	}
}

func TestByPeriodGroupsWeekly(t *testing.T) {
	// Mon 10 Aug and Wed 12 Aug 2026 are the same ISO week; Mon 17 Aug is next.
	trades := []Trade{
		tradeAt(time.Date(2026, 8, 10, 15, 0, 0, 0, IST), 100),
		tradeAt(time.Date(2026, 8, 12, 15, 0, 0, 0, IST), 200),
		tradeAt(time.Date(2026, 8, 17, 15, 0, 0, 0, IST), -50),
	}

	got := ByPeriod(trades, PeriodWeekly, 100000, 0.06)
	if len(got) != 2 {
		t.Fatalf("got %d weekly buckets, want 2: %+v", len(got), got)
	}
	if got[0].Trades != 2 || got[0].Metrics.NetPnL != 300 {
		t.Errorf("week 1 = %d trades / %v net, want 2 / 300",
			got[0].Trades, got[0].Metrics.NetPnL)
	}
	if got[1].Trades != 1 || got[1].Metrics.NetPnL != -50 {
		t.Errorf("week 2 = %d trades / %v net, want 1 / -50",
			got[1].Trades, got[1].Metrics.NetPnL)
	}
}

func TestByPeriodGroupsMonthly(t *testing.T) {
	trades := []Trade{
		tradeAt(time.Date(2026, 7, 31, 15, 0, 0, 0, IST), 10),
		tradeAt(time.Date(2026, 8, 1, 15, 0, 0, 0, IST), 20),
		tradeAt(time.Date(2026, 8, 28, 15, 0, 0, 0, IST), 30),
	}

	got := ByPeriod(trades, PeriodMonthly, 100000, 0.06)
	if len(got) != 2 {
		t.Fatalf("got %d monthly buckets, want 2", len(got))
	}
	if got[0].Label != "July 2026" || got[1].Label != "August 2026" {
		t.Errorf("labels = %q, %q", got[0].Label, got[1].Label)
	}
	if got[1].Metrics.NetPnL != 50 {
		t.Errorf("August net = %v, want 50", got[1].Metrics.NetPnL)
	}
}

// Buckets must sort chronologically as plain strings, or the table renders out
// of order. A year boundary is where a naive "year + week number" key breaks:
// 29 Dec 2025 falls in ISO week 2026-W01.
func TestWeeklyKeysSortAcrossAYearBoundary(t *testing.T) {
	trades := []Trade{
		tradeAt(time.Date(2025, 12, 22, 15, 0, 0, 0, IST), 1), // 2025-W52
		tradeAt(time.Date(2025, 12, 30, 15, 0, 0, 0, IST), 2), // 2026-W01
		tradeAt(time.Date(2026, 1, 6, 15, 0, 0, 0, IST), 3),   // 2026-W02
	}

	got := ByPeriod(trades, PeriodWeekly, 100000, 0.06)
	if len(got) != 3 {
		t.Fatalf("got %d buckets, want 3: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Key >= got[i].Key {
			t.Errorf("keys out of order: %q then %q", got[i-1].Key, got[i].Key)
		}
		if !got[i-1].Start.Before(got[i].Start) {
			t.Errorf("bucket starts out of order: %v then %v", got[i-1].Start, got[i].Start)
		}
	}
	if got[1].Key != "2026-W01" {
		t.Errorf("30 Dec 2025 keyed as %q, want 2026-W01", got[1].Key)
	}
}

// Buckets are keyed on EXIT, because that is when the money is booked. Keying
// on entry would credit an overnight trade to the wrong period and the buckets
// would not sum to the total.
func TestByPeriodBucketsOnExitNotEntry(t *testing.T) {
	overnight := Trade{
		StrategyID: "s", TradingSymbol: "X", Quantity: 1,
		EntryTime: time.Date(2026, 8, 14, 15, 0, 0, 0, IST), // Friday, W33
		ExitTime:  time.Date(2026, 8, 17, 10, 0, 0, 0, IST), // Monday, W34
		NetPnL:    500,
	}

	got := ByPeriod([]Trade{overnight}, PeriodWeekly, 100000, 0.06)
	if len(got) != 1 {
		t.Fatalf("got %d buckets, want 1", len(got))
	}
	if got[0].Key != "2026-W34" {
		t.Errorf("bucketed as %q, want 2026-W34 (the exit week)", got[0].Key)
	}
}

// Weekly buckets must be the whole of the total, or the breakdown is lying.
func TestBucketNetSumsToTheWhole(t *testing.T) {
	trades := []Trade{
		tradeAt(time.Date(2026, 8, 10, 15, 0, 0, 0, IST), 100),
		tradeAt(time.Date(2026, 8, 12, 15, 0, 0, 0, IST), -40),
		tradeAt(time.Date(2026, 8, 17, 15, 0, 0, 0, IST), 25),
		tradeAt(time.Date(2026, 9, 2, 15, 0, 0, 0, IST), -5),
	}
	total := Compute(trades, 100000, 0.06).NetPnL

	for _, p := range []Period{PeriodDaily, PeriodWeekly, PeriodMonthly} {
		var sum float64
		var n int
		for _, b := range ByPeriod(trades, p, 100000, 0.06) {
			sum += b.Metrics.NetPnL
			n += b.Trades
		}
		if sum != total {
			t.Errorf("%s buckets sum to %v, total is %v", p, sum, total)
		}
		if n != len(trades) {
			t.Errorf("%s buckets hold %d trades, want %d", p, n, len(trades))
		}
	}
}

func TestByPeriodEmpty(t *testing.T) {
	if got := ByPeriod(nil, PeriodWeekly, 100000, 0.06); got != nil {
		t.Errorf("got %v, want nil for no trades", got)
	}
}

func TestParsePeriodDefaultsToWeekly(t *testing.T) {
	for _, in := range []string{"", "nonsense", "WEEKLY"} {
		if got := ParsePeriod(in); got != PeriodWeekly {
			t.Errorf("ParsePeriod(%q) = %q, want weekly", in, got)
		}
	}
	if got := ParsePeriod("monthly"); got != PeriodMonthly {
		t.Errorf("ParsePeriod(monthly) = %q", got)
	}
}
