package kite

import "testing"

// Market protection must travel with MARKET orders and nowhere else.
//
// The exchanges mandate it on algo market orders and Kite refuses a MARKET order
// that arrives without one — "Market orders not allowed without market
// protection" — so an absent field here is not a missing nicety, it is every
// market order on the real book failing, square-offs included.
func TestMarketProtectionIsSentOnlyWhereItApplies(t *testing.T) {
	base := PlaceOrderParams{
		Exchange:         "NFO",
		Tradingsymbol:    "NIFTY2681824350CE",
		TransactionType:  "SELL",
		Quantity:         75,
		Product:          "MIS",
		Validity:         "DAY",
		MarketProtection: -1,
	}

	cases := []struct {
		orderType string
		want      string // "" means the field must be absent
	}{
		{"MARKET", "-1"},
		{"SL-M", "-1"},
		// Both carry their own price bound, and Kite documents the parameter as
		// having no effect on them.
		{"LIMIT", ""},
		{"SL", ""},
	}
	for _, tc := range cases {
		t.Run(tc.orderType, func(t *testing.T) {
			p := base
			p.OrderType = tc.orderType
			got := paramsToForm(p).Get("market_protection")
			if got != tc.want {
				t.Errorf("market_protection = %q, want %q", got, tc.want)
			}
		})
	}
}

// Zero must never be sent. Kite rejects market_protection=0 outright, so a zero
// reaching the form is indistinguishable from the bug this parameter fixes.
func TestZeroMarketProtectionIsNotSent(t *testing.T) {
	p := PlaceOrderParams{
		Exchange: "NFO", Tradingsymbol: "X", TransactionType: "BUY",
		Quantity: 75, Product: "MIS", OrderType: "MARKET", Validity: "DAY",
		MarketProtection: 0,
	}
	if got, ok := paramsToForm(p)["market_protection"]; ok {
		t.Errorf("market_protection sent as %v; Kite rejects 0, so it must be omitted", got)
	}
}
