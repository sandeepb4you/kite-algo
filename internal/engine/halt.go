package engine

import (
	"context"
	"sync"
	"time"

	"kite-algo/internal/events"
	"kite-algo/internal/risk"
)

// HaltOptions configures the kill switch.
type HaltOptions struct {
	Reason         string
	By             string // "operator" | "risk"
	StopStrategies bool   // also stop every running strategy
	SquareOffAll   bool   // also flatten every open position
}

// HaltState describes the current trading halt, if any.
type HaltState struct {
	Halted     bool      `json:"halted"`
	Reason     string    `json:"reason,omitempty"`
	By         string    `json:"by,omitempty"`
	At         time.Time `json:"at,omitempty"`
	SquaredOff bool      `json:"squared_off,omitempty"`
}

// haltGuard holds the kill-switch state.
type haltGuard struct {
	mu    sync.RWMutex
	state HaltState
}

// Halt blocks all new orders and, if asked, stops strategies and flattens the
// book.
//
// The order of operations matters: the flag is set first, so a strategy reacting
// to the very next tick cannot slip an order in while the book is being
// unwound. Square-off orders bypass the flag through placeOrderInternal.
func (e *Engine) Halt(ctx context.Context, opt HaltOptions) (HaltState, []error) {
	e.halt.mu.Lock()
	e.halt.state = HaltState{
		Halted: true,
		Reason: opt.Reason,
		By:     opt.By,
		At:     time.Now(),
	}
	e.halt.mu.Unlock()

	if e.logger != nil {
		e.logger.Warn("TRADING HALTED",
			"reason", opt.Reason, "by", opt.By,
			"stop_strategies", opt.StopStrategies, "square_off", opt.SquareOffAll)
	}

	var errs []error
	if opt.StopStrategies {
		errs = append(errs, e.StopAllStrategies(ctx, StopOptions{
			SquareOff: false, // handled below in one sweep, so nothing is closed twice
			Reason:    "kill switch: " + opt.Reason,
		})...)
	}
	if opt.SquareOffAll {
		_, sqErrs := e.SquareOffAll(ctx)
		errs = append(errs, sqErrs...)

		e.halt.mu.Lock()
		e.halt.state.SquaredOff = len(sqErrs) == 0
		e.halt.mu.Unlock()
	}

	state := e.HaltState()
	e.pub.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelError,
		Message: "TRADING HALTED — " + opt.Reason,
		Fields:  map[string]any{"halted": true, "by": opt.By, "squared_off": state.SquaredOff},
	})
	return state, errs
}

// Resume lifts a halt. Strategies stopped by the halt are not restarted: which
// of them should trade again is the operator's decision, not a side effect.
func (e *Engine) Resume(by string) HaltState {
	e.halt.mu.Lock()
	e.halt.state = HaltState{}
	e.halt.mu.Unlock()

	if e.logger != nil {
		e.logger.Warn("trading resumed", "by", by)
	}
	e.pub.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelWarn,
		Message: "trading resumed",
		Fields:  map[string]any{"halted": false},
	})
	return HaltState{}
}

// HaltState reports the current halt.
func (e *Engine) HaltState() HaltState {
	e.halt.mu.RLock()
	defer e.halt.mu.RUnlock()
	return e.halt.state
}

// IsHalted reports whether new orders are currently blocked.
func (e *Engine) IsHalted() bool {
	e.halt.mu.RLock()
	defer e.halt.mu.RUnlock()
	return e.halt.state.Halted
}

// haltError builds the rejection returned to a strategy that tries to trade
// during a halt. It is a *risk.RiskError so the UI renders it identically to
// every other pre-trade rejection.
func (e *Engine) haltError() error {
	s := e.HaltState()
	return &risk.RiskError{
		Rule:    "kill-switch",
		Message: "trading is halted (" + s.Reason + "); resume in the UI to trade again",
	}
}
