package risk

import (
	"context"
	"errors"
	"testing"

	"kite-algo/internal/broker"
)

func limits() Limits {
	return Limits{
		MaxDailyLoss:     5000,
		MaxOpenPositions: 5,
		MaxOrderValue:    100000,
		MaxLotsPerTrade:  2,
	}
}

func order(intent broker.OrderIntent, qty int, price float64) broker.OrderRequest {
	return broker.OrderRequest{
		StrategyID:    "manual",
		Intent:        intent,
		Exchange:      "NFO",
		TradingSymbol: "NIFTY24AUG24500CE",
		OrderType:     broker.OrderTypeLimit,
		Side:          broker.SideBuy,
		Quantity:      qty,
		Price:         price,
	}
}

func ruleOf(t *testing.T, err error) string {
	t.Helper()
	var re *RiskError
	if !errors.As(err, &re) {
		t.Fatalf("expected a *RiskError, got %T: %v", err, err)
	}
	return re.Rule
}

// TestClosingOrdersSurviveDailyLossLimit is the most important test in this
// package. The daily-loss limit trips on exactly the day you most need to
// flatten; if it also blocked closing orders, the risk manager would reject the
// square-off — and the panic button — at the worst possible moment.
func TestClosingOrdersSurviveDailyLossLimit(t *testing.T) {
	m := NewManager(limits())
	ctx := context.Background()
	blownPnL := -9000.0 // well past the -5000 limit

	// An opening order must be refused.
	err := m.Check(ctx, order(broker.IntentOpen, 75, 100), 75, 1, blownPnL, true)
	if err == nil {
		t.Fatal("opening order allowed after the daily loss limit was breached")
	}
	if rule := ruleOf(t, err); rule != "max-daily-loss" {
		t.Errorf("rule = %q, want max-daily-loss", rule)
	}

	// The same order marked as closing must be allowed through.
	if err := m.Check(ctx, order(broker.IntentClose, 75, 100), 75, 1, blownPnL, false); err != nil {
		t.Errorf("closing order rejected after a loss-limit breach: %v — "+
			"this would make square-off impossible on a bad day", err)
	}
}

// TestClosingOrdersSurviveOrderValueLimit covers flattening a position larger
// than the per-order value cap. The cap exists to bound new exposure; refusing
// to let a big position out is the opposite of risk control.
func TestClosingOrdersSurviveOrderValueLimit(t *testing.T) {
	m := NewManager(limits())
	ctx := context.Background()
	big := order(broker.IntentOpen, 75, 5000) // 375,000 > 100,000 cap

	if err := m.Check(ctx, big, 75, 0, 0, true); err == nil {
		t.Fatal("oversized opening order was allowed")
	} else if rule := ruleOf(t, err); rule != "max-order-value" {
		t.Errorf("rule = %q, want max-order-value", rule)
	}

	closing := big
	closing.Intent = broker.IntentClose
	if err := m.Check(ctx, closing, 75, 1, 0, false); err != nil {
		t.Errorf("oversized closing order rejected: %v", err)
	}
}

// TestNoLimitEverBlocksAnExit is the flat rule, stated once.
//
// Every limit here caps risk being TAKEN ON. Applied to an exit they trap the
// operator in the position they were meant to guard against. This has been got
// wrong four separate times, each plausible in isolation, so the property is
// asserted for every rule at once rather than case by case.
func TestNoLimitEverBlocksAnExit(t *testing.T) {
	m := NewManager(limits())
	ctx := context.Background()

	cases := []struct {
		name      string
		req       broker.OrderRequest
		lotSize   int
		openPos   int
		dayPnL    float64
		newSymbol bool
	}{
		{"daily loss breached", order(broker.IntentClose, 75, 100), 75, 1, -50000, false},
		{"value over the cap", order(broker.IntentClose, 75, 5000), 75, 1, 0, false},
		{"more lots than the per-trade cap",
			// The exact report from the field: a 3-lot position built from three
			// 1-lot entries cannot be closed by one 3-lot order.
			order(broker.IntentClose, 225, 100), 75, 1, 0, false},
		{"quantity is not a lot multiple", order(broker.IntentClose, 70, 100), 75, 1, 0, false},
		{"at the open-position cap", order(broker.IntentClose, 75, 100), 75, 99, 0, true},
		{"everything wrong at once", order(broker.IntentClose, 9999, 99999), 75, 99, -999999, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.Check(ctx, tc.req, tc.lotSize, tc.openPos, tc.dayPnL, tc.newSymbol); err != nil {
				t.Errorf("a closing order was blocked by %v.\n"+
					"No limit may ever prevent an exit — that traps the operator in "+
					"the position the limit exists to protect them from.", err)
			}
		})
	}
}

// TestOpeningOrdersStillEnforceEveryLimit guards against the exemption leaking
// into the default path — IntentOpen is the zero value, so a bug here would
// silently disable the limits for every ordinary order.
func TestOpeningOrdersStillEnforceEveryLimit(t *testing.T) {
	m := NewManager(limits())
	ctx := context.Background()

	tests := []struct {
		name      string
		req       broker.OrderRequest
		lotSize   int
		openPos   int
		dayPnL    float64
		newSymbol bool
		wantRule  string
	}{
		{"daily loss", order(broker.IntentOpen, 75, 100), 75, 1, -6000, true, "max-daily-loss"},
		{"order value", order(broker.IntentOpen, 75, 5000), 75, 1, 0, true, "max-order-value"},
		{"lot multiple", order(broker.IntentOpen, 70, 100), 75, 1, 0, true, "invalid-lot-quantity"},
		{"lots per trade", order(broker.IntentOpen, 225, 10), 75, 1, 0, true, "max-lots-per-trade"},
		{"open positions", order(broker.IntentOpen, 75, 100), 75, 5, 0, true, "max-open-positions"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := m.Check(ctx, tc.req, tc.lotSize, tc.openPos, tc.dayPnL, tc.newSymbol)
			if err == nil {
				t.Fatalf("%s limit was not enforced on an opening order", tc.name)
			}
			if rule := ruleOf(t, err); rule != tc.wantRule {
				t.Errorf("rule = %q, want %q", rule, tc.wantRule)
			}
		})
	}
}

// TestLotMultipleSurvivesEveryCapBeingOff pins the one behaviour that made the
// lot-multiple check worth separating from MaxLotsPerTrade: an operator who
// switches off the sizing caps is saying "let me trade any size", not "send the
// exchange quantities it cannot accept". With every cap at zero a malformed
// quantity must still be rejected here rather than on a round trip to Zerodha.
func TestLotMultipleSurvivesEveryCapBeingOff(t *testing.T) {
	m := NewManager(Limits{MaxDailyLoss: 5000}) // every sizing cap off
	ctx := context.Background()

	err := m.Check(ctx, order(broker.IntentOpen, 70, 100), 75, 0, 0, true)
	if err == nil {
		t.Fatal("qty 70 against lot size 75 was accepted with the sizing caps off")
	}
	if rule := ruleOf(t, err); rule != "invalid-lot-quantity" {
		t.Errorf("rule = %q, want %q", rule, "invalid-lot-quantity")
	}

	// The cap really is off: a size that MaxLotsPerTrade would have blocked
	// passes, so the check above is validity and not the cap in disguise.
	if err := m.Check(ctx, order(broker.IntentOpen, 7500, 100), 75, 0, 0, true); err != nil {
		t.Errorf("100 lots rejected with no lots cap configured: %v", err)
	}
}

func TestValidOrderPasses(t *testing.T) {
	m := NewManager(limits())
	if err := m.Check(context.Background(), order(broker.IntentOpen, 75, 100), 75, 1, 500, false); err != nil {
		t.Errorf("a well-formed order within every limit was rejected: %v", err)
	}
}

// TestSetLimitsTakesEffect covers runtime limit editing from the web UI.
func TestSetLimitsTakesEffect(t *testing.T) {
	m := NewManager(limits())
	req := order(broker.IntentOpen, 75, 100) // value 7,500

	if err := m.Check(context.Background(), req, 75, 1, 0, false); err != nil {
		t.Fatalf("order rejected under the original limits: %v", err)
	}

	tightened := limits()
	tightened.MaxOrderValue = 1000
	m.SetLimits(tightened)

	err := m.Check(context.Background(), req, 75, 1, 0, false)
	if err == nil {
		t.Fatal("order still allowed after the limit was tightened")
	}
	if rule := ruleOf(t, err); rule != "max-order-value" {
		t.Errorf("rule = %q, want max-order-value", rule)
	}
	if got := m.Limits().MaxOrderValue; got != 1000 {
		t.Errorf("Limits().MaxOrderValue = %v, want 1000", got)
	}
}
