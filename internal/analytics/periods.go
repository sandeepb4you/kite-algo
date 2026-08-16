package analytics

import (
	"fmt"
	"sort"
	"time"
)

// IST is the exchange timezone. Periods are exchange periods: a trade at
// 15:20 IST belongs to that trading day's week, not to whatever week the
// server's UTC clock says.
var IST = time.FixedZone("IST", 5*3600+30*60)

// Period is how trades are grouped for a performance breakdown.
type Period string

const (
	PeriodDaily   Period = "daily"
	PeriodWeekly  Period = "weekly"
	PeriodMonthly Period = "monthly"
)

// ParsePeriod resolves a period name, defaulting to weekly.
func ParsePeriod(s string) Period {
	switch Period(s) {
	case PeriodDaily:
		return PeriodDaily
	case PeriodMonthly:
		return PeriodMonthly
	default:
		return PeriodWeekly
	}
}

// PeriodSummary is one bucket's performance.
type PeriodSummary struct {
	// Key sorts chronologically as a string ("2026-W33", "2026-08"), which is
	// what makes a rendered table read in order without a second sort field.
	Key   string    `json:"key"`
	Label string    `json:"label"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`

	Metrics Metrics `json:"metrics"`
	Trades  int     `json:"trades"`
}

// ByPeriod groups trades into calendar buckets and computes each one's metrics.
//
// Buckets are keyed on EXIT time, not entry: a trade's P&L is realised when it
// closes, so an overnight position belongs to the period that booked the money.
// Grouping by entry would credit a Friday-to-Monday trade to the wrong week and
// make the weekly numbers not sum to the total.
//
// initialCapital is applied per bucket so return_pct is comparable between
// periods rather than compounding, which is what "how did last week do?" means.
func ByPeriod(trades []Trade, p Period, initialCapital, riskFreeRate float64) []PeriodSummary {
	if len(trades) == 0 {
		return nil
	}

	buckets := make(map[string][]Trade)
	bounds := make(map[string]TimeSpan)
	labels := make(map[string]string)

	for _, t := range trades {
		at := t.ExitTime
		if at.IsZero() {
			at = t.EntryTime
		}
		key, label, span := bucketFor(at, p)
		buckets[key] = append(buckets[key], t)
		bounds[key] = span
		labels[key] = label
	}

	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]PeriodSummary, 0, len(keys))
	for _, k := range keys {
		bt := buckets[k]
		out = append(out, PeriodSummary{
			Key:     k,
			Label:   labels[k],
			Start:   bounds[k].From,
			End:     bounds[k].To,
			Metrics: Compute(bt, initialCapital, riskFreeRate),
			Trades:  len(bt),
		})
	}
	return out
}

// TimeSpan is a bucket's half-open date range.
type TimeSpan struct {
	From time.Time
	To   time.Time
}

// bucketFor returns the sort key, display label and bounds of t's bucket.
func bucketFor(t time.Time, p Period) (key, label string, span TimeSpan) {
	local := t.In(IST)

	switch p {
	case PeriodDaily:
		start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, IST)
		return start.Format("2006-01-02"),
			start.Format("Mon 02 Jan 2006"),
			TimeSpan{From: start, To: start.AddDate(0, 0, 1)}

	case PeriodMonthly:
		start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, IST)
		return start.Format("2006-01"),
			start.Format("January 2006"),
			TimeSpan{From: start, To: start.AddDate(0, 1, 0)}

	default: // weekly
		// ISO weeks, so the key sorts correctly across a year boundary — the
		// week of 29 Dec 2025 is 2026-W01 and must sort after 2025-W52, which
		// a "year + week-of-year from the calendar date" key gets wrong.
		year, week := local.ISOWeek()
		start := isoWeekStart(local)
		return fmt.Sprintf("%04d-W%02d", year, week),
			fmt.Sprintf("W%02d %d · from %s", week, year, start.Format("02 Jan")),
			TimeSpan{From: start, To: start.AddDate(0, 0, 7)}
	}
}

// isoWeekStart returns the Monday of t's ISO week, at midnight IST.
func isoWeekStart(t time.Time) time.Time {
	local := t.In(IST)
	// Go's Weekday puts Sunday at 0; ISO weeks start on Monday.
	offset := (int(local.Weekday()) + 6) % 7
	day := local.AddDate(0, 0, -offset)
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, IST)
}
