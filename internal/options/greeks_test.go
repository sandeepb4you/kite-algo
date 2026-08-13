package options

import (
	"math"
	"testing"
	"time"
)

// approxEq checks equality within a relative tolerance, used for float math.
func approxEq(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol*math.Max(1, math.Abs(b))
}

// TestBlackScholesCall checks the canonical textbook example
// (Hull, Options Futures and Other Derivatives).
func TestBlackScholesCall(t *testing.T) {
	// S=100, K=100, T=1y, sigma=0.20, r=0.05, European call.
	g := BlackScholes(100, 100, 1.0, 0.20, 0.05, Call)
	// Reference values (standard tables):
	//   Price ~10.4506, Delta ~0.6368, Gamma ~0.0188,
	//   Vega per 1% ~0.3752, Theta/day ~-0.01757, Rho per 1% ~0.5324
	if !approxEq(g.Price, 10.4506, 1e-3) {
		t.Errorf("call price = %.4f, want ~10.4506", g.Price)
	}
	if !approxEq(g.Delta, 0.6368, 1e-3) {
		t.Errorf("call delta = %.4f, want ~0.6368", g.Delta)
	}
	if !approxEq(g.Gamma, 0.0188, 1e-3) {
		t.Errorf("call gamma = %.4f, want ~0.0188", g.Gamma)
	}
	if !approxEq(g.Vega, 0.3752, 1e-3) {
		t.Errorf("call vega = %.4f, want ~0.3752", g.Vega)
	}
	if !approxEq(g.Theta, -6.4140/365.0, 1e-4) {
		t.Errorf("call theta/day = %.5f, want ~%.5f", g.Theta, -6.4140/365.0)
	}
}

// TestPutCallParity verifies C - P = S - K*e^{-rT} via the put price.
func TestPutCallParity(t *testing.T) {
	c := BlackScholes(100, 100, 1.0, 0.20, 0.05, Call)
	p := BlackScholes(100, 100, 1.0, 0.20, 0.05, Put)
	parity := 100 - 100*math.Exp(-0.05*1.0)
	if !approxEq(c.Price-p.Price, parity, 1e-9) {
		t.Errorf("C-P = %.6f, want %.6f (parity)", c.Price-p.Price, parity)
	}
	// Put delta should equal call delta - 1.
	if !approxEq(p.Delta, c.Delta-1, 1e-9) {
		t.Errorf("put delta = %.6f, want %.6f", p.Delta, c.Delta-1)
	}
}

// TestIntrinsicAtExpiry checks the degenerate-case branch.
func TestIntrinsicAtExpiry(t *testing.T) {
	g := BlackScholes(120, 100, 0, 0.2, 0.05, Call)
	if g.Price != 20 || g.Delta != 1 {
		t.Errorf("intrinsic call: price=%.1f delta=%.1f, want 20/1", g.Price, g.Delta)
	}
	p := BlackScholes(80, 100, 0, 0.2, 0.05, Put)
	if p.Price != 20 || p.Delta != -1 {
		t.Errorf("intrinsic put: price=%.1f delta=%.1f, want 20/-1", p.Price, p.Delta)
	}
}

// TestImpliedVol checks that ImpliedVol recovers the volatility that produced a
// Black-Scholes price.
func TestImpliedVol(t *testing.T) {
	trueVol := 0.22
	g := BlackScholes(100, 100, 0.25, trueVol, 0.06, Call)
	iv, err := ImpliedVol(g.Price, 100, 100, 0.25, 0.06, Call)
	if err != nil {
		t.Fatalf("ImpliedVol err: %v", err)
	}
	if !approxEq(iv, trueVol, 1e-3) {
		t.Errorf("ImpliedVol = %.4f, want %.4f", iv, trueVol)
	}
}

// TestParseSymbol checks symbol parsing for the common monthly format.
func TestParseSymbol(t *testing.T) {
	sp, ok := ParseSymbol("NIFTY24AUG24500CE")
	if !ok {
		t.Fatal("expected parse success")
	}
	if sp.Underlying != "NIFTY" {
		t.Errorf("underlying = %q, want NIFTY", sp.Underlying)
	}
	if sp.Strike != 24500 {
		t.Errorf("strike = %v, want 24500", sp.Strike)
	}
	if sp.Type != Call {
		t.Errorf("type = %v, want Call", sp.Type)
	}
	// Last Thursday of Aug 2024 is the 29th.
	wantExpiry := time.Date(2024, time.August, 29, 0, 0, 0, 0, time.UTC)
	if !sp.Expiry.Equal(wantExpiry) {
		t.Errorf("expiry = %v, want %v", sp.Expiry, wantExpiry)
	}

	bp, ok := ParseSymbol("BANKNIFTY24OCT50000PE")
	if !ok || bp.Underlying != "BANKNIFTY" || bp.Strike != 50000 || bp.Type != Put {
		t.Errorf("banknifty parse wrong: %+v ok=%v", bp, ok)
	}
}

// TestATMStrike checks rounding to the strike step.
func TestATMStrike(t *testing.T) {
	if v := ATMStrike(24527, 50); v != 24550 {
		t.Errorf("ATMStrike(24527,50) = %v, want 24550", v)
	}
	if v := ATMStrike(24524, 50); v != 24500 {
		t.Errorf("ATMStrike(24524,50) = %v, want 24500", v)
	}
}
