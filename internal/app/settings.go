package app

import (
	"context"
	"encoding/json"
	"fmt"

	"kite-algo/internal/config"
	"kite-algo/internal/risk"
)

// riskLimitsKey is where runtime risk-limit overrides are stored.
const riskLimitsKey = "risk.limits"

// configuredRiskLimits returns the limits from config.yaml — the defaults that
// apply when nothing has been saved.
func configuredRiskLimits(cfg *config.Config) risk.Limits {
	return risk.Limits{
		MaxDailyLoss:     cfg.Risk.MaxDailyLoss,
		MaxOpenPositions: cfg.Risk.MaxOpenPositions,
		MaxOrderValue:    cfg.Risk.MaxOrderValue,
		MaxLotsPerTrade:  cfg.Risk.MaxLotsPerTrade,
	}
}

// configuredLiveRiskLimits resolves the real book's non-derived limits.
//
// MaxDailyLoss is deliberately absent: it comes from LiveRisk, computed as a
// percentage of the day's opening balance, and a config value here would be a
// second source of truth that could silently disagree.
func configuredLiveRiskLimits(cfg *config.Config) risk.Limits {
	base := configuredRiskLimits(cfg)
	l := cfg.Risk.Live

	out := base
	out.MaxDailyLoss = 0 // supplied by LiveRisk
	if l.MaxOpenPositions > 0 {
		out.MaxOpenPositions = l.MaxOpenPositions
	}
	if l.MaxOrderValue > 0 {
		out.MaxOrderValue = l.MaxOrderValue
	}
	if l.MaxLotsPerTrade > 0 {
		out.MaxLotsPerTrade = l.MaxLotsPerTrade
	}
	return out
}

// configuredPaperRiskLimits resolves the simulated book's limits.
//
// Each field falls back to the real limit when unset, so an operator who only
// wants a looser daily-loss allowance for strategies sets that one line and
// inherits the rest. Inheriting is the safe default: an unset field becoming
// "no limit" would silently remove a guardrail.
func configuredPaperRiskLimits(cfg *config.Config) risk.Limits {
	real := configuredRiskLimits(cfg)
	p := cfg.Risk.Paper

	out := real
	if p.MaxDailyLoss > 0 {
		out.MaxDailyLoss = p.MaxDailyLoss
	}
	if p.MaxOpenPositions > 0 {
		out.MaxOpenPositions = p.MaxOpenPositions
	}
	if p.MaxOrderValue > 0 {
		out.MaxOrderValue = p.MaxOrderValue
	}
	if p.MaxLotsPerTrade > 0 {
		out.MaxLotsPerTrade = p.MaxLotsPerTrade
	}
	return out
}

// loadRiskLimits resolves the limits to start with: the configured defaults,
// overridden by anything the operator saved.
//
// A malformed stored value falls back to the config rather than failing the
// boot. Refusing to start because a settings row is corrupt would be the wrong
// trade for a platform whose job is to be reachable so you can flatten.
func loadRiskLimits(ctx context.Context, store interface {
	GetSetting(context.Context, string) (string, bool, error)
}, cfg *config.Config, logf func(string, ...any)) (risk.Limits, bool) {
	defaults := configuredRiskLimits(cfg)

	raw, found, err := store.GetSetting(ctx, riskLimitsKey)
	if err != nil || !found {
		if err != nil && logf != nil {
			logf("read saved risk limits failed: %v", err)
		}
		return defaults, false
	}

	var saved risk.Limits
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		if logf != nil {
			logf("saved risk limits are unreadable, using config defaults: %v", err)
		}
		return defaults, false
	}
	return saved, true
}

// SaveRiskLimits applies limits and persists them so they survive a restart.
func (a *App) SaveRiskLimits(ctx context.Context, l risk.Limits) error {
	a.SetRiskLimits(l)

	raw, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("encode risk limits: %w", err)
	}
	if err := a.Store.SetSetting(ctx, riskLimitsKey, string(raw)); err != nil {
		// The limits ARE live; only persistence failed. Say so precisely rather
		// than implying the change did not take effect.
		return fmt.Errorf("limits applied but not saved (they will revert on restart): %w", err)
	}

	a.mu.Lock()
	a.riskOverridden = true
	a.mu.Unlock()
	return nil
}

// ResetRiskLimits discards the saved override and returns to config.yaml.
//
// Worth having as an explicit action: an operator who has tightened and loosened
// limits through a stressful session should be able to get back to a known state
// without remembering what the numbers originally were.
func (a *App) ResetRiskLimits(ctx context.Context) error {
	defaults := configuredPaperRiskLimits(a.Cfg)
	a.SetRiskLimits(defaults)

	if err := a.Store.DeleteSetting(ctx, riskLimitsKey); err != nil {
		return fmt.Errorf("limits reset but the saved override remains: %w", err)
	}

	a.mu.Lock()
	a.riskOverridden = false
	a.mu.Unlock()
	return nil
}

// RiskOverridden reports whether the active limits came from a saved override
// rather than from config.yaml.
func (a *App) RiskOverridden() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.riskOverridden
}

// ConfiguredRiskLimits returns the config.yaml defaults, for showing what a
// reset would restore.
func (a *App) ConfiguredRiskLimits() risk.Limits { return configuredRiskLimits(a.Cfg) }
