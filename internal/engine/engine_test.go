package engine

import (
	"slices"
	"testing"
)

// newTestEngine builds a bare Engine with no broker, ticker, or storage — the
// state a real one is in between process start and the operator completing the
// Zerodha login.
func newTestEngine() *Engine {
	return &Engine{
		prices:   make(map[string]float64),
		liveSeen: make(map[string]int),
		wanted:   make(map[string]struct{}),
		pinned:   make(map[string]struct{}),
	}
}

// TestSubscribeBeforeMarketDataIsRemembered covers the core invariant of eager
// boot: the process now starts before the operator has completed the Zerodha
// login, so strategies call Subscribe from Init while there is no ticker.
// Previously Subscribe returned early and threw the request away, which would
// leave a strategy permanently starved of the data it was written around.
func TestSubscribeBeforeMarketDataIsRemembered(t *testing.T) {
	e := newTestEngine()
	if e.HasMarketData() {
		t.Fatal("a bare engine should have no market data")
	}

	if err := e.Subscribe([]string{"NIFTY 50", "NIFTY24AUG24500CE"}); err != nil {
		t.Fatalf("Subscribe without a ticker should succeed, got %v", err)
	}
	// A second call must not duplicate, and empty symbols must be ignored.
	if err := e.Subscribe([]string{"NIFTY 50", ""}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	got := e.wantedSymbols()
	want := []string{"NIFTY 50", "NIFTY24AUG24500CE"}
	if !slices.Equal(got, want) {
		t.Errorf("pending subscriptions = %v, want %v", got, want)
	}
}

// TestUnsubscribeNeverReleasesStrategySymbols is the safety property behind
// browser-driven subscriptions. The UI subscribes and unsubscribes as tabs open
// and close; if that could release a symbol a strategy is trading, closing a tab
// would blind the strategy while it holds an open position.
func TestUnsubscribeNeverReleasesStrategySymbols(t *testing.T) {
	e := newTestEngine()

	// A strategy takes a position in an option leg (pinned)...
	if err := e.Subscribe([]string{"NIFTY24AUG24500CE"}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// ...and a browser tab happens to watch that leg plus an unrelated symbol.
	if err := e.SubscribeTransient([]string{"NIFTY24AUG24500CE", "NIFTY 50"}); err != nil {
		t.Fatalf("SubscribeTransient: %v", err)
	}

	// The tab closes and releases both.
	if err := e.Unsubscribe([]string{"NIFTY24AUG24500CE", "NIFTY 50"}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}

	got := e.wantedSymbols()
	want := []string{"NIFTY24AUG24500CE"}
	if !slices.Equal(got, want) {
		t.Errorf("after unsubscribe, streaming %v, want %v — the strategy's leg must survive", got, want)
	}
}

// TestSubscribeTransientDoesNotPin ensures the UI path stays releasable.
func TestSubscribeTransientDoesNotPin(t *testing.T) {
	e := newTestEngine()
	if err := e.SubscribeTransient([]string{"NIFTY 50"}); err != nil {
		t.Fatalf("SubscribeTransient: %v", err)
	}
	if err := e.Unsubscribe([]string{"NIFTY 50"}); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if got := e.wantedSymbols(); len(got) != 0 {
		t.Errorf("transient subscription survived unsubscribe: %v", got)
	}
}

// TestAttachMarketDataIgnoresNilTicker guards the detach/reattach path against
// wiping a live ticker when a session hand-off fails to produce one.
func TestAttachMarketDataIgnoresNilTicker(t *testing.T) {
	e := newTestEngine()
	e.AttachMarketData(nil, nil)
	if e.HasMarketData() {
		t.Error("attaching a nil ticker must not mark market data as connected")
	}
}

// TestKnownIndexTokensAreIndexSegment guards the index-token table. Kite encodes
// the exchange segment in an instrument token's low byte, and index quotes live
// in segment 9 (see priceDivisor in internal/kite/ticker.go).
//
// This test exists because the original table shipped three tokens from the
// wrong segment (NIFTY 50 = 256, NIFTY BANK = 260542, NIFTY FIN SERVICE = 257).
// A wrong token is not an error anyone sees: the exchange simply never sends
// ticks for it, so a strategy driven by that symbol idles forever while every
// log line looks healthy.
func TestKnownIndexTokensAreIndexSegment(t *testing.T) {
	if len(knownIndexTokens) == 0 {
		t.Fatal("knownIndexTokens is empty")
	}
	for name, tok := range knownIndexTokens {
		if !indexTokenValid(tok) {
			t.Errorf("index %q has token %d with segment %d; want segment %d",
				name, tok, tok&0xff, segIndices)
		}
	}
}

// TestKnownIndexTokensExact pins the specific values, so a well-formed but
// incorrect token (right segment, wrong index) still fails.
func TestKnownIndexTokensExact(t *testing.T) {
	want := map[string]uint32{
		"NIFTY 50":          256265,
		"NIFTY BANK":        260105,
		"NIFTY FIN SERVICE": 257801,
		"INDIA VIX":         264969,
	}
	for name, w := range want {
		got, ok := knownIndexTokens[name]
		if !ok {
			t.Errorf("index %q missing from knownIndexTokens", name)
			continue
		}
		if got != w {
			t.Errorf("index %q = %d, want %d", name, got, w)
		}
	}
}
