// Package risk enforces pre-trade risk limits. Every order passes through
// Check before it reaches the broker; a violation rejects the order with a
// typed RiskError. This is the platform's primary guardrail against runaway
// losses — options can move fast.
//
// Check is kept stateless and pure: the engine passes in the current open
// position count and day PnL (computed from fresh in-memory state), so the
// limits are never evaluated against stale data.
package risk

import (
	"context"
	"fmt"
	"sync"

	"kite-algo/internal/broker"
)

// Limits are the configurable risk thresholds.
type Limits struct {
	MaxDailyLoss     float64 // rupees; block new entries when day PnL <= -this
	MaxOpenPositions int     // concurrent open positions (distinct symbols)
	MaxOrderValue    float64 // max rupee value of a single order (qty * price)
	MaxLotsPerTrade  int     // max lots per order; 0 = uncapped (lot-multiple validity still enforced)
}

// RiskError is returned when an order violates a limit. Typed so callers can
// distinguish risk rejections from infrastructure errors.
type RiskError struct {
	Rule    string
	Message string
}

func (e *RiskError) Error() string { return fmt.Sprintf("risk:%s: %s", e.Rule, e.Message) }

// Manager evaluates orders against Limits.
//
// Limits are adjustable at runtime from the web UI, so every access goes
// through the mutex: Check runs on the order path (potentially the market-data
// goroutine) while SetLimits runs on an HTTP handler goroutine.
type Manager struct {
	mu     sync.RWMutex
	limits Limits
}

// NewManager returns a risk Manager.
func NewManager(limits Limits) *Manager {
	return &Manager{limits: limits}
}

// Limits returns the configured limits (for display/logging).
func (m *Manager) Limits() Limits {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.limits
}

// SetLimits replaces the active limits. Takes effect on the next order; orders
// already submitted to the broker are unaffected.
func (m *Manager) SetLimits(l Limits) {
	m.mu.Lock()
	m.limits = l
	m.mu.Unlock()
}

// Check returns nil if the order is allowed, or a *RiskError describing the
// violated rule.
//
//	openPositions: current count of distinct open positions
//	dayPnL:        realized + unrealized PnL for the trading day (rupees)
//	lotSize:       instrument lot size (0 disables the lot checks)
//	openingNew:    true if this order would open a brand-new position (vs add to
//	               or close an existing one) — only opening orders are capped by
//	               MaxOpenPositions, so closing trades are never blocked by it.
func (m *Manager) Check(ctx context.Context, req broker.OrderRequest, lotSize int, openPositions int, dayPnL float64, openingNew bool) error {
	_ = ctx // reserved for future use (e.g. tracing); checks are synchronous

	// Snapshot once so a concurrent SetLimits cannot change the rules midway
	// through evaluating a single order.
	m.mu.RLock()
	limits := m.limits
	m.mu.RUnlock()

	// An order that reduces exposure is never blocked. Not by any rule here.
	//
	// Every limit in this file exists to cap the risk you are TAKING ON. Applied
	// to an exit they do the opposite of their purpose: they trap you in the
	// position they were meant to protect you from. This has been got wrong
	// repeatedly, in four different ways, and each looked reasonable in
	// isolation:
	//
	//   - max-daily-loss fires exactly on the day you most need to flatten.
	//   - max-order-value blocks closing a position bigger than the cap you set
	//     for opening one.
	//   - max-lots-per-trade blocks closing a 3-lot position built from three
	//     1-lot entries, because the exit is naturally one larger order.
	//   - the kill switch blocked its own square-off (handled in the engine).
	//
	// So the rule is unconditional: closing orders pass. Quantity validity is
	// left to the exchange, which is the real authority on what it will accept —
	// rejecting an exit locally to spare a round trip is a bad trade.
	if req.Intent == broker.IntentClose {
		return nil
	}

	// 1. Daily loss limit — the most important guardrail for options.
	if limits.MaxDailyLoss > 0 && dayPnL <= -limits.MaxDailyLoss {
		return &RiskError{
			Rule: "max-daily-loss",
			Message: fmt.Sprintf(
				"day PnL %.2f has hit the -%.2f limit; no new entries", dayPnL, limits.MaxDailyLoss),
		}
	}

	// 2. Order value limit (qty * reference price).
	if limits.MaxOrderValue > 0 && req.Price > 0 {
		value := float64(req.Quantity) * req.Price
		if value > limits.MaxOrderValue {
			return &RiskError{
				Rule:    "max-order-value",
				Message: fmt.Sprintf("order value %.2f exceeds limit %.2f", value, limits.MaxOrderValue),
			}
		}
	}

	// 3. Valid lot quantity.
	//
	// Validity, not sizing, and therefore NOT conditional on MaxLotsPerTrade.
	// The exchange rejects a quantity that is not a multiple of the lot size
	// whatever your limits say, so catching it here saves a round trip and an
	// order that was never going to rest.
	//
	// This used to be nested inside the MaxLotsPerTrade block, which meant
	// setting that limit to "no limit" quietly disabled the check as well —
	// switching off a sizing cap should not switch off input validation.
	if lotSize > 0 && req.Quantity%lotSize != 0 {
		return &RiskError{
			Rule:    "invalid-lot-quantity",
			Message: fmt.Sprintf("qty %d is not a multiple of lot size %d", req.Quantity, lotSize),
		}
	}

	// 4. Lots-per-trade cap.
	if limits.MaxLotsPerTrade > 0 && lotSize > 0 {
		lots := req.Quantity / lotSize
		if lots > limits.MaxLotsPerTrade {
			return &RiskError{
				Rule:    "max-lots-per-trade",
				Message: fmt.Sprintf("%d lots exceeds limit %d", lots, limits.MaxLotsPerTrade),
			}
		}
	}

	// 5. Open-positions cap — only blocks orders that would open a NEW symbol.
	if limits.MaxOpenPositions > 0 && openingNew && openPositions >= limits.MaxOpenPositions {
		return &RiskError{
			Rule: "max-open-positions",
			Message: fmt.Sprintf(
				"%d open positions at the limit %d", openPositions, limits.MaxOpenPositions),
		}
	}

	return nil
}
