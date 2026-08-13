// Package marketdata defines the core market-data types shared across the
// platform: ticks streamed from the broker, and OHLC candles aggregated from
// them. These types are broker-agnostic so the rest of the platform does not
// depend on Kite's wire format.
package marketdata

import "time"

// Tick represents a single market-data update for one instrument.
// Kite streams these over its WebSocket ticker; for paper/strategies we work
// only with this normalized form.
type Tick struct {
	InstrumentToken uint32    // Kite's numeric instrument token
	TradingSymbol   string    // e.g. NIFTY24AUG24500CE
	Exchange        string    // NFO, NSE, ...
	LastPrice       float64   // last traded price
	LastQuantity    int64     // last traded quantity
	LastTradeTime   time.Time // time of last trade
	AveragePrice    float64   // average trade price for the day
	Volume          int64     // day's volume
	BuyQuantity     int64     // total buy qty
	SellQuantity    int64     // total sell qty
	OHLC            OHLC      // day's OHLC
	Depth           *Depth    // market depth (5 levels); nil if not provided
	Timestamp       time.Time // tick arrival time (server or local)
}

// OHLC holds the open/high/low/close for a period (usually the trading day).
type OHLC struct {
	Open  float64
	High  float64
	Low   float64
	Close float64
}

// Depth holds the 5-level bid and ask market depth.
type Depth struct {
	Bids [5]QuoteLevel
	Asks [5]QuoteLevel
}

// QuoteLevel is one level of the market depth (price + quantity + orders).
type QuoteLevel struct {
	Price    float64
	Quantity int64
	Orders   int64
}

// Candle is an OHLC bar aggregated over a fixed interval, used for storage and
// later backtesting.
type Candle struct {
	InstrumentToken uint32
	TradingSymbol   string
	Interval        string // "1m", "5m", "15m", ...
	Open            float64
	High            float64
	Low             float64
	Close           float64
	Volume          int64
	OpenTime        time.Time
	CloseTime       time.Time
}

// Mode is the kind of market-data subscription requested from the ticker.
type Mode string

const (
	ModeFull    Mode = "full"    // ltp + quote + depth
	ModeQuote   Mode = "quote"   // ltp + quote (OHLC, volume)
	ModeLTP     Mode = "ltp"     // last traded price only
)
