package analytics

import (
	"math"
	"sort"
	"time"
)

// tradingDaysPerYear is the conventional annualization factor for Indian
// equities and F&O.
const tradingDaysPerYear = 252

// Metrics summarizes a set of trades.
//
// Conventions are documented per field because performance statistics are easy
// to compute in several defensible ways, and a number whose definition is
// unclear invites more confidence than it deserves.
type Metrics struct {
	GrossPnL   float64 `json:"gross_pnl"`
	TotalCosts float64 `json:"total_costs"`
	NetPnL     float64 `json:"net_pnl"`
	// ReturnPct is NetPnL over the initial capital. Margin is not modelled, so
	// treat it as a scale-free comparison between runs, not an account return.
	ReturnPct float64 `json:"return_pct"`

	TradeCount int     `json:"trade_count"`
	WinCount   int     `json:"win_count"`
	LossCount  int     `json:"loss_count"`
	WinRate    float64 `json:"win_rate"`

	AvgWin      float64 `json:"avg_win"`
	AvgLoss     float64 `json:"avg_loss"` // reported as a positive magnitude
	LargestWin  float64 `json:"largest_win"`
	LargestLoss float64 `json:"largest_loss"`
	// ProfitFactor is gross wins over gross losses. Zero losses would make it
	// infinite, so it is capped and flagged by ProfitFactorInfinite.
	ProfitFactor         float64 `json:"profit_factor"`
	ProfitFactorInfinite bool    `json:"profit_factor_infinite"`
	Expectancy           float64 `json:"expectancy"` // average net P&L per trade

	MaxDrawdown    float64 `json:"max_drawdown"`
	MaxDrawdownPct float64 `json:"max_drawdown_pct"`

	// Sharpe is annualized from DAILY returns of end-of-day equity, which is
	// the standard convention. It is zero when there are fewer than two trading
	// days, because a ratio computed from one observation is meaningless rather
	// than merely imprecise.
	Sharpe float64 `json:"sharpe"`

	TradingDays int     `json:"trading_days"`
	AvgDailyPnL float64 `json:"avg_daily_pnl"`
	BestDay     float64 `json:"best_day"`
	WorstDay    float64 `json:"worst_day"`

	MaxConsecutiveWins   int `json:"max_consecutive_wins"`
	MaxConsecutiveLosses int `json:"max_consecutive_losses"`

	AvgHolding time.Duration `json:"avg_holding"`
}

// Compute derives metrics from a trade ledger.
func Compute(trades []Trade, initialCapital, riskFreeRate float64) Metrics {
	var m Metrics
	m.TradeCount = len(trades)
	if len(trades) == 0 {
		return m
	}

	var (
		grossWins, grossLosses float64
		totalHolding           time.Duration
		streakWin, streakLoss  int
	)

	for _, t := range trades {
		m.GrossPnL += t.GrossPnL
		m.TotalCosts += t.Costs
		m.NetPnL += t.NetPnL
		totalHolding += t.Holding

		if t.IsWin() {
			m.WinCount++
			grossWins += t.NetPnL
			if t.NetPnL > m.LargestWin {
				m.LargestWin = t.NetPnL
			}
			streakWin++
			streakLoss = 0
			if streakWin > m.MaxConsecutiveWins {
				m.MaxConsecutiveWins = streakWin
			}
		} else {
			m.LossCount++
			grossLosses += abs(t.NetPnL)
			if abs(t.NetPnL) > m.LargestLoss {
				m.LargestLoss = abs(t.NetPnL)
			}
			streakLoss++
			streakWin = 0
			if streakLoss > m.MaxConsecutiveLosses {
				m.MaxConsecutiveLosses = streakLoss
			}
		}
	}

	m.WinRate = float64(m.WinCount) / float64(m.TradeCount) * 100
	m.Expectancy = m.NetPnL / float64(m.TradeCount)
	m.AvgHolding = totalHolding / time.Duration(m.TradeCount)

	if m.WinCount > 0 {
		m.AvgWin = grossWins / float64(m.WinCount)
	}
	if m.LossCount > 0 {
		m.AvgLoss = grossLosses / float64(m.LossCount)
	}

	switch {
	case grossLosses > 0:
		m.ProfitFactor = grossWins / grossLosses
	case grossWins > 0:
		// No losing trades at all. Reporting +Inf breaks JSON encoding and every
		// downstream format, so flag it and cap the number instead.
		m.ProfitFactorInfinite = true
		m.ProfitFactor = math.Inf(1)
	}
	if math.IsInf(m.ProfitFactor, 1) {
		m.ProfitFactor = 0 // the flag carries the meaning
	}

	if initialCapital > 0 {
		m.ReturnPct = m.NetPnL / initialCapital * 100
	}

	curve := BuildEquityCurve(trades, initialCapital)
	m.MaxDrawdown = maxDrawdown(curve)
	if peak := peakEquity(curve, initialCapital); peak > 0 {
		m.MaxDrawdownPct = m.MaxDrawdown / peak * 100
	}

	daily := dailyPnL(trades)
	m.TradingDays = len(daily)
	if m.TradingDays > 0 {
		var sum float64
		m.BestDay, m.WorstDay = daily[0].pnl, daily[0].pnl
		for _, d := range daily {
			sum += d.pnl
			if d.pnl > m.BestDay {
				m.BestDay = d.pnl
			}
			if d.pnl < m.WorstDay {
				m.WorstDay = d.pnl
			}
		}
		m.AvgDailyPnL = sum / float64(m.TradingDays)
	}
	m.Sharpe = sharpe(daily, initialCapital, riskFreeRate)

	return m
}

type dayPnL struct {
	day string
	pnl float64
}

// dailyPnL buckets realized P&L by the exit date, in IST.
func dailyPnL(trades []Trade) []dayPnL {
	ist := time.FixedZone("IST", 5*3600+30*60)
	byDay := make(map[string]float64)
	for _, t := range trades {
		byDay[t.ExitTime.In(ist).Format("2006-01-02")] += t.NetPnL
	}

	out := make([]dayPnL, 0, len(byDay))
	for d, v := range byDay {
		out = append(out, dayPnL{day: d, pnl: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].day < out[j].day })
	return out
}

// sharpe annualizes the ratio of mean daily return to its standard deviation.
func sharpe(daily []dayPnL, capital, riskFreeRate float64) float64 {
	// One observation has no dispersion; a "ratio" from it is noise dressed as a
	// statistic. Report zero rather than something impressive and meaningless.
	if len(daily) < 2 || capital <= 0 {
		return 0
	}

	returns := make([]float64, len(daily))
	for i, d := range daily {
		returns[i] = d.pnl / capital
	}

	var mean float64
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))

	var variance float64
	for _, r := range returns {
		variance += (r - mean) * (r - mean)
	}
	variance /= float64(len(returns) - 1) // sample variance
	sd := math.Sqrt(variance)
	if sd == 0 {
		return 0
	}

	dailyRF := riskFreeRate / tradingDaysPerYear
	return (mean - dailyRF) / sd * math.Sqrt(tradingDaysPerYear)
}

func maxDrawdown(curve []EquityPoint) float64 {
	var worst float64
	for _, p := range curve {
		if p.Drawdown > worst {
			worst = p.Drawdown
		}
	}
	return worst
}

func peakEquity(curve []EquityPoint, initial float64) float64 {
	peak := initial
	for _, p := range curve {
		if p.Equity > peak {
			peak = p.Equity
		}
	}
	return peak
}
