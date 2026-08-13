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
	Underlying string    // NIFTY, BANKNIFTY, FINNIFTY, ...
	Strike     float64   // 24500
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

	// Strike = trailing digits of the body.
	i := len(body)
	for i > 0 && body[i-1] >= '0' && body[i-1] <= '9' {
		i--
	}
	if i == len(body) {
		return Spec{}, false // no digits → not an option symbol
	}
	strikeStr := body[i:]
	strike, err := strconv.ParseFloat(strikeStr, 64)
	if err != nil {
		return Spec{}, false
	}
	prefix := body[:i] // underlying + expiry code

	underlying, expiryCode := splitUnderlying(prefix)
	exp := parseExpiryCode(expiryCode)

	return Spec{
		Underlying: underlying,
		Strike:     strike,
		Type:       optType,
		ExpiryCode: expiryCode,
		Expiry:     exp,
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
// Returns the zero time if it can't parse.
func parseExpiryCode(code string) time.Time {
	code = strings.TrimSpace(code)
	if code == "" {
		return time.Time{}
	}
	// Numeric form: YYMM or YYMMDD.
	if isAllDigits(code) {
		return parseNumericExpiry(code)
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
