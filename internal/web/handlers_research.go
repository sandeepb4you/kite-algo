package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kite-algo/internal/history"
	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/storage"
)

// researchData drives the historical-data page.
type researchData struct {
	Symbol    string
	Interval  string
	From      string
	To        string
	Intervals []kite.Interval
	Candles   []marketdata.Candle
	Error     string
	Source    string
	Snapshots snapshotInfo
}

// Recent returns the last 50 bars, newest last — enough to eyeball the data
// without rendering tens of thousands of table rows.
func (d researchData) Recent() []marketdata.Candle {
	const n = 50
	if len(d.Candles) <= n {
		return d.Candles
	}
	return d.Candles[len(d.Candles)-n:]
}

// snapshotInfo reports the state of instrument capture, which is the one piece
// of data that becomes permanently unavailable if not collected.
type snapshotInfo struct {
	HaveToday bool
	AsOf      string
}

// handleResearch renders the historical-data explorer.
func (s *Server) handleResearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	today := time.Now().In(history.IST)

	data := researchData{
		Symbol:    strings.ToUpper(strings.TrimSpace(q.Get("symbol"))),
		Interval:  q.Get("interval"),
		From:      q.Get("from"),
		To:        q.Get("to"),
		Intervals: kite.Intervals,
		Snapshots: s.snapshotInfo(r),
	}
	if data.Interval == "" {
		data.Interval = string(kite.Interval15Minute)
	}
	if data.From == "" {
		data.From = today.AddDate(0, 0, -7).Format("2006-01-02")
	}
	if data.To == "" {
		data.To = today.Format("2006-01-02")
	}

	if data.Symbol != "" {
		candles, source, err := s.fetchHistory(r, data)
		if err != nil {
			data.Error = err.Error()
		} else {
			data.Candles = candles
			data.Source = source
		}
	}

	s.renderPage(w, r, "research.html", "Research", data)
}

// fetchHistory resolves the form into a provider call.
func (s *Server) fetchHistory(r *http.Request, d researchData) ([]marketdata.Candle, string, error) {
	interval, ok := kite.ParseInterval(d.Interval)
	if !ok {
		return nil, "", fmt.Errorf("%q is not a Kite interval", d.Interval)
	}
	from, err := time.ParseInLocation("2006-01-02", d.From, history.IST)
	if err != nil {
		return nil, "", fmt.Errorf("'from' must be a date like 2024-08-01")
	}
	to, err := time.ParseInLocation("2006-01-02", d.To, history.IST)
	if err != nil {
		return nil, "", fmt.Errorf("'to' must be a date like 2024-08-08")
	}
	// Make 'to' inclusive of its own trading day, which is what a human means by
	// a date range.
	to = to.AddDate(0, 0, 1)
	if !to.After(from) {
		return nil, "", fmt.Errorf("'to' must be on or after 'from'")
	}

	provider, name := s.historyProvider()
	if provider == nil {
		return nil, "", fmt.Errorf("historical data needs a Zerodha session — log in first")
	}

	candles, err := provider.Candles(r.Context(), history.Request{
		Symbol:   d.Symbol,
		Interval: interval,
		From:     from,
		To:       to,
	})
	if err != nil {
		return nil, name, err
	}
	return candles, name, nil
}

// historyProvider builds the cache→Kite→ticks chain for the current session.
//
// It is constructed per request rather than held on the Server because the Kite
// client and instrument master only exist after login, and are replaced when a
// session is renewed.
func (s *Server) historyProvider() (history.Provider, string) {
	store, ok := s.app.Store.(storage.HistoryStore)
	if !ok {
		return nil, ""
	}

	client := s.app.Kite.Client()
	instruments := s.app.Kite.Instruments()

	// Recorded ticks are always available as a fallback; the Kite provider only
	// when there is a live session.
	ticks := history.NewTickProvider(store, s.log)
	if client == nil || !s.app.Kite.Snapshot().Connected() {
		return history.NewCacheProvider(store, ticks, s.log), "ticks"
	}

	upstream := history.NewChain(
		history.NewKiteProvider(client, instruments, s.log),
		ticks,
	)
	return history.NewCacheProvider(store, upstream, s.log), "kite"
}

// snapshotInfo reports whether today's instrument master has been captured.
func (s *Server) snapshotInfo(r *http.Request) snapshotInfo {
	store, ok := s.app.Store.(storage.HistoryStore)
	if !ok {
		return snapshotInfo{}
	}
	today := time.Now().In(history.IST)
	have, err := store.HasInstrumentSnapshot(r.Context(), today)
	if err != nil {
		s.log.Debug("check instrument snapshot failed", "err", err)
	}
	return snapshotInfo{HaveToday: have, AsOf: today.Format("2006-01-02")}
}

// handleCandlesJSON serves candles for the chart.
func (s *Server) handleCandlesJSON(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := researchData{
		Symbol:   strings.ToUpper(strings.TrimSpace(q.Get("symbol"))),
		Interval: q.Get("interval"),
		From:     q.Get("from"),
		To:       q.Get("to"),
	}
	if d.Symbol == "" {
		http.Error(w, "symbol is required", http.StatusBadRequest)
		return
	}

	candles, _, err := s.fetchHistory(r, d)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeCandlesJSON(w, candles)
}

// candleWire is the shape chart.js consumes. Short field names on purpose: a
// year of minute bars is a lot of JSON, and spelling them out would triple it
// for no benefit to a chart.
type candleWire struct {
	T int64   `json:"t"`
	O float64 `json:"o"`
	H float64 `json:"h"`
	L float64 `json:"l"`
	C float64 `json:"c"`
	V int64   `json:"v"`
}

// writeCandlesJSON renders a candle series for the chart.
func writeCandlesJSON(w http.ResponseWriter, candles []marketdata.Candle) {
	out := make([]candleWire, 0, len(candles))
	for _, c := range candles {
		out = append(out, candleWire{
			T: c.OpenTime.UnixMilli(),
			O: c.Open, H: c.High, L: c.Low, C: c.Close, V: c.Volume,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
