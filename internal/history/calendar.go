package history

import (
	"time"

	"kite-algo/internal/storage"
)

// IST is the exchange timezone.
var IST = time.FixedZone("IST", 5*3600+30*60)

// Session bounds for NSE equity and F&O, in IST.
const (
	sessionOpenHour   = 9
	sessionOpenMinute = 15
	sessionCloseHour  = 15
	sessionCloseMin   = 30
)

// Calendar knows when the exchange is open.
//
// It is used to avoid requesting historical data for times the market was shut.
// Those requests always return nothing, but they still consume the rate limit
// and the historical-data quota — and a month-long backtest window contains
// eight or nine non-trading days.
type Calendar struct {
	// holidays are exchange holidays keyed as YYYY-MM-DD in IST.
	holidays map[string]bool
}

// NSE returns a calendar for the National Stock Exchange.
//
// Weekends are handled structurally. Holidays must be loaded explicitly — the
// list changes annually and is not derivable, so an unloaded calendar simply
// treats holidays as trading days and fetches a few empty windows, which is
// wasteful but never wrong.
func NSE() *Calendar {
	return &Calendar{holidays: map[string]bool{}}
}

// SetHolidays replaces the holiday set. Dates are YYYY-MM-DD in IST.
func (c *Calendar) SetHolidays(dates []string) {
	c.holidays = make(map[string]bool, len(dates))
	for _, d := range dates {
		c.holidays[d] = true
	}
}

// IsTradingDay reports whether the exchange trades on t's calendar day.
func (c *Calendar) IsTradingDay(t time.Time) bool {
	local := t.In(IST)
	switch local.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	return !c.holidays[local.Format("2006-01-02")]
}

// SessionFor returns the trading session on t's day, and whether there is one.
func (c *Calendar) SessionFor(t time.Time) (storage.TimeRange, bool) {
	if !c.IsTradingDay(t) {
		return storage.TimeRange{}, false
	}
	local := t.In(IST)
	open := time.Date(local.Year(), local.Month(), local.Day(),
		sessionOpenHour, sessionOpenMinute, 0, 0, IST)
	close := time.Date(local.Year(), local.Month(), local.Day(),
		sessionCloseHour, sessionCloseMin, 0, 0, IST)
	return storage.TimeRange{From: open, To: close}, true
}

// MostRecentTradingDay returns the latest trading day at or before t.
//
// A manual "capture now" pressed on a Sunday must not target Sunday: capture
// skips non-trading days, so the button would appear to work and do nothing.
// The day the operator means is the last one that traded. Bounded to a fortnight
// so an unbroken run of configured holidays cannot loop forever.
func (c *Calendar) MostRecentTradingDay(t time.Time) (time.Time, bool) {
	day := t.In(IST)
	for i := 0; i < 14; i++ {
		if c.IsTradingDay(day) {
			return day, true
		}
		day = day.AddDate(0, 0, -1)
	}
	return time.Time{}, false
}

// TradingWindows splits a range into the portions the exchange was open for.
func (c *Calendar) TradingWindows(r storage.TimeRange) []storage.TimeRange {
	var out []storage.TimeRange

	day := time.Date(r.From.In(IST).Year(), r.From.In(IST).Month(), r.From.In(IST).Day(),
		0, 0, 0, 0, IST)

	for !day.After(r.To) {
		session, ok := c.SessionFor(day)
		if ok {
			clipped, valid := intersect(r, session)
			if valid {
				out = append(out, clipped)
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return out
}

// ClosedWindows returns the portions of a range the exchange was shut for — the
// complement of TradingWindows within r.
func (c *Calendar) ClosedWindows(r storage.TimeRange) []storage.TimeRange {
	open := c.TradingWindows(r)
	if len(open) == 0 {
		return []storage.TimeRange{r}
	}
	return Subtract(r, open)
}

// intersect clips a to b, reporting whether anything remains.
func intersect(a, b storage.TimeRange) (storage.TimeRange, bool) {
	from, to := a.From, a.To
	if b.From.After(from) {
		from = b.From
	}
	if b.To.Before(to) {
		to = b.To
	}
	if !to.After(from) {
		return storage.TimeRange{}, false
	}
	return storage.TimeRange{From: from, To: to}, true
}
