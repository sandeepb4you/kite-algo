package kite

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Instrument mirrors the columns in Kite's instrument master CSV.
// Only the fields the platform uses are kept; Kite ships a few more.
type Instrument struct {
	InstrumentToken uint32
	TradingSymbol   string // NIFTY24AUG24500CE
	Name            string // underlying: NIFTY, BANKNIFTY, ...
	Expiry          time.Time
	Strike          float64
	LotSize         int
	InstrumentType  string // CE, PE, EQ, FUT
	Segment         string // NFO-OPT, NSE, NFO-FUT
	Exchange        string // NFO, NSE
	TickSize        float64
}

// IsOption reports whether the instrument is an option (CE/PE).
func (i Instrument) IsOption() bool {
	return i.InstrumentType == "CE" || i.InstrumentType == "PE"
}

// Instruments is the in-memory instrument master with fast lookups.
type Instruments struct {
	bySymbol map[string]*Instrument     // trading symbol -> instrument
	byToken  map[uint32]*Instrument     // instrument token -> instrument
}

// Lookup returns an instrument by trading symbol.
func (m *Instruments) Lookup(symbol string) (*Instrument, bool) {
	i, ok := m.bySymbol[strings.ToUpper(symbol)]
	return i, ok
}

// LookupToken returns an instrument by its Kite instrument token.
func (m *Instruments) LookupToken(token uint32) (*Instrument, bool) {
	i, ok := m.byToken[token]
	return i, ok
}

// Options returns all option instruments for an underlying's nearest expiry on
// or after `minExpiry`. Pass time.Time{} to get the soonest expiry. This is how
// strategies find the ATM/OTM strikes to trade.
func (m *Instruments) Options(underlying string, minExpiry time.Time) []Instrument {
	underlying = strings.ToUpper(underlying)
	// Find the soonest expiry >= minExpiry for this underlying.
	var targetExpiry time.Time
	for _, inst := range m.bySymbol {
		if !inst.IsOption() || inst.Name != underlying {
			continue
		}
		if !minExpiry.IsZero() && inst.Expiry.Before(minExpiry) {
			continue
		}
		if targetExpiry.IsZero() || inst.Expiry.Before(targetExpiry) {
			targetExpiry = inst.Expiry
		}
	}

	var out []Instrument
	for _, inst := range m.bySymbol {
		if inst.IsOption() && inst.Name == underlying && inst.Expiry.Equal(targetExpiry) {
			out = append(out, *inst)
		}
	}
	// Sort by strike for deterministic ATM lookup.
	sort.Slice(out, func(i, j int) bool { return out[i].Strike < out[j].Strike })
	return out
}

// FetchInstruments downloads the full instrument master CSV and parses it.
// The file is large (~1-2 MB, tens of thousands of rows) so call this once at
// startup and cache the result. Kite also publishes a per-exchange endpoint:
// /instruments/{exchange} if you only need NFO.
func (c *Client) FetchInstruments(ctx context.Context) (*Instruments, error) {
	data, err := c.rawGetBytes(ctx, "/instruments")
	if err != nil {
		return nil, err
	}
	return ParseInstruments(bytes.NewReader(data))
}

// FetchInstrumentsExchange downloads instruments for a single exchange (e.g.
// "NFO"), which is much smaller than the full master and faster for options.
func (c *Client) FetchInstrumentsExchange(ctx context.Context, exchange string) (*Instruments, error) {
	data, err := c.rawGetBytes(ctx, "/instruments/"+strings.ToUpper(exchange))
	if err != nil {
		return nil, err
	}
	return ParseInstruments(bytes.NewReader(data))
}

// ParseInstruments reads a Kite instrument CSV and builds the lookup index.
func ParseInstruments(r io.Reader) (*Instruments, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tolerate trailing column count changes

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read instruments header: %w", err)
	}
	idx := indexHeader(header)

	m := &Instruments{
		bySymbol: make(map[string]*Instrument),
		byToken:  make(map[uint32]*Instrument),
	}

	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read instruments row: %w", err)
		}
		inst, ok := parseInstrumentRow(rec, idx)
		if !ok {
			continue
		}
		// Deduplicate (same symbol can't appear twice; keep first).
		if _, exists := m.bySymbol[inst.TradingSymbol]; exists {
			continue
		}
		m.bySymbol[inst.TradingSymbol] = inst
		m.byToken[inst.InstrumentToken] = inst
	}
	return m, nil
}

// indexHeader builds a column-name -> position map so we're robust to Kite
// adding/reordering columns in the CSV header.
func indexHeader(header []string) map[string]int {
	idx := make(map[string]int, len(header))
	for i, h := range header {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	return idx
}

func col(rec []string, idx map[string]int, name string) string {
	i, ok := idx[name]
	if !ok || i >= len(rec) {
		return ""
	}
	return rec[i]
}

func parseInstrumentRow(rec []string, idx map[string]int) (*Instrument, bool) {
	tok, err := strconv.ParseUint(col(rec, idx, "instrument_token"), 10, 32)
	if err != nil {
		return nil, false
	}
	lot, _ := strconv.Atoi(col(rec, idx, "lot_size"))
	strike, _ := strconv.ParseFloat(col(rec, idx, "strike"), 64)
	tick, _ := strconv.ParseFloat(col(rec, idx, "tick_size"), 64)
	expiry, _ := time.Parse("2006-01-02", col(rec, idx, "expiry"))

	return &Instrument{
		InstrumentToken: uint32(tok),
		TradingSymbol:   col(rec, idx, "tradingsymbol"),
		Name:            col(rec, idx, "name"),
		Expiry:          expiry,
		Strike:          strike,
		LotSize:         lot,
		InstrumentType:  col(rec, idx, "instrument_type"),
		Segment:         col(rec, idx, "segment"),
		Exchange:        col(rec, idx, "exchange"),
		TickSize:        tick,
	}, true
}

// LTPResp is the response shape of GET /quote/ltp.
type LTPResp map[string]struct {
	InstrumentToken uint32  `json:"instrument_token"`
	LastPrice       float64 `json:"last_price"`
}

// GetLTP returns the last traded price for one or more "exchange:tradingsymbol"
// keys, e.g. []string{"NFO:NIFTY24AUG24500CE"}.
func (c *Client) GetLTP(ctx context.Context, keys []string) (LTPResp, error) {
	q := url.Values{}
	q.Set("i", strings.Join(keys, "&i="))
	var out LTPResp
	if err := c.get(ctx, "/quote/ltp", q, &out); err != nil {
		return nil, err
	}
	if c.logger != nil {
		c.logger.Debug("fetched ltp", "count", len(out))
	}
	return out, nil
}

// LogInstrumentsSummary logs a quick count of loaded instruments by segment.
func (c *Client) LogInstrumentsSummary(m *Instruments) {
	if c.logger == nil || m == nil {
		return
	}
	counts := map[string]int{}
	for _, inst := range m.bySymbol {
		counts[inst.Segment]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := []any{}
	for _, k := range keys {
		attrs = append(attrs, slog.Int(k, counts[k]))
	}
	c.logger.Info("instruments loaded", attrs...)
}
