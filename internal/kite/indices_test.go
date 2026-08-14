package kite

import (
	"strings"
	"testing"

	"kite-algo/internal/marketdata"
)

// TestIndexTokensAreIndicesSegment guards the table against a token from the
// wrong segment. Kite encodes the exchange segment in the low byte and indices
// are 9; a wrong token is silently ignored by the exchange, so the subscription
// just never delivers ticks and nothing reports an error.
func TestIndexTokensAreIndicesSegment(t *testing.T) {
	if len(IndexTokens) == 0 {
		t.Fatal("IndexTokens is empty")
	}
	for name, tok := range IndexTokens {
		if !IsIndexToken(tok) {
			t.Errorf("index %q has token %d with segment %d; want segment 9",
				name, tok, tok&0xff)
		}
	}
}

// TestIndexTokenRoundTrip is the property whose absence made every index price
// read 0.00.
//
// Subscribing needs name → token; decoding the resulting tick needs token →
// name, because the engine drops any tick whose trading symbol is empty. Having
// only the first direction meant index ticks arrived and were discarded: the
// watchlist stayed at zero for ever, and a strategy keyed to a spot price never
// fired, with nothing logged to explain it.
func TestIndexTokenRoundTrip(t *testing.T) {
	for name, tok := range IndexTokens {
		gotToken, ok := IndexTokenFor(name)
		if !ok || gotToken != tok {
			t.Errorf("IndexTokenFor(%q) = %d, %v; want %d", name, gotToken, ok, tok)
		}
		gotName, ok := IndexSymbolFor(tok)
		if !ok {
			t.Errorf("token %d (%s) cannot be resolved back to a name; "+
				"its ticks would be dropped", tok, name)
			continue
		}
		if gotName != name {
			t.Errorf("IndexSymbolFor(%d) = %q, want %q", tok, gotName, name)
		}
	}
}

// TestEnrichTickNamesIndexTicks covers the decode path directly. The instrument
// master this platform loads is NFO-only, so an index token is never in it —
// the ticker must fall back to the index table or the tick is unusable.
func TestEnrichTickNamesIndexTicks(t *testing.T) {
	// An NFO-only master, exactly as the platform loads it.
	master, err := ParseInstruments(strings.NewReader(
		"instrument_token,tradingsymbol,name,expiry,strike,lot_size,instrument_type,segment,exchange,tick_size\n" +
			"12345,NIFTY25AUG24500CE,NIFTY,2025-08-28,24500,75,CE,NFO-OPT,NFO,0.05\n"))
	if err != nil {
		t.Fatalf("parse instruments: %v", err)
	}

	tk := &Ticker{instruments: master}

	// An option in the master resolves normally.
	opt := &marketdata.Tick{InstrumentToken: 12345}
	tk.enrichTick(opt)
	if opt.TradingSymbol != "NIFTY25AUG24500CE" {
		t.Errorf("option symbol = %q, want NIFTY25AUG24500CE", opt.TradingSymbol)
	}

	// An index is absent from the master and must still be named.
	idx := &marketdata.Tick{InstrumentToken: IndexTokens["NIFTY 50"]}
	tk.enrichTick(idx)
	if idx.TradingSymbol != "NIFTY 50" {
		t.Errorf("index symbol = %q, want %q — an unnamed tick is dropped by the "+
			"engine, so the spot price never appears", idx.TradingSymbol, "NIFTY 50")
	}
	if idx.Exchange == "" {
		t.Error("index tick has no exchange")
	}
}

// TestEnrichTickWithoutInstrumentsStillNamesIndices covers the window right
// after login, before the instrument master has loaded.
func TestEnrichTickWithoutInstrumentsStillNamesIndices(t *testing.T) {
	tk := &Ticker{}
	idx := &marketdata.Tick{InstrumentToken: IndexTokens["NIFTY BANK"]}
	tk.enrichTick(idx)
	if idx.TradingSymbol != "NIFTY BANK" {
		t.Errorf("symbol = %q, want NIFTY BANK", idx.TradingSymbol)
	}
}

// TestUnknownTokenStaysUnnamed: a token in neither place must not be given a
// made-up symbol, or it would be traded under a name that means nothing.
func TestUnknownTokenStaysUnnamed(t *testing.T) {
	tk := &Ticker{}
	unknown := &marketdata.Tick{InstrumentToken: 999999999}
	tk.enrichTick(unknown)
	if unknown.TradingSymbol != "" {
		t.Errorf("symbol = %q, want empty for an unknown token", unknown.TradingSymbol)
	}
}
