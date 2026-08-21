package engine

import (
	"sync"

	"kite-algo/internal/broker"
	"kite-algo/internal/risk"
)

// Order routing across two books.
//
// The platform can run with real money and simulated money at the same time:
// orders typed by hand go to the exchange while strategies stay on the paper
// broker. That is the shape an operator wants while a strategy is still being
// evaluated — real discretionary trading, simulated automation, one screen.
//
// Reaching the exchange takes THREE independent conditions, all required:
//
//  1. the order is manual (StrategyID == ManualStrategyID);
//  2. live routing is armed (a live broker is installed);
//  3. the order itself asked for it (req.Book == BookReal).
//
// Anything else is simulated. Condition 3 is what keeps manual trading usable
// while strategies are still being evaluated: an operator can place a manual
// order into the PAPER book to try something by hand, on the same screen, on a
// day when live is armed. Defaulting an unspecified book to paper means the
// failure mode of forgetting is a simulated order, never a real one.
//
// Condition 1 keys off StrategyID rather than a per-strategy setting because
// there is then no configuration and no form field by which a strategy can
// reach the exchange. Taking strategies live — the eventual goal — is a
// deliberate change to this function, reviewed and deployed, not a checkbox
// somebody can mis-click at 09:15.

// routeMode describes how orders are being routed, for display and logging.
type routeMode string

const (
	// RouteAllPaper simulates everything. The default.
	RouteAllPaper routeMode = "all-paper"
	// RouteRealExitOnly means the real book is reachable for CLOSING orders
	// only: live routing was armed and has since been stood down, and the
	// broker is kept so open real positions can still be squared off.
	RouteRealExitOnly routeMode = "real-exit-only"
	// RouteManualLive sends hand-typed orders to the exchange and keeps every
	// strategy simulated.
	//
	// There is deliberately no "all live" mode. Routing a strategy to the
	// exchange would have to be a change to bookFor below, reviewed and
	// deployed — not a flag, a config value, or a checkbox on a form.
	RouteManualLive routeMode = "manual-live"
)

// orderBooks remembers which book each order was routed to, so a later cancel
// or modify reaches the broker that actually holds it.
//
// Without this, cancelling a live order while routing is mixed would be sent to
// the paper broker, silently succeed against nothing, and leave a real working
// order at the exchange that the UI reports as cancelled.
type orderBooks struct {
	mu   sync.RWMutex
	book map[string]broker.Book
}

func newOrderBooks() *orderBooks {
	return &orderBooks{book: make(map[string]broker.Book)}
}

func (o *orderBooks) set(orderID string, b broker.Book) {
	if orderID == "" {
		return
	}
	o.mu.Lock()
	o.book[orderID] = b
	o.mu.Unlock()
}

func (o *orderBooks) get(orderID string) (broker.Book, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	b, ok := o.book[orderID]
	return b, ok
}

// SetLiveBroker installs (or removes, with nil) the real-money broker used for
// manual orders, and arms or disarms real entries with it. Strategies are
// unaffected: they always use the paper broker.
//
// To stand down without losing the ability to CLOSE a real position, use
// DisarmLiveEntries instead.
func (e *Engine) SetLiveBroker(b broker.Broker) {
	e.cmu.Lock()
	e.liveBroker = b
	e.liveArmed = b != nil
	e.cmu.Unlock()

	if e.logger != nil {
		if b == nil {
			e.logger.Warn("live routing removed; manual orders are simulated again")
		} else {
			e.logger.Warn("LIVE ROUTING ENABLED for manual orders — " +
				"hand-typed orders now reach the exchange; strategies remain simulated")
		}
	}
}

// liveBrokerOrNil returns the installed live broker, if any.
func (e *Engine) liveBrokerOrNil() broker.Broker {
	e.cmu.RLock()
	defer e.cmu.RUnlock()
	return e.liveBroker
}

// DisarmLiveEntries closes the real book to NEW orders while keeping it
// reachable for exits.
//
// Deliberately not SetLiveBroker(nil). Dropping the broker also drops the only
// route to the exchange, so every attempt to close a real position failed with
// ErrNoLiveBroker and the real book vanished from the desk entirely — the
// engine polls it only while the broker is installed. An operator who had stood
// down was then holding real exposure the UI could neither show nor close, and
// the way out was to arm live again, phrase and password, just to get flat.
// De-escalating must never require escalating first.
//
// Entries are blocked in two independent places once this is called: bookFor
// sends new manual orders to the paper book, and placeOrderIn refuses a real
// order that is not IntentClose outright.
func (e *Engine) DisarmLiveEntries() {
	e.cmu.Lock()
	e.liveArmed = false
	installed := e.liveBroker != nil
	e.cmu.Unlock()

	if e.logger != nil && installed {
		e.logger.Warn("live entries disarmed; the real book stays reachable for " +
			"CLOSING orders only — new manual orders are simulated again")
	}
}

// liveEntriesArmed reports whether a NEW real order may be routed.
func (e *Engine) liveEntriesArmed() bool {
	e.cmu.RLock()
	defer e.cmu.RUnlock()
	return e.liveArmed && e.liveBroker != nil
}

// RealExitOnly reports that the real book can be closed but not opened: the
// live broker is still installed, and entries are disarmed.
//
// The UI needs this to tell two states apart that look identical from
// LiveActive alone — "never armed today, there is nothing real to manage" and
// "stood down holding real positions, which you can still close here".
func (e *Engine) RealExitOnly() bool {
	e.cmu.RLock()
	defer e.cmu.RUnlock()
	return !e.liveArmed && e.liveBroker != nil
}

// RouteMode reports how orders are currently routed.
func (e *Engine) RouteMode() routeMode {
	switch {
	case e.liveEntriesArmed():
		return RouteManualLive
	case e.RealExitOnly():
		return RouteRealExitOnly
	default:
		return RouteAllPaper
	}
}

// LiveManualActive reports whether hand-typed orders reach the exchange.
func (e *Engine) LiveManualActive() bool { return e.liveEntriesArmed() }

// bookFor reports which book an order request will be routed to.
//
// Keys off liveEntriesArmed, not merely on a live broker being installed: after
// a disarm the broker is deliberately kept for exits, and inferring "armed"
// from its presence would have quietly re-opened the real book to new manual
// orders the moment standing down stopped dropping it.
func (e *Engine) bookFor(req broker.OrderRequest) broker.Book {
	if !e.liveEntriesArmed() {
		return broker.BookPaper
	}
	if req.StrategyID == ManualStrategyID && req.Book.IsReal() {
		return broker.BookReal
	}
	return broker.BookPaper
}

// brokerForBook returns the broker that holds one book.
//
// It REFUSES rather than substituting when the real book is asked for and no live
// broker exists. That looks like the opposite of the fallback rule one level up
// in bookFor, and it is: the two are answering different questions.
//
// bookFor decides where a NEW order should go, and falling back to paper there is
// the safe direction — a manual order quietly simulated is a missing trade, while
// a strategy order quietly sent live is real money moved by something nobody
// authorised. But this function is reached once a book has already been decided,
// including by the engine closing a position it can see in the real book. Silently
// substituting the paper broker there does not simulate the close, it invents an
// unrelated paper position and reports success while the real exposure stays open.
// That is strictly worse than failing, so it fails.
func (e *Engine) brokerForBook(b broker.Book) (broker.Broker, error) {
	if b.IsReal() {
		if live := e.liveBrokerOrNil(); live != nil {
			return live, nil
		}
		return nil, ErrNoLiveBroker
	}
	return e.paperBrokerOrCurrent(), nil
}

// brokerForOrder resolves the broker holding an existing order.
func (e *Engine) brokerForOrder(orderID string) broker.Broker {
	if b, ok := e.orderBooks.get(orderID); ok && b == broker.BookReal {
		if live := e.liveBrokerOrNil(); live != nil {
			return live
		}
	}
	return e.paperBrokerOrCurrent()
}

// paperBrokerOrCurrent returns the simulated broker, falling back to whatever
// broker was supplied at construction (a backtest installs its own).
func (e *Engine) paperBrokerOrCurrent() broker.Broker {
	e.cmu.RLock()
	defer e.cmu.RUnlock()
	if e.paperBroker != nil {
		return e.paperBroker
	}
	return e.broker
}

// booksInUse lists the brokers to poll for positions and open orders, each with
// the book it represents.
func (e *Engine) booksInUse() []struct {
	Broker broker.Broker
	Book   broker.Book
} {
	type entry = struct {
		Broker broker.Broker
		Book   broker.Book
	}
	out := []entry{{Broker: e.paperBrokerOrCurrent(), Book: broker.BookPaper}}
	if live := e.liveBrokerOrNil(); live != nil {
		out = append(out, entry{Broker: live, Book: broker.BookReal})
	}
	return out
}

// SetPaperRisk installs the risk manager for the simulated book. Passing nil
// makes the simulated book reuse the real limits.
func (e *Engine) SetPaperRisk(m *risk.Manager) {
	e.cmu.Lock()
	e.paperRisk = m
	e.cmu.Unlock()
}

// riskFor returns the risk manager governing a book.
func (e *Engine) riskFor(b broker.Book) *risk.Manager {
	if b.IsReal() {
		return e.risk
	}
	e.cmu.RLock()
	pr := e.paperRisk
	e.cmu.RUnlock()
	if pr != nil {
		return pr
	}
	return e.risk
}

// SetLiveGate installs a predicate consulted before every real-money ENTRY.
//
// Separate from the risk manager because it answers a different question. The
// risk manager evaluates one order against limits; this asks whether the real
// book is open for business at all — a daily-loss lockout, or an account whose
// balance is not yet known so the percentage limit cannot be computed. It fails
// closed: no gate installed means live entries proceed to the risk manager as
// before, but a gate that says no is final.
func (e *Engine) SetLiveGate(fn func() (bool, string)) {
	e.cmu.Lock()
	e.liveGate = fn
	e.cmu.Unlock()
}

// liveGateAllows consults the gate, defaulting to allow when none is set.
func (e *Engine) liveGateAllows() (bool, string) {
	e.cmu.RLock()
	fn := e.liveGate
	e.cmu.RUnlock()
	if fn == nil {
		return true, ""
	}
	return fn()
}

// LiquidationOrder sequences positions so SHORT legs are covered first,
// exported for callers that flatten a subset (the expiry sweeper) and must not
// lose that ordering.
func (e *Engine) LiquidationOrder(positions []broker.Position) []broker.Position {
	return liquidationOrder(positions)
}
