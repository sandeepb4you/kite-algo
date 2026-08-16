package kite

// Index instruments are not in any tradable instrument master.
//
// Kite's /instruments/NFO feed lists option and future contracts; the index
// quotes those contracts are written on (NIFTY 50, NIFTY BANK, …) live in the
// indices segment and appear in no exchange CSV this platform loads. Their
// tokens are stable and documented, so they are hard-coded here.
//
// This table is needed in BOTH directions, and missing either one breaks
// silently:
//
//   - name → token, to subscribe to the spot feed at all;
//   - token → name, because a tick whose trading symbol cannot be resolved is
//     dropped by the engine. Index ticks then arrive and are discarded, the
//     watchlist shows 0.00 for ever, and any strategy driven by a spot price
//     simply never triggers — with nothing logged to say why.
//
// Every token must be in the indices segment: Kite encodes the segment in the
// low byte, and indices are 9. TestIndexTokens enforces that.
// BSE indices are here too. SENSEX options trade on BFO, and capturing them
// needs the spot quote to locate the at-the-money strike — without an entry
// here the capture job cannot centre its strike window and skips the underlying
// entirely.
var IndexTokens = map[string]uint32{
	"NIFTY 50":          256265,
	"NIFTY BANK":        260105,
	"NIFTY FIN SERVICE": 257801,
	"NIFTY MID SELECT":  288009,
	"NIFTY NEXT 50":     270857,
	"INDIA VIX":         264969,
	"SENSEX":            265,
	"BANKEX":            274441,
}

// IndexExchange maps an index to the exchange its quote is published on, which
// is what /quote/ltp keys need ("NSE:NIFTY 50", "BSE:SENSEX"). Indices are not
// in any instrument CSV, so the exchange cannot be looked up like a contract's.
var IndexExchange = map[string]string{
	"NIFTY 50":          "NSE",
	"NIFTY BANK":        "NSE",
	"NIFTY FIN SERVICE": "NSE",
	"NIFTY MID SELECT":  "NSE",
	"NIFTY NEXT 50":     "NSE",
	"INDIA VIX":         "NSE",
	"SENSEX":            "BSE",
	"BANKEX":            "BSE",
}

// IndexExchangeFor returns the exchange an index quote is published on.
func IndexExchangeFor(name string) (string, bool) {
	ex, ok := IndexExchange[name]
	return ex, ok
}

// indexSymbols is the reverse lookup, built once at start-up.
var indexSymbols = func() map[uint32]string {
	out := make(map[uint32]string, len(IndexTokens))
	for name, token := range IndexTokens {
		out[token] = name
	}
	return out
}()

// IndexTokenFor returns the instrument token quoting an index by name.
func IndexTokenFor(name string) (uint32, bool) {
	tok, ok := IndexTokens[name]
	return tok, ok
}

// IndexSymbolFor returns the index name for a token, if it is a known index.
func IndexSymbolFor(token uint32) (string, bool) {
	name, ok := indexSymbols[token]
	return name, ok
}

// IsIndexToken reports whether a token belongs to the indices segment.
func IsIndexToken(token uint32) bool { return token&0xff == segIndices }
