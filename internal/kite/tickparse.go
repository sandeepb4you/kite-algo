package kite

import (
	"time"

	"kite-algo/internal/marketdata"
)

// parseBinaryTicks decodes a Kite WebSocket binary frame into ticks.
//
// Frame layout (big-endian, verified against pykiteconnect ticker.py):
//
//	[2B uint16] number_of_packets
//	repeat per packet:
//	  [2B uint16] packet_length
//	  [packet_length B] payload
//
// Payload layout depends on length:
//	  8  B : LTP                       (token, last_price)
//	  28 B : indices quote             (token, last_price, OHLC[h,l,o,c])
//	  32 B : indices full              (28B + exchange timestamp)
//	  44 B : standard quote            (token, ltp, ltq, avg, vol, buy_q, sell_q, OHLC[o,h,l,c])
//	  184B : standard full             (44B + last_trade_time, oi, oi_high, oi_low, ts, depth)
func parseBinaryTicks(frame []byte) []marketdata.Tick {
	packets, ok := splitPackets(frame)
	if !ok {
		return nil
	}
	ticks := make([]marketdata.Tick, 0, len(packets))
	now := time.Now()
	for _, p := range packets {
		if len(p) < 8 {
			continue
		}
		token := beU32(p, 0)
		div := priceDivisor(token)

		switch len(p) {
		case 8: // LTP only
			ticks = append(ticks, marketdata.Tick{
				InstrumentToken: token,
				LastPrice:       float64(beU32(p, 4)) / div,
				Timestamp:       now,
			})
		case 28, 32: // indices quote / full
			tk := marketdata.Tick{
				InstrumentToken: token,
				LastPrice:       float64(beU32(p, 4)) / div,
				OHLC: marketdata.OHLC{
					High:  float64(beU32(p, 8)) / div,
					Low:   float64(beU32(p, 12)) / div,
					Open:  float64(beU32(p, 16)) / div,
					Close: float64(beU32(p, 20)) / div,
				},
				Timestamp: now,
			}
			if len(p) == 32 {
				if ts := unixTime(beU32(p, 28)); !ts.IsZero() {
					tk.Timestamp = ts
				}
			}
			ticks = append(ticks, tk)
		case 44, 184: // standard quote / full
			tk := marketdata.Tick{
				InstrumentToken: token,
				LastPrice:       float64(beU32(p, 4)) / div,
				LastQuantity:    int64(beU32(p, 8)),
				AveragePrice:    float64(beU32(p, 12)) / div,
				Volume:          int64(beU32(p, 16)),
				BuyQuantity:     int64(beU32(p, 20)),
				SellQuantity:    int64(beU32(p, 24)),
				OHLC: marketdata.OHLC{
					Open:  float64(beU32(p, 28)) / div,
					High:  float64(beU32(p, 32)) / div,
					Low:   float64(beU32(p, 36)) / div,
					Close: float64(beU32(p, 40)) / div,
				},
				Timestamp: now,
			}
			if len(p) == 184 {
				if ltt := unixTime(beU32(p, 44)); !ltt.IsZero() {
					tk.LastTradeTime = ltt
				}
				if ts := unixTime(beU32(p, 60)); !ts.IsZero() {
					tk.Timestamp = ts
				}
				tk.Depth = parseDepth(p, 64, 184, div)
			}
			ticks = append(ticks, tk)
		}
	}
	return ticks
}

// parseDepth reads the 10 market-depth levels (5 buy, 5 sell) from a full-mode
// packet. Each level is on a 12-byte stride: quantity[4], price[4], orders[2],
// (2 bytes skipped). Buy levels are 0..4, sell levels are 5..9.
func parseDepth(p []byte, start, end int, div float64) *marketdata.Depth {
	d := &marketdata.Depth{}
	idx := 0
	for off := start; off+10 <= end && idx < 10; off += 12 {
		level := marketdata.QuoteLevel{
			Quantity: int64(beU32(p, off)),
			Price:    float64(beU32(p, off+4)) / div,
			Orders:   int64(beU16(p, off+8)),
		}
		if idx < 5 {
			d.Bids[idx] = level
		} else {
			d.Asks[idx-5] = level
		}
		idx++
	}
	return d
}

// splitPackets reads the 2-byte packet count and splits on the 2-byte per-packet
// length prefix. Returns ok=false if the frame is too short to be valid.
func splitPackets(frame []byte) ([][]byte, bool) {
	if len(frame) < 2 {
		return nil, false
	}
	n := int(beU16(frame, 0))
	packets := make([][]byte, 0, n)
	j := 2
	for i := 0; i < n; i++ {
		if j+2 > len(frame) {
			break
		}
		length := int(beU16(frame, j))
		j += 2
		if j+length > len(frame) {
			break
		}
		packets = append(packets, frame[j:j+length])
		j += length
	}
	return packets, true
}

// --- big-endian readers ---

func beU16(b []byte, off int) uint16 {
	if off+2 > len(b) {
		return 0
	}
	return uint16(b[off])<<8 | uint16(b[off+1])
}

func beU32(b []byte, off int) uint32 {
	if off+4 > len(b) {
		return 0
	}
	return uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3])
}

// unixTime converts a Kite epoch-seconds field to a local time, returning the
// zero time for invalid values (0 is used as a sentinel by the protocol).
func unixTime(u uint32) time.Time {
	if u == 0 {
		return time.Time{}
	}
	return time.Unix(int64(u), 0)
}
