package options

import (
	"strconv"
	"strings"
	"time"
)

// Spec is the parsed form of an Indian option trading symbol.
//
// Note: NSE has changed its symbol format several times (monthly abbreviations
// vs numeric weekly dates). The strike and option type parse reliably; the
// expiry is best-effort. For the authoritative expiry/strike, always prefer the
// Kite instrument master (kite.Instruments).
type Spec struct {
	Underlying string  // NIFTY, BANKNIFTY, FINNIFTY, ...
	Strike     float64 // 24500
	Type       OptionType
	ExpiryCode string    // raw middle token, e.g. "24AUG" or "24815"
	Expiry     time.Time // best-effort parsed expiry date (zero if unknown)
}

// ParseSymbol parses a trading symbol like "NIFTY24AUG24500CE" into a Spec.
// Returns ok=false if it doesn't look like an option symbol.
func ParseSymbol(symbol string) (Spec, bool) {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if len(s) < 6 {
		return Spec{}, false
	}

	// Type is always the last two chars (CE/PE).
	typeSuffix := s[len(s)-2:]
	optType, ok := ParseOptionType(typeSuffix)
	if !ok {
		return Spec{}, false
	}
	body := s[:len(s)-2]

	// Split the underlying off FIRST, then take the expiry code, then the
	// strike — rather than grabbing trailing digits and hoping.
	//
	// Trailing digits do not work on a date-coded weekly. In
	// NIFTY2681824350CE the expiry (26818) and the strike (24350) are adjacent
	// digits with nothing between them, so scanning back from the end consumed
	// both and produced a strike of 2,681,824,350 and no expiry at all. It only
	// ever worked on the older monthly form, where letters happen to separate
	// them (NIFTY24AUG24500CE).
	//
	// Kite's expiry code is always FIVE characters, in every form it uses:
	//
	//	26818  weekly, Jan-Sep     (YY M DD)
	//	26O15  weekly, Oct-Dec     (YY letter DD)
	//	26AUG  monthly             (YY MMM)
	//
	// so once the underlying is off the front, the split is unambiguous.
	underlying, rest := splitUnderlying(body)

	const expiryCodeLen = 5
	if underlying != "" && len(rest) > expiryCodeLen {
		code := rest[:expiryCodeLen]
		if exp := parseExpiryCode(code); !exp.IsZero() {
			if strike, err := strconv.ParseFloat(rest[expiryCodeLen:], 64); err == nil {
				return Spec{
					Underlying: underlying,
					Strike:     strike,
					Type:       optType,
					ExpiryCode: code,
					Expiry:     exp,
				}, true
			}
		}
	}

	// Fallback for anything that does not fit that shape: trailing digits are
	// the strike, whatever precedes them is underlying + expiry. Keeps unusual
	// or future symbol formats parsing at least partially rather than not at
	// all.
	i := len(body)
	for i > 0 && body[i-1] >= '0' && body[i-1] <= '9' {
		i--
	}
	if i == len(body) {
		return Spec{}, false // no digits → not an option symbol
	}
	strike, err := strconv.ParseFloat(body[i:], 64)
	if err != nil {
		return Spec{}, false
	}
	underlying, expiryCode := splitUnderlying(body[:i])

	return Spec{
		Underlying: underlying,
		Strike:     strike,
		Type:       optType,
		ExpiryCode: expiryCode,
		Expiry:     parseExpiryCode(expiryCode),
	}, true
}

// knownUnderlyings lists index/equity symbols whose option contracts trade on
// NSE/BSE, longest-first so "BANKNIFTY" matches before "NIFTY".
var knownUnderlyings = []string{
	"MIDCPNIFTY", "BANKNIFTY", "FINNIFTY", "NIFTY", "SENSEX", "BANKEX",
	"RELIANCE", "HDFCBANK", "INFY", "TCS", "ICICIBANK",
}

// splitUnderlying peels a known underlying off the front of prefix, returning
// the underlying and the remaining expiry code.
func splitUnderlying(prefix string) (string, string) {
	for _, u := range knownUnderlyings {
		if strings.HasPrefix(prefix, u) {
			return u, prefix[len(u):]
		}
	}
	// Fallback: assume the leading letters (up to the first digit) are the
	// underlying.
	i := 0
	for i < len(prefix) && (prefix[i] < '0' || prefix[i] > '9') {
		i++
	}
	return prefix[:i], prefix[i:]
}

// parseExpiryCode best-effort parses the middle expiry token.
// Recognized forms:
//   - "YYMMM"  : 24AUG → Aug 2024 (old monthly format)
//   - "YYMMDD" : 24815 → 2024-08-15 (numeric weekly format)
//   - "YYM"    : 248 (rare)
//
// Returns the zero time if it can't parse.
func parseExpiryCode(code string) time.Time {
	code = strings.TrimSpace(code)
	if code == "" {
		return time.Time{}
	}
	// Numeric form: YYMM, YYMDD (weekly) or YYMMDD.
	if isAllDigits(code) {
		return parseNumericExpiry(code)
	}
	// Weekly expiries in Oct/Nov/Dec use a LETTER for the month, because a
	// two-digit month would make the code ambiguous with the day: NIFTY26O1524350CE
	// is 15 October 2026. Handled before the monthly form, which expects three
	// letters ("26OCT").
	if t := parseLetterMonthWeekly(code); !t.IsZero() {
		return t
	}
	// Mixed form like "24AUG".
	return parseMonthExpiry(code)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseNumericExpiry(code string) time.Time {
	// First two digits = year (20xx).
	if len(code) < 3 {
		return time.Time{}
	}
	year := 2000 + atoiSafe(code[:2])
	rest := code[2:]
	switch len(rest) {
	case 1, 2: // month only → last Thursday of that month
		m := int(atoiSafe(rest))
		return lastThursday(year, time.Month(m))
	case 3:
		// WEEKLY: one month digit then the day, e.g. "26818" = 18 Aug 2026.
		// This is the commonest NFO option symbol there is, and it was falling
		// through to "unknown" — which silently excluded almost every weekly
		// contract from anything keyed on expiry.
		//
		// Only months 1-9 appear here; Oct/Nov/Dec weeklies use a letter
		// instead, precisely because "1015" could not be told from "115".
		m := int(atoiSafe(rest[:1]))
		d := int(atoiSafe(rest[1:]))
		if m < 1 || m > 9 || d < 1 || d > 31 {
			return time.Time{}
		}
		return time.Date(year, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	case 4: // MMDD
		m := int(atoiSafe(rest[:2]))
		d := int(atoiSafe(rest[2:]))
		return time.Date(year, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	}
	return time.Time{}
}

func parseMonthExpiry(code string) time.Time {
	if len(code) < 4 {
		return time.Time{}
	}
	year := 2000 + atoiSafe(code[:2])
	monStr := code[2:]
	monStr = strings.ToUpper(monStr)
	months := map[string]time.Month{
		"JAN": time.January, "FEB": time.February, "MAR": time.March,
		"APR": time.April, "MAY": time.May, "JUN": time.June,
		"JUL": time.July, "AUG": time.August, "SEP": time.September,
		"OCT": time.October, "NOV": time.November, "DEC": time.December,
	}
	m, ok := months[monStr]
	if !ok {
		return time.Time{}
	}
	// Monthly options expire on the last Thursday of the month.
	return lastThursday(year, m)
}

// lastThursday returns the date of the last Thursday in the given year/month.
// NSE equity/index monthly expiries settle on the last Thursday (Friday if
// Thursday is a holiday, but we don't track the holiday calendar here).
func lastThursday(year int, m time.Month) time.Time {
	if m < 1 || m > 12 {
		return time.Time{}
	}
	// Start from the last day of the month and walk back to Thursday (weekday 4).
	firstNext := time.Date(year, m+1, 1, 0, 0, 0, 0, time.UTC)
	for d := firstNext.AddDate(0, 0, -1); d.Month() == m; d = d.AddDate(0, 0, -1) {
		if d.Weekday() == time.Thursday {
			return d
		}
	}
	return time.Time{}
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// parseLetterMonthWeekly parses "26O15" — year, a single letter for
// October/November/December, then the day.
//
// Returns the zero time for anything else, so the monthly parser still gets
// "26OCT" and friends.
func parseLetterMonthWeekly(code string) time.Time {
	if len(code) != 5 {
		return time.Time{}
	}
	if !isAllDigits(code[:2]) || !isAllDigits(code[3:]) {
		return time.Time{}
	}
	var month time.Month
	switch code[2] {
	case 'O', 'o':
		month = time.October
	case 'N', 'n':
		month = time.November
	case 'D', 'd':
		month = time.December
	default:
		return time.Time{}
	}
	day := int(atoiSafe(code[3:]))
	if day < 1 || day > 31 {
		return time.Time{}
	}
	return time.Date(2000+atoiSafe(code[:2]), month, day, 0, 0, 0, 0, time.UTC)
}
