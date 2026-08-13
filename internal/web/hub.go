package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"kite-algo/internal/engine"
	"kite-algo/internal/events"
	"kite-algo/internal/marketdata"
)

// clientBuffer is the per-connection outbound queue depth.
const clientBuffer = 128

// Frame is one message pushed to a browser. Keys are short because tick frames
// go out several times a second per client.
type Frame struct {
	T string `json:"t"`           // frame type: ticks | order | fill | positions | status | hello
	D any    `json:"d,omitempty"` // payload
}

// tickWire is the compact on-the-wire form of a tick. The full marketdata.Tick
// carries depth and timestamps the browser has no use for at 5 Hz.
type tickWire struct {
	S  string  `json:"s"`           // trading symbol
	P  float64 `json:"p"`           // last price
	C  float64 `json:"c,omitempty"` // change from previous close, percent
	O  float64 `json:"o,omitempty"` // day open
	H  float64 `json:"h,omitempty"` // day high
	L  float64 `json:"l,omitempty"` // day low
	V  int64   `json:"v,omitempty"` // day volume
	TS int64   `json:"ts"`          // unix millis
}

// Hub fans engine events out to connected browsers.
//
// A single goroutine owns all shared state, so there are no locks on the
// broadcast path. Two properties matter more than anything else here:
//
//   - Ticks are COALESCED. An option chain produces far more updates per second
//     than a screen can display or a browser can paint, so only the newest tick
//     per symbol survives until the next flush.
//   - Delivery is LOSSY for ticks and fatal-on-backpressure for everything else.
//     A tick that cannot be delivered is dropped, because the next flush carries
//     the current price anyway. An order or fill that cannot be delivered closes
//     the connection instead — a silently missing fill would leave the operator
//     looking at an order book that disagrees with reality.
type Hub struct {
	bus     *events.Bus
	eng     *engine.Engine
	log     *slog.Logger
	flushIn time.Duration

	register   chan *Client
	unregister chan *Client

	// Owned exclusively by run(); never touched from another goroutine.
	clients map[*Client]struct{}
	pending map[string]marketdata.Tick

	// subs reference-counts which symbols browsers are watching, so the Kite
	// subscription can be released when the last watcher goes away.
	subsMu sync.Mutex
	subs   map[string]int
}

// NewHub builds a hub. flushInterval bounds how often tick frames are emitted.
func NewHub(bus *events.Bus, eng *engine.Engine, flushInterval time.Duration, log *slog.Logger) *Hub {
	if flushInterval <= 0 {
		flushInterval = 200 * time.Millisecond
	}
	return &Hub{
		bus:        bus,
		eng:        eng,
		log:        log,
		flushIn:    flushInterval,
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]struct{}),
		pending:    make(map[string]marketdata.Tick),
		subs:       make(map[string]int),
	}
}

// Run pumps events to clients until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) {
	evCh, cancel := h.bus.Subscribe(1024)
	defer cancel()

	flush := time.NewTicker(h.flushIn)
	defer flush.Stop()

	for {
		select {
		case <-ctx.Done():
			for c := range h.clients {
				close(c.send)
				delete(h.clients, c)
			}
			return

		case c := <-h.register:
			h.clients[c] = struct{}{}
			h.log.Debug("ws client connected", "clients", len(h.clients))

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				h.releaseAll(c)
				h.log.Debug("ws client disconnected", "clients", len(h.clients))
			}

		case ev, ok := <-evCh:
			if !ok {
				return
			}
			h.dispatch(ev)

		case <-flush.C:
			h.flushTicks()
		}
	}
}

// dispatch routes one engine event.
func (h *Hub) dispatch(ev events.Event) {
	switch ev.Kind {
	case events.KindTick:
		// Coalesce rather than send: keep only the newest per symbol.
		if ev.Tick != nil {
			h.pending[ev.Tick.TradingSymbol] = *ev.Tick
		}

	case events.KindOrder:
		h.broadcastCritical(Frame{T: "order", D: ev.Order})

	case events.KindOrderRejected:
		h.broadcastCritical(Frame{T: "rejected", D: map[string]any{
			"symbol": ev.Symbol, "strategy": ev.StrategyID,
			"message": ev.Message, "fields": ev.Fields,
		}})

	case events.KindFill:
		h.broadcastCritical(Frame{T: "fill", D: ev.Fill})

	case events.KindPositions:
		h.broadcast(Frame{T: "positions", D: map[string]any{
			"positions": ev.Positions, "day_pnl": ev.DayPnL,
		}})

	case events.KindStatus:
		h.broadcast(Frame{T: "status", D: map[string]any{
			"level": ev.Level, "message": ev.Message, "fields": ev.Fields,
		}})

	case events.KindSignal:
		h.broadcast(Frame{T: "signal", D: map[string]any{
			"strategy": ev.StrategyID, "symbol": ev.Symbol,
			"level": ev.Level, "message": ev.Message,
		}})
	}
}

// flushTicks emits one coalesced frame per client, filtered to that client's
// symbols, then clears the pending set.
func (h *Hub) flushTicks() {
	if len(h.pending) == 0 {
		return
	}
	for c := range h.clients {
		wire := make([]tickWire, 0, 8)
		for sym, tk := range h.pending {
			if !c.watching(sym) {
				continue
			}
			wire = append(wire, toWire(tk))
		}
		if len(wire) == 0 {
			continue
		}
		if b, err := json.Marshal(Frame{T: "ticks", D: wire}); err == nil {
			c.trySend(b) // lossy: the next flush carries the current price
		}
	}
	clear(h.pending)
}

func toWire(tk marketdata.Tick) tickWire {
	w := tickWire{
		S:  tk.TradingSymbol,
		P:  tk.LastPrice,
		O:  tk.OHLC.Open,
		H:  tk.OHLC.High,
		L:  tk.OHLC.Low,
		V:  tk.Volume,
		TS: tk.Timestamp.UnixMilli(),
	}
	if tk.OHLC.Close > 0 {
		w.C = (tk.LastPrice - tk.OHLC.Close) / tk.OHLC.Close * 100
	}
	if w.TS == 0 {
		w.TS = time.Now().UnixMilli()
	}
	return w
}

// broadcast sends to every client, dropping on backpressure.
func (h *Hub) broadcast(f Frame) {
	b, err := json.Marshal(f)
	if err != nil {
		h.log.Warn("marshal frame failed", "type", f.T, "err", err)
		return
	}
	for c := range h.clients {
		c.trySend(b)
	}
}

// broadcastCritical sends frames that must not be silently lost. A client whose
// queue is full is disconnected; it reconnects within a second and re-fetches a
// full snapshot over HTTP, which resynchronises it by construction.
func (h *Hub) broadcastCritical(f Frame) {
	b, err := json.Marshal(f)
	if err != nil {
		h.log.Warn("marshal frame failed", "type", f.T, "err", err)
		return
	}
	for c := range h.clients {
		if !c.trySend(b) {
			h.log.Warn("dropping ws client that fell behind on order flow",
				"frame", f.T)
			go func(c *Client) { h.unregister <- c }(c)
		}
	}
}

// acquire streams symbols for a client, subscribing upstream on first interest.
func (h *Hub) acquire(symbols []string) {
	var fresh []string
	h.subsMu.Lock()
	for _, s := range symbols {
		h.subs[s]++
		if h.subs[s] == 1 {
			fresh = append(fresh, s)
		}
	}
	h.subsMu.Unlock()

	if len(fresh) > 0 && h.eng != nil {
		if err := h.eng.SubscribeTransient(fresh); err != nil {
			h.log.Warn("subscribe for ui failed", "err", err)
		}
	}
}

// release drops a client's interest, unsubscribing upstream when it was the last.
func (h *Hub) release(symbols []string) {
	var dead []string
	h.subsMu.Lock()
	for _, s := range symbols {
		if h.subs[s] > 0 {
			h.subs[s]--
			if h.subs[s] == 0 {
				delete(h.subs, s)
				dead = append(dead, s)
			}
		}
	}
	h.subsMu.Unlock()

	// Engine.Unsubscribe ignores symbols pinned by a strategy, so this can
	// never cut market data to something holding a position.
	if len(dead) > 0 && h.eng != nil {
		if err := h.eng.Unsubscribe(dead); err != nil {
			h.log.Warn("unsubscribe for ui failed", "err", err)
		}
	}
}

// releaseAll drops every symbol a departing client was watching.
func (h *Hub) releaseAll(c *Client) {
	h.release(c.symbols())
	c.clearSymbols()
}
