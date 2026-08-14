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
	bySymbol map[string]*Instrument // trading symbol -> instrument
	byToken  map[uint32]*Instrument // instrument token -> instrument
}

// Lookup returns an instrument by trading symbol.
func (m *Instruments) Lookup(symbol string) (*Instrument, bool) {
	i, ok := m.bySymbol[strings.ToUpper(symbol)]
	return i, ok
}

// Search returns instruments whose trading symbol contains query, case
// insensitively, capped at limit results.
//
// Both the cap and the ordering matter for the order ticket's typeahead: an NFO
// master holds tens of thousands of contracts, and raw map iteration order would
// make the same query return different suggestions on each keystroke.
func (m *Instruments) Search(query string, limit int) []Instrument {
	needle := strings.ToUpper(strings.TrimSpace(query))
	if needle == "" || limit <= 0 {
		return nil
	}

	var matches []Instrument
	for symbol, inst := range m.bySymbol {
		if strings.Contains(symbol, needle) {
			matches = append(matches, *inst)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		// Prefer symbols that start with the query, so typing "NIFTY24AUG"
		// surfaces those contracts ahead of ones that merely contain it.
		pi := strings.HasPrefix(strings.ToUpper(matches[i].TradingSymbol), needle)
		pj := strings.HasPrefix(strings.ToUpper(matches[j].TradingSymbol), needle)
		if pi != pj {
			return pi
		}
		return matches[i].TradingSymbol < matches[j].TradingSymbol
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

// All returns every instrument in the master, sorted by trading symbol.
//
// Used to snapshot the master for backtesting. The sort makes the output
// deterministic, which matters because a snapshot is a persisted artefact.
func (m *Instruments) All() []Instrument {
	out := make([]Instrument, 0, len(m.bySymbol))
	for _, inst := range m.bySymbol {
		out = append(out, *inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TradingSymbol < out[j].TradingSymbol })
	return out
}

// Len reports how many instruments are loaded.
func (m *Instruments) Len() int { return len(m.bySymbol) }

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

// Expiries returns the distinct option expiry dates for an underlying, soonest
// first, excluding any that have already passed.
//
// Kite lists weeklies and monthlies together with no flag distinguishing them,
// so this returns every expiry and lets the caller choose. For NIFTY the first
// entry is normally the current week.
func (m *Instruments) Expiries(underlying string, after time.Time) []time.Time {
	underlying = strings.ToUpper(underlying)
	seen := make(map[string]time.Time)

	for _, inst := range m.bySymbol {
		if !inst.IsOption() || inst.Name != underlying || inst.Expiry.IsZero() {
			continue
		}
		// Compare on the date only: an expiry is valid for the whole of its last
		// trading day, and dropping it at midnight would hide the contract
		// everyone is actually trading that morning.
		if !after.IsZero() && inst.Expiry.Before(after.Truncate(24*time.Hour)) {
			continue
		}
		seen[inst.Expiry.Format("2006-01-02")] = inst.Expiry
	}

	out := make([]time.Time, 0, len(seen))
	for _, t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// Chain returns every option for an underlying at one specific expiry, sorted
// by strike.
//
// Unlike Options, which picks the nearest expiry itself, this takes the expiry
// the caller chose — which is what an operator selecting "next week" needs.
// Matching is on the calendar date, so a caller need not reproduce the exact
// timestamp stored in the instrument master.
func (m *Instruments) Chain(underlying string, expiry time.Time) []Instrument {
	underlying = strings.ToUpper(underlying)
	want := expiry.Format("2006-01-02")

	var out []Instrument
	for _, inst := range m.bySymbol {
		if inst.IsOption() && inst.Name == underlying &&
			inst.Expiry.Format("2006-01-02") == want {
			out = append(out, *inst)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Strike != out[j].Strike {
			return out[i].Strike < out[j].Strike
		}
		return out[i].InstrumentType < out[j].InstrumentType
	})
	return out
}

// Underlyings returns the distinct option underlyings in the master, sorted.
func (m *Instruments) Underlyings() []string {
	seen := make(map[string]struct{})
	for _, inst := range m.bySymbol {
		if inst.IsOption() && inst.Name != "" {
			seen[inst.Name] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
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
	// Kite expects the "i" parameter repeated once per instrument. Building the
	// repetition by hand (strings.Join with "&i=") does not work: Encode()
	// percent-escapes the separators, collapsing every key into one malformed
	// value. Add() emits a genuine repeated parameter.
	q := url.Values{}
	for _, k := range keys {
		q.Add("i", k)
	}
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
