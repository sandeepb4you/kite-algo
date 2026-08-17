package engine

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/events"
	"kite-algo/internal/strategy"
)

// StrategyState is where a strategy instance is in its lifecycle.
type StrategyState string

const (
	StateRunning  StrategyState = "running"
	StateStopping StrategyState = "stopping"
	StateStopped  StrategyState = "stopped"
	// StateErrored means Init failed or OnTick panicked. The instance is no
	// longer receiving market data.
	StateErrored StrategyState = "errored"
)

// StrategySpec asks the engine to start one strategy instance.
type StrategySpec struct {
	// InstanceID is the unique name for this instance and becomes the
	// StrategyID on its orders and positions. Defaults to Type.
	InstanceID string
	Type       string         // registry key
	Params     map[string]any // raw; normalized against the descriptor

	// Resume carries the instance's already-open positions when this start is a
	// restore after a restart rather than a fresh operator-initiated start.
	//
	// Non-empty means "you are picking up mid-trade": the instance must
	// implement strategy.Resumable or the start is refused, because a strategy
	// that cannot rebuild its state would treat the open position as absent and
	// enter again on top of it.
	Resume []broker.Position
}

// StopOptions controls how a strategy is shut down.
type StopOptions struct {
	// SquareOff flattens the strategy's open positions. This is deliberately an
	// explicit choice with no default: silently leaving short options open would
	// be dangerous, and silently closing them would be an unrequested trade.
	SquareOff bool
	Reason    string
}

// StrategyStatus is the engine's report on one instance, for the UI.
type StrategyStatus struct {
	InstanceID    string            `json:"instance_id"`
	Type          string            `json:"type"`
	Title         string            `json:"title"`
	State         StrategyState     `json:"state"`
	Error         string            `json:"error,omitempty"`
	Params        map[string]any    `json:"params"`
	StartedAt     time.Time         `json:"started_at"`
	StoppedAt     time.Time         `json:"stopped_at,omitempty"`
	Positions     []broker.Position `json:"positions"`
	RealizedPnL   float64           `json:"realized_pnl"`
	UnrealizedPnL float64           `json:"unrealized_pnl"`
	TotalPnL      float64           `json:"total_pnl"`
	OrderCount    int               `json:"order_count"`
	FillCount     int               `json:"fill_count"`
	TickCount     int64             `json:"tick_count"`
	LastSignal    *strategy.Signal  `json:"last_signal,omitempty"`
}

// strategyHandle is the engine's per-instance bookkeeping.
type strategyHandle struct {
	id     string
	typ    string
	title  string
	inst   strategy.Strategy
	params map[string]any
	cancel context.CancelFunc

	// Guarded by Engine.smu.
	state     StrategyState
	lastErr   string
	startedAt time.Time
	stoppedAt time.Time

	// initialized records that Init has already run for this instance.
	//
	// Start() initializes strategies added before it via AddStrategy, and used
	// to do so unconditionally. An instance created by StartStrategy is already
	// initialized, and a second Init is not harmless: shortstraddle's Init
	// resets its leg map, so re-initializing one that had adopted an open
	// position erased the adoption and let it enter again on top. That is
	// exactly what happened on 2026-08-17 — a redeploy produced a second live
	// straddle.
	initialized bool

	// Hot-path counters, read without the lock.
	ticks      atomic.Int64
	orders     atomic.Int64
	fills      atomic.Int64
	lastSignal atomic.Pointer[strategy.Signal]
}

// activeStrategies returns the current fan-out snapshot.
//
// handleTick and handleFill call this on the market-data goroutine for every
// tick, so it must not take a lock that an HTTP handler could be holding. The
// slice is replaced wholesale on every lifecycle change (copy-on-write) and
// never mutated in place, so readers always see a coherent set.
func (e *Engine) activeStrategies() []*strategyHandle {
	p := e.active.Load()
	if p == nil {
		return nil
	}
	return *p
}

// rebuildActive recomputes the fan-out snapshot. Caller holds smu.
func (e *Engine) rebuildActiveLocked() {
	live := make([]*strategyHandle, 0, len(e.handles))
	for _, h := range e.handles {
		if h.state == StateRunning {
			live = append(live, h)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].id < live[j].id })
	e.active.Store(&live)
}

// StartStrategy builds, initializes, and starts a strategy instance.
//
// Init runs before the instance is published to the fan-out snapshot, so a
// strategy that fails to initialize never receives a tick, and a partially
// constructed instance is never visible to the market-data goroutine.
func (e *Engine) StartStrategy(ctx context.Context, spec StrategySpec) (StrategyStatus, error) {
	if e.registry == nil {
		return StrategyStatus{}, fmt.Errorf("no strategy registry configured")
	}
	id := spec.InstanceID
	if id == "" {
		id = spec.Type
	}

	desc, ok := e.registry.Get(spec.Type)
	if !ok {
		return StrategyStatus{}, fmt.Errorf("unknown strategy type %q", spec.Type)
	}
	params, err := desc.Normalize(spec.Params)
	if err != nil {
		return StrategyStatus{}, err
	}

	e.smu.Lock()
	defer e.smu.Unlock()

	if existing, dup := e.handles[id]; dup && existing.state == StateRunning {
		return StrategyStatus{}, fmt.Errorf("strategy %q is already running", id)
	}

	inst, _, err := e.registry.New(spec.Type, id, e.logger)
	if err != nil {
		return StrategyStatus{}, err
	}

	parent := e.runCtx
	if parent == nil {
		parent = context.Background()
	}
	sctx, cancel := context.WithCancel(parent)

	h := &strategyHandle{
		id:        id,
		typ:       spec.Type,
		title:     desc.Title,
		inst:      inst,
		params:    params,
		cancel:    cancel,
		state:     StateRunning,
		startedAt: time.Now(),
	}

	cfg := config.StrategyCfg{Name: id, Enabled: true, Params: params}
	h.initialized = true // whatever Init returns, it has now run for this instance
	if err := inst.Init(sctx, e, cfg); err != nil {
		cancel()
		h.state = StateErrored
		h.lastErr = err.Error()
		e.handles[id] = h
		return e.statusLocked(h), fmt.Errorf("initialize %s: %w", id, err)
	}

	// Restoring mid-trade. Rebuild the instance's session state from what it
	// already holds, BEFORE it is published to the fan-out snapshot — a tick
	// arriving against a strategy that believes it is flat, while its legs are
	// open, is the double-entry this whole path exists to prevent.
	if len(spec.Resume) > 0 {
		r, ok := inst.(strategy.Resumable)
		if !ok {
			cancel()
			return StrategyStatus{}, fmt.Errorf(
				"cannot resume %s: %d position(s) are open and this strategy "+
					"cannot rebuild its state; stop it or square off by hand",
				id, len(spec.Resume))
		}
		if err := r.Resume(sctx, spec.Resume); err != nil {
			cancel()
			h.state = StateErrored
			h.lastErr = err.Error()
			e.handles[id] = h
			return e.statusLocked(h), fmt.Errorf("resume %s: %w", id, err)
		}
	}

	e.handles[id] = h
	e.rebuildActiveLocked()

	if e.logger != nil {
		e.logger.Info("strategy started", "id", id, "type", spec.Type, "params", params)
	}
	e.pub.Publish(events.Event{
		Kind:       events.KindStatus,
		StrategyID: id,
		Level:      events.LevelInfo,
		Message:    "strategy " + id + " started",
		Fields:     map[string]any{"strategy_state": string(StateRunning)},
	})
	return e.statusLocked(h), nil
}

// StopStrategy shuts an instance down, optionally flattening its positions.
//
// The instance is removed from the fan-out snapshot first, so it cannot open a
// new position while it is being unwound.
func (e *Engine) StopStrategy(ctx context.Context, instanceID string, opt StopOptions) (StrategyStatus, error) {
	e.smu.Lock()
	h, ok := e.handles[instanceID]
	if !ok {
		e.smu.Unlock()
		return StrategyStatus{}, fmt.Errorf("no strategy instance %q", instanceID)
	}
	if h.state == StateStopped {
		st := e.statusLocked(h)
		e.smu.Unlock()
		return st, nil
	}
	h.state = StateStopping
	e.rebuildActiveLocked() // no further ticks or fills reach it from here
	inst := h.inst
	e.smu.Unlock()

	// Flatten before tearing down, while the strategy can still be asked to
	// unwind its own legs in the right order.
	var flattenErr error
	if opt.SquareOff {
		flattenErr = e.flattenStrategy(ctx, instanceID, inst, opt.Reason)
	}

	if err := inst.Stop(ctx); err != nil && e.logger != nil {
		e.logger.Error("strategy stop failed", "id", instanceID, "err", err)
	}

	e.smu.Lock()
	h.cancel()
	h.state = StateStopped
	h.stoppedAt = time.Now()
	if flattenErr != nil {
		h.lastErr = flattenErr.Error()
	}
	st := e.statusLocked(h)
	e.rebuildActiveLocked()
	e.smu.Unlock()

	if e.logger != nil {
		e.logger.Warn("strategy stopped",
			"id", instanceID, "square_off", opt.SquareOff, "reason", opt.Reason)
	}
	e.pub.Publish(events.Event{
		Kind:       events.KindStatus,
		StrategyID: instanceID,
		Level:      events.LevelWarn,
		Message:    "strategy " + instanceID + " stopped",
		Fields:     map[string]any{"strategy_state": string(StateStopped), "squared_off": opt.SquareOff},
	})
	return st, flattenErr
}

// flattenStrategy unwinds one strategy's positions, preferring the strategy's
// own square-off logic when it has any.
func (e *Engine) flattenStrategy(ctx context.Context, id string, inst strategy.Strategy, reason string) error {
	if f, ok := inst.(strategy.Flattener); ok {
		if err := f.SquareOff(ctx, reason); err != nil {
			return fmt.Errorf("strategy square-off: %w", err)
		}
		// Fall through: the engine still sweeps for anything the strategy's own
		// unwind missed, so a partial flatten is not mistaken for a complete one.
	}

	var failures []error
	for _, p := range e.Positions() {
		if p.StrategyID != id || !p.IsOpen() {
			continue
		}
		if _, err := e.flatten(ctx, p); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", p.TradingSymbol, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("could not flatten %d position(s): %v", len(failures), failures)
	}
	return nil
}

// ListStrategies reports every known instance, sorted by id.
func (e *Engine) ListStrategies() []StrategyStatus {
	e.smu.Lock()
	defer e.smu.Unlock()

	out := make([]StrategyStatus, 0, len(e.handles))
	for _, h := range e.handles {
		out = append(out, e.statusLocked(h))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

// StrategyStatusByID reports one instance.
func (e *Engine) StrategyStatusByID(id string) (StrategyStatus, bool) {
	e.smu.Lock()
	defer e.smu.Unlock()
	h, ok := e.handles[id]
	if !ok {
		return StrategyStatus{}, false
	}
	return e.statusLocked(h), true
}

// statusLocked builds a status report. Caller holds smu.
//
// Everything here is derived on demand from the position book and the handle's
// counters; nothing is cached, because a stale P&L figure on a trading screen is
// worse than a slightly expensive one.
func (e *Engine) statusLocked(h *strategyHandle) StrategyStatus {
	st := StrategyStatus{
		InstanceID: h.id,
		Type:       h.typ,
		Title:      h.title,
		State:      h.state,
		Error:      h.lastErr,
		Params:     h.params,
		StartedAt:  h.startedAt,
		StoppedAt:  h.stoppedAt,
		OrderCount: int(h.orders.Load()),
		FillCount:  int(h.fills.Load()),
		TickCount:  h.ticks.Load(),
		LastSignal: h.lastSignal.Load(),
	}

	prices := e.Prices()
	for _, p := range e.Positions() {
		if p.StrategyID != h.id {
			continue
		}
		if p.IsOpen() {
			st.Positions = append(st.Positions, p)
			if last, ok := prices[p.TradingSymbol]; ok && last > 0 {
				st.UnrealizedPnL += positionPnL(p, last) - p.PnL
			}
		}
		st.RealizedPnL += p.PnL
	}
	st.TotalPnL = st.RealizedPnL + st.UnrealizedPnL
	return st
}

// StopAllStrategies shuts every running instance down. Used at shutdown and by
// the kill switch.
func (e *Engine) StopAllStrategies(ctx context.Context, opt StopOptions) []error {
	e.smu.Lock()
	ids := make([]string, 0, len(e.handles))
	for id, h := range e.handles {
		if h.state == StateRunning {
			ids = append(ids, id)
		}
	}
	e.smu.Unlock()
	sort.Strings(ids)

	var errs []error
	for _, id := range ids {
		if _, err := e.StopStrategy(ctx, id, opt); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
