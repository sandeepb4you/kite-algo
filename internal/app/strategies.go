package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"kite-algo/internal/broker"
	"kite-algo/internal/engine"
)

// Strategy instances survive a restart.
//
// This is the one exception to "strategies are never started automatically"
// (see app.go). The rule exists because config.yaml's `enabled: true` is a
// static flag that is easy to leave set from a previous session, so honouring it
// at boot means a redeploy or a crash-loop can begin trading with nobody
// watching. A persisted RUNNING instance is a different claim: it records that
// an operator clicked start, recently, and that the process died rather than
// being stopped.
//
// The dangerous half is not restarting the instance, it is restarting it
// EMPTY. Strategy state lives in memory while positions live in sqlite, so a
// naive restore gives you a strategy that believes it is flat while its legs are
// open — and it enters again on top of them. Restore therefore refuses to start
// any instance holding open positions unless the strategy implements
// strategy.Resumable and can rebuild its state from those positions.
//
// What comes back is management, not fresh risk: exits, delta monitoring and
// the square-off clock resume against what is already open. A new entry is
// still gated by the strategy's own entry rules.

const runningStrategiesKey = "strategies.running"

// persistedStrategy is one running instance, as stored.
//
// Deliberately just the spec. Runtime state — P&L, tick counts, positions — is
// derived from the order and position tables and would only rot here.
type persistedStrategy struct {
	InstanceID string         `json:"instance_id"`
	Type       string         `json:"type"`
	Params     map[string]any `json:"params"`
}

// StartStrategy starts an instance and records it, so a restart brings it back.
func (a *App) StartStrategy(ctx context.Context, spec engine.StrategySpec) (engine.StrategyStatus, error) {
	st, err := a.Engine.StartStrategy(ctx, spec)
	if err != nil {
		return st, err
	}
	a.saveRunningStrategies(ctx)
	return st, nil
}

// StopStrategy stops an instance and updates the record.
//
// The record is rewritten even when the stop reports an error: it is rebuilt
// from the engine's actual state rather than from what was asked for, so a
// partial failure persists what is really running instead of a guess.
func (a *App) StopStrategy(ctx context.Context, id string, opt engine.StopOptions) (engine.StrategyStatus, error) {
	st, err := a.Engine.StopStrategy(ctx, id, opt)
	a.saveRunningStrategies(ctx)
	return st, err
}

// SyncRunningStrategies rewrites the record from the engine's current state.
//
// For callers that change what is running without going through StartStrategy
// or StopStrategy — the kill switch being the one that matters, since a halt
// whose effect a restart silently undoes is not a kill switch.
func (a *App) SyncRunningStrategies(ctx context.Context) {
	a.saveRunningStrategies(ctx)
}

// saveRunningStrategies snapshots the engine's running instances.
//
// Best effort by design. Persistence failing must not fail the start — the
// strategy IS running, and refusing to trade because a settings row could not be
// written would be the wrong trade. The consequence of the failure is limited to
// not coming back after the next restart, which is exactly today's behaviour.
func (a *App) saveRunningStrategies(ctx context.Context) {
	if a.Store == nil || a.Engine == nil {
		return
	}

	var out []persistedStrategy
	for _, s := range a.Engine.ListStrategies() {
		if s.State != engine.StateRunning {
			continue
		}
		out = append(out, persistedStrategy{
			InstanceID: s.InstanceID,
			Type:       s.Type,
			Params:     s.Params,
		})
	}

	raw, err := json.Marshal(out)
	if err != nil {
		a.logf("encode running strategies: %v", err)
		return
	}
	if err := a.Store.SetSetting(ctx, runningStrategiesKey, string(raw)); err != nil {
		a.logf("could not persist running strategies (they will not restart after a reboot): %v", err)
	}
}

// RestoreStrategies restarts the instances that were running when the process
// last stopped. Returns the instances it refused to restart, for the UI.
//
// Called once market data is up rather than at boot: resuming needs the
// instrument master to resolve each open leg, and a subscription to receive the
// ticks that drive exits.
func (a *App) RestoreStrategies(ctx context.Context) []OrphanGroup {
	if a.Store == nil || a.Engine == nil {
		return nil
	}

	raw, found, err := a.Store.GetSetting(ctx, runningStrategiesKey)
	if err != nil {
		a.logf("read running strategies: %v", err)
		return nil
	}
	if !found || raw == "" {
		return nil
	}

	var saved []persistedStrategy
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		a.logf("saved running strategies are unreadable, not restoring: %v", err)
		return nil
	}

	held := a.positionsByStrategy(ctx)

	var refused []OrphanGroup
	for _, p := range saved {
		if _, running := a.Engine.StrategyStatusByID(p.InstanceID); running {
			continue // already up; nothing to restore
		}

		spec := engine.StrategySpec{
			InstanceID: p.InstanceID,
			Type:       p.Type,
			Params:     p.Params,
			Resume:     held[p.InstanceID],
		}
		if _, err := a.Engine.StartStrategy(ctx, spec); err != nil {
			// The engine refuses rather than starting an instance that would
			// re-enter. Surface it: an unmanaged position the operator knows
			// about beats a doubled one they do not.
			a.logf("could not restore strategy %s: %v", p.InstanceID, err)
			refused = append(refused, OrphanGroup{
				StrategyID: p.InstanceID,
				Positions:  held[p.InstanceID],
				Reason:     err.Error(),
			})
			continue
		}
		a.logf("restored strategy %s (%d open position(s) adopted)",
			p.InstanceID, len(held[p.InstanceID]))
	}
	return refused
}

// OrphanGroup is a strategy's open positions with nothing managing them.
type OrphanGroup struct {
	StrategyID string
	Positions  []broker.Position
	Reason     string
}

// Orphans reports open positions tagged with a strategy that is not running.
//
// Computed live rather than recorded at boot, so it clears the moment the
// operator starts the strategy or squares off, and appears if a strategy dies
// later — the condition is "nothing is managing this", whenever it becomes true.
//
// Manual positions carry an empty StrategyID and are never orphans: nothing was
// ever managing them, and saying otherwise would train the operator to dismiss
// this banner.
func (a *App) Orphans(ctx context.Context) []OrphanGroup {
	if a.Engine == nil {
		return nil
	}

	running := make(map[string]bool)
	for _, s := range a.Engine.ListStrategies() {
		if s.State == engine.StateRunning {
			running[s.InstanceID] = true
		}
	}

	var out []OrphanGroup
	for id, pos := range a.positionsByStrategy(ctx) {
		if running[id] {
			continue
		}
		out = append(out, OrphanGroup{StrategyID: id, Positions: pos})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StrategyID < out[j].StrategyID })
	return out
}

// positionsByStrategy groups open, strategy-tagged positions by instance.
func (a *App) positionsByStrategy(_ context.Context) map[string][]broker.Position {
	held := make(map[string][]broker.Position)
	for _, p := range a.Engine.Positions() {
		if !p.IsOpen() || p.StrategyID == "" {
			continue
		}
		held[p.StrategyID] = append(held[p.StrategyID], p)
	}
	return held
}

// logf writes an app-level line without requiring a logger to be configured.
func (a *App) logf(format string, args ...any) {
	if a.Log == nil {
		return
	}
	a.Log.Warn(fmt.Sprintf(format, args...))
}
