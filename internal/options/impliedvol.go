package options

import (
	"errors"
	"math"
	"time"
)

// ImpliedVol inverts the Black-Scholes price to find the volatility implied by
// a market price. We use bisection: it is slower than Newton-Raphson but always
// converges for arbitrage-free prices and never divides by (near-zero) vega.
//
// Returns annualized volatility (e.g. 0.15 for 15%). Returns an error if the
// given price is below intrinsic value or above the no-arbitrage bound.
func ImpliedVol(price, spot, strike, timeToExpiryYears, riskFreeRate float64, optType OptionType) (float64, error) {
	if price <= 0 || spot <= 0 || strike <= 0 || timeToExpiryYears <= 0 {
		return 0, errors.New("implied vol: inputs must be positive")
	}
	// Sanity: the option price must lie within [intrinsic, arbitrage bound].
	intrinsic := intrinsicValue(spot, strike, optType)
	if price < intrinsic-1e-9 {
		return 0, errors.New("implied vol: price below intrinsic value")
	}

	lo, hi := 0.0001, 5.0 // 0.01% .. 500%
	// Expand the top if needed so that BS(hi) >= price.
	for i := 0; i < 64; i++ {
		g := BlackScholes(spot, strike, timeToExpiryYears, hi, riskFreeRate, optType)
		if g.Price >= price {
			break
		}
		hi *= 2
		if hi > 1e6 {
			break
		}
	}

	var mid float64
	for i := 0; i < 100; i++ {
		mid = 0.5 * (lo + hi)
		g := BlackScholes(spot, strike, timeToExpiryYears, mid, riskFreeRate, optType)
		diff := g.Price - price
		if math.Abs(diff) < 1e-4 || (hi-lo) < 1e-6 {
			return mid, nil
		}
		// If model price > market price, vol is too high → lower the ceiling.
		if diff > 0 {
			hi = mid
		} else {
			lo = mid
		}
	}
	return mid, nil
}

// intrinsicValue returns the payoff at expiry of an option.
func intrinsicValue(spot, strike float64, optType OptionType) float64 {
	if optType == Call {
		return math.Max(spot-strike, 0)
	}
	return math.Max(strike-spot, 0)
}

// YearsToExpiry returns the fraction of a year from now until expiry, using the
// conventional Indian option expiry cutoff of 15:30 IST. Pass the expiry DATE
// (not datetime); the time component is filled in here.
func YearsToExpiry(now, expiry time.Time) float64 {
	if expiry.IsZero() {
		return 0
	}
	// Normalize expiry to 15:30 IST (UTC+5:30).
	ist := time.FixedZone("IST", 5*3600+30*60)
	exp := time.Date(expiry.Year(), expiry.Month(), expiry.Day(), 15, 30, 0, 0, ist)
	now = now.In(ist)
	d := exp.Sub(now)
	if d <= 0 {
		return 0
	}
	return d.Hours() / 24.0 / 365.0
}

// ATMStrike returns the nearest tradable strike to spot, rounded to the strike
// step (e.g. 50 for NIFTY, 100 for BANKNIFTY). Useful for picking the short leg
// of an ATM strategy.
func ATMStrike(spot, step float64) float64 {
	if step <= 0 {
		return spot
	}
	return math.Round(spot/step) * step
}
