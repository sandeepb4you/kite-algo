package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"kite-algo/internal/events"
	"kite-algo/internal/marketdata"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// newTestClient returns a client already watching symbols, registered with hub.
func newTestClient(h *Hub, symbols ...string) *Client {
	c := &Client{hub: h, send: make(chan []byte, clientBuffer), syms: map[string]struct{}{}}
	for _, s := range symbols {
		c.syms[s] = struct{}{}
	}
	return c
}

// TestHubCoalescesTicks is the property that makes streaming an option chain to
// a browser viable at all: many ticks for one symbol within a flush window must
// collapse to a single frame carrying the newest price. Without it the browser
// would receive hundreds of frames a second and fall behind immediately.
func TestHubCoalescesTicks(t *testing.T) {
	bus := events.NewBus(nil)
	defer bus.Close()

	h := NewHub(bus, nil, 30*time.Millisecond, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newTestClient(h, "NIFTY 50")
	go h.Run(ctx)
	h.register <- c

	// 200 ticks for one symbol, all inside a single flush window.
	for i := 1; i <= 200; i++ {
		bus.Publish(events.Event{
			Kind:   events.KindTick,
			Symbol: "NIFTY 50",
			Tick:   &marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: float64(24000 + i)},
		})
	}

	select {
	case raw := <-c.send:
		var f struct {
			T string     `json:"t"`
			D []tickWire `json:"d"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if f.T != "ticks" {
			t.Fatalf("frame type = %q, want ticks", f.T)
		}
		if len(f.D) != 1 {
			t.Fatalf("frame carried %d ticks, want 1 coalesced entry", len(f.D))
		}
		if f.D[0].P != 24200 {
			t.Errorf("coalesced price = %v, want the newest (24200)", f.D[0].P)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no tick frame arrived")
	}

	// And no second frame should follow for the same batch.
	select {
	case extra := <-c.send:
		t.Errorf("a second frame was emitted for one batch: %s", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHubFiltersBySubscription checks a client is only sent symbols it asked
// for — otherwise every browser would receive the whole option chain.
func TestHubFiltersBySubscription(t *testing.T) {
	bus := events.NewBus(nil)
	defer bus.Close()

	h := NewHub(bus, nil, 20*time.Millisecond, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newTestClient(h, "NIFTY 50")
	go h.Run(ctx)
	h.register <- c

	bus.Publish(events.Event{Kind: events.KindTick, Symbol: "NIFTY BANK",
		Tick: &marketdata.Tick{TradingSymbol: "NIFTY BANK", LastPrice: 52000}})
	bus.Publish(events.Event{Kind: events.KindTick, Symbol: "NIFTY 50",
		Tick: &marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 24000}})

	select {
	case raw := <-c.send:
		var f struct {
			D []tickWire `json:"d"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		for _, tick := range f.D {
			if tick.S != "NIFTY 50" {
				t.Errorf("client received unsubscribed symbol %q", tick.S)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame arrived")
	}
}

// TestHubSurvivesAStalledClient is the load-bearing guarantee. The hub consumes
// engine events on the trading path's behalf; a browser that stops reading must
// degrade only itself. If this test hangs, a closed laptop lid can stall market
// data for every strategy in the process.
func TestHubSurvivesAStalledClient(t *testing.T) {
	bus := events.NewBus(nil)
	defer bus.Close()

	h := NewHub(bus, nil, 10*time.Millisecond, quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stalled := newTestClient(h, "NIFTY 50") // never drained
	healthy := newTestClient(h, "NIFTY 50")

	go h.Run(ctx)
	h.register <- stalled
	h.register <- healthy

	// Drain the healthy client concurrently for the duration of the run.
	var served atomic.Int64
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range healthy.send {
			served.Add(1)
		}
	}()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			bus.Publish(events.Event{
				Kind:   events.KindTick,
				Symbol: "NIFTY 50",
				Tick:   &marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: float64(i)},
			})
			if i%100 == 0 {
				// Let the hub flush, so the stalled client's queue fills up.
				time.Sleep(time.Millisecond)
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publishing stalled behind a client that stopped reading")
	}

	// Give the last flushes time to land, then shut the hub so the drain ends.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-drainDone

	if served.Load() == 0 {
		t.Error("the responsive client received nothing; the hub is not serving past a stalled peer")
	}
	// Note there is no assertion that the stalled client dropped frames. It
	// usually does not, and that is the design working: coalescing collapses
	// thousands of ticks into a handful of frames per second, so a client has to
	// be unresponsive for many seconds before a 128-deep queue overflows.
	// Drop behaviour is covered directly by TestClientTrySendDropsWhenFull.
	t.Logf("responsive client served %d frames; stalled client dropped %d",
		served.Load(), stalled.drops.Load())
}

// TestClientTrySendDropsWhenFull covers the back-pressure valve directly.
// trySend is called from the hub's single goroutine, which is also draining
// engine events, so it must never block regardless of client state.
func TestClientTrySendDropsWhenFull(t *testing.T) {
	c := &Client{send: make(chan []byte, 2), syms: map[string]struct{}{}}

	if !c.trySend([]byte("a")) || !c.trySend([]byte("b")) {
		t.Fatal("sends within capacity should succeed")
	}
	if c.drops.Load() != 0 {
		t.Errorf("drops = %d before overflow, want 0", c.drops.Load())
	}

	done := make(chan bool, 1)
	go func() { done <- c.trySend([]byte("c")) }()

	select {
	case ok := <-done:
		if ok {
			t.Error("trySend reported success on a full queue")
		}
	case <-time.After(time.Second):
		t.Fatal("trySend blocked on a full queue; the hub goroutine would stall")
	}

	if c.drops.Load() != 1 {
		t.Errorf("drops = %d after one overflow, want 1", c.drops.Load())
	}
}

// TestClientAddSymbolsIsIdempotent guards the subscription refcount against a
// client re-sending its whole watch list, which the browser does on reconnect.
func TestClientAddSymbolsIsIdempotent(t *testing.T) {
	c := &Client{send: make(chan []byte, 1), syms: map[string]struct{}{}}

	if added := c.addSymbols([]string{"NIFTY 50", "NIFTY BANK", ""}); len(added) != 2 {
		t.Fatalf("first add returned %v, want 2 symbols (empty ignored)", added)
	}
	if added := c.addSymbols([]string{"NIFTY 50", "NIFTY BANK"}); len(added) != 0 {
		t.Errorf("re-adding known symbols returned %v; refcounts would inflate", added)
	}
	if removed := c.removeSymbols([]string{"NIFTY 50", "UNKNOWN"}); len(removed) != 1 {
		t.Errorf("remove returned %v, want only the symbol actually held", removed)
	}
}

// TestHubReleasesSymbolsOnDisconnect ensures a departing browser's interest is
// given back, so upstream subscriptions do not leak as tabs come and go.
func TestHubReleasesSymbolsOnDisconnect(t *testing.T) {
	bus := events.NewBus(nil)
	defer bus.Close()

	h := NewHub(bus, nil, time.Second, quietLogger())
	c := newTestClient(h, "NIFTY 50", "NIFTY BANK")

	h.acquire(c.symbols())
	h.subsMu.Lock()
	got := len(h.subs)
	h.subsMu.Unlock()
	if got != 2 {
		t.Fatalf("tracked %d symbols after acquire, want 2", got)
	}

	h.releaseAll(c)

	h.subsMu.Lock()
	got = len(h.subs)
	h.subsMu.Unlock()
	if got != 0 {
		t.Errorf("%d symbols still tracked after the client left", got)
	}
	if syms := c.symbols(); len(syms) != 0 {
		t.Errorf("client still lists %v after release", syms)
	}
}

// TestHubRefcountsSharedSymbols checks two tabs watching the same symbol do not
// unsubscribe it when only one of them closes.
func TestHubRefcountsSharedSymbols(t *testing.T) {
	bus := events.NewBus(nil)
	defer bus.Close()

	h := NewHub(bus, nil, time.Second, quietLogger())
	a := newTestClient(h, "NIFTY 50")
	b := newTestClient(h, "NIFTY 50")

	h.acquire(a.symbols())
	h.acquire(b.symbols())
	h.releaseAll(a)

	h.subsMu.Lock()
	n := h.subs["NIFTY 50"]
	h.subsMu.Unlock()
	if n != 1 {
		t.Errorf("refcount = %d after one of two watchers left, want 1", n)
	}
}
