package kite

import (
	"encoding/binary"
	"testing"
)

// beFrame builds a binary frame: [u16 num_packets][per packet: u16 len + bytes].
func beFrame(packets ...[]byte) []byte {
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out, uint16(len(packets)))
	for _, p := range packets {
		hdr := make([]byte, 2)
		binary.BigEndian.PutUint16(hdr, uint16(len(p)))
		out = append(out, hdr...)
		out = append(out, p...)
	}
	return out
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }

// TestParseLTP verifies an 8-byte LTP packet decodes to last_price = raw/100.
func TestParseLTP(t *testing.T) {
	// Token 0x402 → segment 2 (NFO) → divisor 100.
	tok := uint32(0x402)
	pkt := append(u32(tok), u32(15000)...) // raw price 15000 → 150.00
	ticks := parseBinaryTicks(beFrame(pkt))
	if len(ticks) != 1 {
		t.Fatalf("got %d ticks, want 1", len(ticks))
	}
	if ticks[0].InstrumentToken != tok {
		t.Errorf("token = %d, want %d", ticks[0].InstrumentToken, tok)
	}
	if ticks[0].LastPrice != 150.0 {
		t.Errorf("last_price = %v, want 150.0", ticks[0].LastPrice)
	}
}

// TestParseQuote verifies a 44-byte standard quote packet.
func TestParseQuote(t *testing.T) {
	tok := uint32(0x402) // NFO, divisor 100
	pkt := []byte{}
	pkt = append(pkt, u32(tok)...)
	pkt = append(pkt, u32(15000)...) // ltp → 150.00
	pkt = append(pkt, u32(10)...)    // last_traded_qty
	pkt = append(pkt, u32(14000)...) // avg → 140.00
	pkt = append(pkt, u32(1000)...)  // volume
	pkt = append(pkt, u32(500)...)   // total_buy_qty
	pkt = append(pkt, u32(600)...)   // total_sell_qty
	pkt = append(pkt, u32(14500)...) // open → 145.00
	pkt = append(pkt, u32(15500)...) // high → 155.00
	pkt = append(pkt, u32(14000)...) // low → 140.00
	pkt = append(pkt, u32(15000)...) // close → 150.00

	if len(pkt) != 44 {
		t.Fatalf("test packet length = %d, want 44", len(pkt))
	}
	ticks := parseBinaryTicks(beFrame(pkt))
	if len(ticks) != 1 {
		t.Fatalf("got %d ticks, want 1", len(ticks))
	}
	tk := ticks[0]
	checks := []struct {
		name      string
		got, want float64
	}{
		{"last_price", tk.LastPrice, 150.0},
		{"volume", float64(tk.Volume), 1000},
		{"buy_qty", float64(tk.BuyQuantity), 500},
		{"sell_qty", float64(tk.SellQuantity), 600},
		{"open", tk.OHLC.Open, 145.0},
		{"high", tk.OHLC.High, 155.0},
		{"low", tk.OHLC.Low, 140.0},
		{"close", tk.OHLC.Close, 150.0},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestParseIndicesQuote verifies the 28-byte indices packet (different OHLC
// order: high, low, open, close).
func TestParseIndicesQuote(t *testing.T) {
	// Indices token: low byte 9 → indices segment. Use 0x109.
	tok := uint32(0x109)
	pkt := []byte{}
	pkt = append(pkt, u32(tok)...)
	pkt = append(pkt, u32(22500)...) // ltp → 225.00
	pkt = append(pkt, u32(22600)...) // high → 226.00
	pkt = append(pkt, u32(22400)...) // low → 224.00
	pkt = append(pkt, u32(22000)...) // open → 220.00
	pkt = append(pkt, u32(22500)...) // close → 225.00
	// Bytes [24:28] of an indices quote packet are sent by Kite but not read by
	// pykiteconnect (and not by us); pad with zeros to form a faithful 28-byte packet.
	pkt = append(pkt, u32(0)...)
	if len(pkt) != 28 {
		t.Fatalf("indices packet length = %d, want 28", len(pkt))
	}
	ticks := parseBinaryTicks(beFrame(pkt))
	if len(ticks) != 1 {
		t.Fatalf("got %d ticks, want 1", len(ticks))
	}
	tk := ticks[0]
	if tk.LastPrice != 225.0 {
		t.Errorf("ltp = %v, want 225", tk.LastPrice)
	}
	if tk.OHLC.High != 226.0 || tk.OHLC.Low != 224.0 || tk.OHLC.Open != 220.0 || tk.OHLC.Close != 225.0 {
		t.Errorf("indices OHLC wrong: %+v", tk.OHLC)
	}
}

// TestParseFullDepth verifies the 184-byte packet parses the first bid level.
func TestParseFullDepth(t *testing.T) {
	tok := uint32(0x402)
	pkt := make([]byte, 184)
	// Header (44 bytes): token, ltp, ltq, avg, vol, buyq, sellq, o, h, l, c.
	binary.BigEndian.PutUint32(pkt[0:], tok)
	binary.BigEndian.PutUint32(pkt[4:], 15000) // ltp
	// Offsets 8..44 left zero.
	// Depth starts at offset 64. First level (bid 0): qty[64:68], price[68:72], orders[72:74].
	binary.BigEndian.PutUint32(pkt[64:], 300)   // qty
	binary.BigEndian.PutUint32(pkt[68:], 15000) // price → 150.00
	binary.BigEndian.PutUint16(pkt[72:], 5)     // orders

	ticks := parseBinaryTicks(beFrame(pkt))
	if len(ticks) != 1 {
		t.Fatalf("got %d ticks, want 1", len(ticks))
	}
	tk := ticks[0]
	if tk.Depth == nil {
		t.Fatal("expected depth")
	}
	if tk.Depth.Bids[0].Quantity != 300 {
		t.Errorf("bid0 qty = %d, want 300", tk.Depth.Bids[0].Quantity)
	}
	if tk.Depth.Bids[0].Price != 150.0 {
		t.Errorf("bid0 price = %v, want 150", tk.Depth.Bids[0].Price)
	}
	if tk.Depth.Bids[0].Orders != 5 {
		t.Errorf("bid0 orders = %d, want 5", tk.Depth.Bids[0].Orders)
	}
}

// TestParseMultiplePackets verifies the count/length-prefix splitting for >1 packet.
func TestParseMultiplePackets(t *testing.T) {
	tok := uint32(0x402)
	p1 := append(u32(tok), u32(100)...)
	p2 := append(u32(tok), u32(200)...)
	ticks := parseBinaryTicks(beFrame(p1, p2))
	if len(ticks) != 2 {
		t.Fatalf("got %d ticks, want 2", len(ticks))
	}
	if ticks[0].LastPrice != 1.0 || ticks[1].LastPrice != 2.0 {
		t.Errorf("prices = %v, %v; want 1.0, 2.0", ticks[0].LastPrice, ticks[1].LastPrice)
	}
}
