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
type Manager struct {
	limits Limits
}

// NewManager returns a risk Manager.
func NewManager(limits Limits) *Manager {
	return &Manager{limits: limits}
}

// Limits returns the configured limits (for display/logging).
func (m *Manager) Limits() Limits { return m.limits }

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

	// 1. Daily loss limit — the most important guardrail for options.
	if m.limits.MaxDailyLoss > 0 && dayPnL <= -m.limits.MaxDailyLoss {
		return &RiskError{
			Rule: "max-daily-loss",
			Message: fmt.Sprintf(
				"day PnL %.2f has hit the -%.2f limit; no new entries", dayPnL, m.limits.MaxDailyLoss),
		}
	}

	// 2. Order value limit (qty * reference price).
	if m.limits.MaxOrderValue > 0 && req.Price > 0 {
		value := float64(req.Quantity) * req.Price
		if value > m.limits.MaxOrderValue {
			return &RiskError{
				Rule:    "max-order-value",
				Message: fmt.Sprintf("order value %.2f exceeds limit %.2f", value, m.limits.MaxOrderValue),
			}
		}
	}

	// 3. Lots-per-trade + valid-lot-quantity checks.
	if m.limits.MaxLotsPerTrade > 0 && lotSize > 0 {
		if req.Quantity%lotSize != 0 {
			return &RiskError{
				Rule:    "invalid-lot-quantity",
				Message: fmt.Sprintf("qty %d is not a multiple of lot size %d", req.Quantity, lotSize),
			}
		}
		lots := req.Quantity / lotSize
		if lots > m.limits.MaxLotsPerTrade {
			return &RiskError{
				Rule:    "max-lots-per-trade",
				Message: fmt.Sprintf("%d lots exceeds limit %d", lots, m.limits.MaxLotsPerTrade),
			}
		}
	}

	// 4. Open-positions cap — only blocks orders that would open a NEW symbol.
	if m.limits.MaxOpenPositions > 0 && openingNew && openPositions >= m.limits.MaxOpenPositions {
		return &RiskError{
			Rule: "max-open-positions",
			Message: fmt.Sprintf(
				"%d open positions at the limit %d", openPositions, m.limits.MaxOpenPositions),
		}
	}

	return nil
}
