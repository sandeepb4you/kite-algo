package history

import (
	"strings"
	"testing"

	"kite-algo/internal/kite"
)

// TestResolveFindsIndexSymbols covers the gap that made every index-driven
// backtest silently empty.
//
// Kite's instrument CSVs list tradable contracts; the index a strategy watches
// for its spot price ("NIFTY 50") is in none of them, so the lookup failed and
// the feed loaded no candles. The run then reported a clean zero-trade result,
// which reads as "the strategy did nothing" rather than "there was no data".
func TestResolveFindsIndexSymbols(t *testing.T) {
	p := NewKiteProvider(nil, nil, nil) // no client, no master: index path only

	for name, want := range kite.IndexTokens {
		token, err := p.resolve(name)
		if err != nil {
			t.Errorf("resolve(%q): %v", name, err)
			continue
		}
		if token != want {
			t.Errorf("resolve(%q) = %d, want %d", name, token, want)
		}
	}
}

// TestResolveStillNeedsAMasterForContracts: the index table must not paper over
// a missing instrument master, or an unresolvable option would look like a
// symbol that simply has no data.
func TestResolveStillNeedsAMasterForContracts(t *testing.T) {
	p := NewKiteProvider(nil, nil, nil)

	_, err := p.resolve("NIFTY2681824350CE")
	if err == nil {
		t.Fatal("an option resolved without an instrument master")
	}
	if !strings.Contains(err.Error(), "instrument master") {
		t.Errorf("error %q does not point at the missing master", err)
	}
}
