package engine

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// spotSymbols maps an option underlying to the index whose spot price drives it.
//
// Kite names them differently — options are written on "NIFTY" while the index
// quote is "NIFTY 50" — so the chain cannot find its own spot without this.
var spotSymbols = map[string]string{
	"NIFTY":      "NIFTY 50",
	"BANKNIFTY":  "NIFTY BANK",
	"FINNIFTY":   "NIFTY FIN SERVICE",
	"MIDCPNIFTY": "NIFTY MID SELECT",
}

// SpotSymbolFor returns the index symbol quoting an option underlying's spot.
func SpotSymbolFor(underlying string) string {
	if s, ok := spotSymbols[underlying]; ok {
		return s
	}
	return underlying
}

// ChainLeg is one side of a strike.
type ChainLeg struct {
	TradingSymbol string  `json:"symbol"`
	Token         uint32  `json:"token"`
	LastPrice     float64 `json:"last_price"`
	LotSize       int     `json:"lot_size"`
	// Held is the operator's or a strategy's net quantity in this contract,
	// shown inline so an existing position is visible while placing the next
	// order rather than on a different page.
	Held int `json:"held"`
}

// ChainRow is one strike with its call and put.
type ChainRow struct {
	Strike float64  `json:"strike"`
	Call   ChainLeg `json:"call"`
	Put    ChainLeg `json:"put"`
	IsATM  bool     `json:"is_atm"`
}

// OptionChain is a rendered chain for one underlying and expiry.
type OptionChain struct {
	Underlying  string      `json:"underlying"`
	SpotSymbol  string      `json:"spot_symbol"`
	Spot        float64     `json:"spot"`
	Expiry      time.Time   `json:"expiry"`
	Expiries    []time.Time `json:"expiries"`
	Underlyings []string    `json:"underlyings"`
	ATMStrike   float64     `json:"atm_strike"`
	Rows        []ChainRow  `json:"rows"`
	// Truncated reports that strikes outside the requested window were omitted.
	Truncated bool `json:"truncated"`
}

// DefaultChainDepth is how many strikes to show either side of ATM.
//
// A NIFTY weekly chain runs to several hundred strikes; rendering all of them
// buries the ones anyone trades and makes the page unusable. Twenty rows around
// the money is what a trader actually looks at.
const DefaultChainDepth = 10

// OptionChain builds the chain an operator sees.
//
// expiry may be zero, in which case the nearest available one is used — the
// weekly, for an underlying that has weeklies. depth limits how far either side
// of the money to include; pass 0 for DefaultChainDepth.
func (e *Engine) OptionChain(underlying string, expiry time.Time, depth int) (OptionChain, error) {
	e.cmu.RLock()
	instruments := e.instruments
	e.cmu.RUnlock()

	if instruments == nil {
		return OptionChain{}, fmt.Errorf("no instrument master loaded — connect to Zerodha first")
	}
	if depth <= 0 {
		depth = DefaultChainDepth
	}

	out := OptionChain{
		Underlying:  underlying,
		SpotSymbol:  SpotSymbolFor(underlying),
		Underlyings: instruments.Underlyings(),
		Expiries:    instruments.Expiries(underlying, time.Now()),
	}
	if len(out.Expiries) == 0 {
		return out, fmt.Errorf("no live option expiries for %q", underlying)
	}

	// Default to the nearest expiry, which for NIFTY is the current week.
	out.Expiry = out.Expiries[0]
	if !expiry.IsZero() {
		want := expiry.Format("2006-01-02")
		for _, e := range out.Expiries {
			if e.Format("2006-01-02") == want {
				out.Expiry = e
				break
			}
		}
	}

	legs := instruments.Chain(underlying, out.Expiry)
	if len(legs) == 0 {
		return out, fmt.Errorf("no contracts for %s %s",
			underlying, out.Expiry.Format("02 Jan 2006"))
	}

	prices := e.Prices()
	out.Spot = prices[out.SpotSymbol]

	// Group the flat leg list into per-strike rows.
	byStrike := make(map[float64]*ChainRow)
	var strikes []float64
	held := e.heldQuantities()

	for i := range legs {
		leg := &legs[i]
		row, ok := byStrike[leg.Strike]
		if !ok {
			row = &ChainRow{Strike: leg.Strike}
			byStrike[leg.Strike] = row
			strikes = append(strikes, leg.Strike)
		}
		entry := ChainLeg{
			TradingSymbol: leg.TradingSymbol,
			Token:         leg.InstrumentToken,
			LastPrice:     prices[leg.TradingSymbol],
			LotSize:       leg.LotSize,
			Held:          held[leg.TradingSymbol],
		}
		switch leg.InstrumentType {
		case "CE":
			row.Call = entry
		case "PE":
			row.Put = entry
		}
	}
	sort.Float64s(strikes)

	// Centre the window on the money. Without a spot price — before the first
	// tick arrives — fall back to the middle of the chain so the page still
	// shows something useful rather than the deepest out-of-the-money strikes.
	atmIndex := len(strikes) / 2
	if out.Spot > 0 {
		best := math.MaxFloat64
		for i, s := range strikes {
			if d := math.Abs(s - out.Spot); d < best {
				best, atmIndex = d, i
			}
		}
	}
	if len(strikes) > 0 {
		out.ATMStrike = strikes[atmIndex]
	}

	lo := atmIndex - depth
	if lo < 0 {
		lo = 0
	}
	hi := atmIndex + depth + 1
	if hi > len(strikes) {
		hi = len(strikes)
	}
	out.Truncated = lo > 0 || hi < len(strikes)

	for _, s := range strikes[lo:hi] {
		row := *byStrike[s]
		row.IsATM = s == out.ATMStrike
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// heldQuantities maps trading symbol to net position quantity.
func (e *Engine) heldQuantities() map[string]int {
	out := make(map[string]int)
	for _, p := range e.Positions() {
		if p.NetQuantity != 0 {
			out[p.TradingSymbol] += p.NetQuantity
		}
	}
	return out
}

// ChainSymbols returns every trading symbol in the chain, so the caller can
// subscribe to exactly the contracts on screen rather than the whole expiry.
func (c OptionChain) ChainSymbols() []string {
	out := make([]string, 0, len(c.Rows)*2+1)
	if c.SpotSymbol != "" {
		out = append(out, c.SpotSymbol)
	}
	for _, r := range c.Rows {
		if r.Call.TradingSymbol != "" {
			out = append(out, r.Call.TradingSymbol)
		}
		if r.Put.TradingSymbol != "" {
			out = append(out, r.Put.TradingSymbol)
		}
	}
	return out
}
