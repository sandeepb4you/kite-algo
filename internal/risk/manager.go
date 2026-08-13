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
	MaxLotsPerTrade  int     // max lots per order; 0 = unchecked
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

	// Orders that reduce exposure are exempt from the two limits below.
	//
	// This is not a convenience. The daily-loss rule trips precisely on the day
	// you most need to flatten, and an unconditional check would have the risk
	// manager reject the square-off that stops the bleeding — including the
	// panic button. A limit whose purpose is to stop you opening new risk must
	// never stop you shedding risk you already carry.
	//
	// Lot-multiple validation (rule 3) still applies to closes: the exchange
	// rejects a malformed quantity regardless of intent.
	closing := req.Intent == broker.IntentClose

	// 1. Daily loss limit — the most important guardrail for options.
	if !closing && limits.MaxDailyLoss > 0 && dayPnL <= -limits.MaxDailyLoss {
		return &RiskError{
			Rule: "max-daily-loss",
			Message: fmt.Sprintf(
				"day PnL %.2f has hit the -%.2f limit; no new entries", dayPnL, limits.MaxDailyLoss),
		}
	}

	// 2. Order value limit (qty * reference price).
	if !closing && limits.MaxOrderValue > 0 && req.Price > 0 {
		value := float64(req.Quantity) * req.Price
		if value > limits.MaxOrderValue {
			return &RiskError{
				Rule:    "max-order-value",
				Message: fmt.Sprintf("order value %.2f exceeds limit %.2f", value, limits.MaxOrderValue),
			}
		}
	}

	// 3. Lots-per-trade + valid-lot-quantity checks.
	if limits.MaxLotsPerTrade > 0 && lotSize > 0 {
		if req.Quantity%lotSize != 0 {
			return &RiskError{
				Rule:    "invalid-lot-quantity",
				Message: fmt.Sprintf("qty %d is not a multiple of lot size %d", req.Quantity, lotSize),
			}
		}
		lots := req.Quantity / lotSize
		if lots > limits.MaxLotsPerTrade {
			return &RiskError{
				Rule:    "max-lots-per-trade",
				Message: fmt.Sprintf("%d lots exceeds limit %d", lots, limits.MaxLotsPerTrade),
			}
		}
	}

	// 4. Open-positions cap — only blocks orders that would open a NEW symbol.
	if limits.MaxOpenPositions > 0 && openingNew && openPositions >= limits.MaxOpenPositions {
		return &RiskError{
			Rule: "max-open-positions",
			Message: fmt.Sprintf(
				"%d open positions at the limit %d", openPositions, limits.MaxOpenPositions),
		}
	}

	return nil
}
