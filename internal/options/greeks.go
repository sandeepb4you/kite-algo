// Package options implements option pricing and Greeks via the Black-Scholes
// model, implied-volatility solving, and helpers for parsing Indian option
// trading symbols.
//
// Indian exchange-traded equity/index options are European-style (exercisable
// only at expiry), so Black-Scholes is the correct model — no need for the
// binomial/American machinery.
package options

import "math"

// OptionType is Call or Put.
type OptionType int

const (
	Call OptionType = iota
	Put
)

// String returns "CE" or "PE" — the suffixes NSE uses on trading symbols.
func (o OptionType) String() string {
	if o == Put {
		return "PE"
	}
	return "CE"
}

// ParseOptionType parses "CE"/"PE" (case-insensitive). Returns Call, false for
// anything that isn't a recognized option type.
func ParseOptionType(s string) (OptionType, bool) {
	switch norm(s) {
	case "ce", "c", "call":
		return Call, true
	case "pe", "p", "put":
		return Put, true
	}
	return Call, false
}

// Greeks holds the Black-Scholes Greeks plus the theoretical price.
//
// Conventions (what an Indian option trader expects to see):
//   - Price    : theoretical option price in rupees
//   - Delta    : per unit spot (call in [0,1], put in [-1,0])
//   - Gamma    : per unit spot
//   - Theta    : rupees per calendar DAY (negative for long options)
//   - Vega     : rupees per 1% change in implied volatility
//   - Rho      : rupees per 1% change in interest rate
type Greeks struct {
	Price float64
	Delta float64
	Gamma float64
	Theta float64
	Vega  float64
	Rho   float64
}

// BlackScholes computes the price and Greeks of a European option.
//
//	spot             : underlying price
//	strike           : strike price
//	timeToExpiryYears: time to expiry; use YearsToExpiry to compute it
//	volatility       : annualized implied vol (e.g. 0.15 for 15%)
//	riskFreeRate     : annualized continuously-compounded rate (e.g. 0.06)
//	optType          : Call or Put
func BlackScholes(spot, strike, timeToExpiryYears, volatility, riskFreeRate float64, optType OptionType) Greeks {
	// Degenerate cases: at expiry an option is worth intrinsic value, with
	// Delta 1/0/-1 and other Greeks zero. This also guards divide-by-zero.
	if timeToExpiryYears <= 0 || volatility <= 0 || spot <= 0 || strike <= 0 {
		return intrinsicGreeks(spot, strike, optType)
	}

	sqrtT := math.Sqrt(timeToExpiryYears)
	volSqrtT := volatility * sqrtT
	d1 := (math.Log(spot/strike) + (riskFreeRate+0.5*volatility*volatility)*timeToExpiryYears) / volSqrtT
	d2 := d1 - volSqrtT

	nd1 := normCDF(d1) // N(d1)
	nd2 := normCDF(d2) // N(d2)
	pdf := normPDF(d1) // φ(d1)
	discount := math.Exp(-riskFreeRate * timeToExpiryYears)

	g := Greeks{
		Gamma: pdf / (spot * volSqrtT),
		Vega:  spot * pdf * sqrtT / 100.0, // per 1% IV
	}
	// Theta components common to both:
	//   -S·φ(d1)·σ / (2√T)
	thetaTerm := -spot * pdf * volatility / (2 * sqrtT)

	switch optType {
	case Call:
		g.Price = spot*nd1 - strike*discount*nd2
		g.Delta = nd1
		g.Theta = (thetaTerm - riskFreeRate*strike*discount*nd2) / 365.0 // per day
		g.Rho = strike * timeToExpiryYears * discount * nd2 / 100.0      // per 1% rate
	default: // Put
		g.Price = strike*discount*normCDF(-d2) - spot*normCDF(-d1)
		g.Delta = nd1 - 1
		g.Theta = (thetaTerm + riskFreeRate*strike*discount*normCDF(-d2)) / 365.0
		g.Rho = -strike * timeToExpiryYears * discount * normCDF(-d2) / 100.0
	}
	return g
}

// intrinsicGreeks returns the expiry/limit Greeks for a (near-)at-expiry option.
func intrinsicGreeks(spot, strike float64, optType OptionType) Greeks {
	g := Greeks{}
	switch optType {
	case Call:
		intrinsic := math.Max(spot-strike, 0)
		g.Price = intrinsic
		switch {
		case spot > strike:
			g.Delta = 1
		case spot < strike:
			g.Delta = 0
		default:
			g.Delta = 0.5
		}
	default:
		intrinsic := math.Max(strike-spot, 0)
		g.Price = intrinsic
		switch {
		case spot < strike:
			g.Delta = -1
		case spot > strike:
			g.Delta = 0
		default:
			g.Delta = -0.5
		}
	}
	return g
}

// normCDF is the standard normal cumulative distribution N(x).
// Uses the relationship N(x) = 0.5 * erfc(-x / sqrt(2)).
func normCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// normPDF is the standard normal probability density φ(x).
func normPDF(x float64) float64 {
	return math.Exp(-0.5*x*x) / math.Sqrt(2*math.Pi)
}

// norm lowercases and trims a type string for comparison.
func norm(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			out = append(out, byte(r))
		} else if r >= 'A' && r <= 'Z' {
			out = append(out, byte(r+('a'-'A')))
		}
	}
	return string(out)
}
