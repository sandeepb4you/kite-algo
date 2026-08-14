package kite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// IST is the exchange timezone. Kite interprets historical from/to parameters
// in exchange-local time, so every conversion here goes through it.
var IST = time.FixedZone("IST", 5*3600+30*60)

// Interval is a Kite historical-candle interval.
type Interval string

const (
	IntervalMinute   Interval = "minute"
	Interval3Minute  Interval = "3minute"
	Interval5Minute  Interval = "5minute"
	Interval10Minute Interval = "10minute"
	Interval15Minute Interval = "15minute"
	Interval30Minute Interval = "30minute"
	Interval60Minute Interval = "60minute"
	IntervalDay      Interval = "day"
)

// Intervals lists every supported interval, shortest first.
var Intervals = []Interval{
	IntervalMinute, Interval3Minute, Interval5Minute, Interval10Minute,
	Interval15Minute, Interval30Minute, Interval60Minute, IntervalDay,
}

// ParseInterval validates an interval string.
func ParseInterval(s string) (Interval, bool) {
	for _, i := range Intervals {
		if string(i) == s {
			return i, true
		}
	}
	return "", false
}

// Duration returns the wall-clock length of one candle at this interval.
func (i Interval) Duration() time.Duration {
	switch i {
	case IntervalMinute:
		return time.Minute
	case Interval3Minute:
		return 3 * time.Minute
	case Interval5Minute:
		return 5 * time.Minute
	case Interval10Minute:
		return 10 * time.Minute
	case Interval15Minute:
		return 15 * time.Minute
	case Interval30Minute:
		return 30 * time.Minute
	case Interval60Minute:
		return time.Hour
	case IntervalDay:
		return 24 * time.Hour
	}
	return 0
}

// MaxDays is Kite's per-request date-range cap for this interval.
//
// Kite rejects a request whose range exceeds the cap outright, so long ranges
// must be split — see GetHistoricalRange. Finer intervals have tighter caps
// because they return far more rows.
//
// VERIFY these against https://kite.trade/docs/connect/v3/historical/ before
// relying on them; Zerodha has adjusted them in the past, and a cap that is too
// large produces an opaque API error rather than a clear one.
func (i Interval) MaxDays() int {
	switch i {
	case IntervalMinute:
		return 60
	case Interval3Minute, Interval5Minute, Interval10Minute:
		return 100
	case Interval15Minute, Interval30Minute:
		return 200
	case Interval60Minute:
		return 400
	case IntervalDay:
		return 2000
	}
	return 60 // unknown interval: assume the tightest cap
}

// HistoricalCandle is one OHLC bar from Kite's historical endpoint.
type HistoricalCandle struct {
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume int64
	OI     int64
}

// candleTimeLayout is Kite's historical timestamp format, e.g.
// "2024-08-01T09:15:00+0530".
const candleTimeLayout = "2006-01-02T15:04:05-0700"

// UnmarshalJSON decodes Kite's positional candle array.
//
// The API returns candles as heterogeneous arrays rather than objects:
//
//	["2024-08-01T09:15:00+0530", 24500.1, 24512.3, 24495, 24505.2, 187500, 91234]
//
// with the trailing open-interest element present only when oi=1 was requested.
func (c *HistoricalCandle) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("candle is not an array: %w", err)
	}
	if len(raw) < 6 {
		return fmt.Errorf("candle has %d fields, want at least 6", len(raw))
	}

	var ts string
	if err := json.Unmarshal(raw[0], &ts); err != nil {
		return fmt.Errorf("candle timestamp: %w", err)
	}
	t, err := time.Parse(candleTimeLayout, ts)
	if err != nil {
		return fmt.Errorf("parse candle timestamp %q: %w", ts, err)
	}
	c.Time = t

	for i, dst := range []*float64{&c.Open, &c.High, &c.Low, &c.Close} {
		if err := json.Unmarshal(raw[i+1], dst); err != nil {
			return fmt.Errorf("candle field %d: %w", i+1, err)
		}
	}
	if err := json.Unmarshal(raw[5], &c.Volume); err != nil {
		return fmt.Errorf("candle volume: %w", err)
	}
	if len(raw) >= 7 {
		// Open interest is optional; ignore a malformed value rather than
		// discarding an otherwise good candle.
		_ = json.Unmarshal(raw[6], &c.OI)
	}
	return nil
}

// HistoricalRequest describes one historical-data query.
type HistoricalRequest struct {
	InstrumentToken uint32
	Interval        Interval
	From            time.Time
	To              time.Time
	Continuous      bool // stitch expired futures contracts into one series
	OI              bool // include open interest
}

// ErrRangeTooLarge means the request exceeds Kite's per-request cap for its
// interval. Use GetHistoricalRange, which splits automatically.
var ErrRangeTooLarge = errors.New("kite: historical range exceeds the per-request limit for this interval")

// GetHistorical fetches one page of candles.
//
// It returns ErrRangeTooLarge rather than letting Kite reject the request, so
// the caller gets an actionable error instead of an opaque API one.
func (c *Client) GetHistorical(ctx context.Context, req HistoricalRequest) ([]HistoricalCandle, error) {
	if req.InstrumentToken == 0 {
		return nil, errors.New("kite: historical request needs an instrument token")
	}
	if req.Interval == "" {
		return nil, errors.New("kite: historical request needs an interval")
	}
	if !req.To.After(req.From) {
		return nil, errors.New("kite: historical 'to' must be after 'from'")
	}
	if days := req.To.Sub(req.From).Hours() / 24; days > float64(req.Interval.MaxDays()) {
		return nil, fmt.Errorf("%w: %.0f days at %s (max %d)",
			ErrRangeTooLarge, days, req.Interval, req.Interval.MaxDays())
	}

	q := url.Values{}
	q.Set("from", req.From.In(IST).Format("2006-01-02 15:04:05"))
	q.Set("to", req.To.In(IST).Format("2006-01-02 15:04:05"))
	if req.Continuous {
		q.Set("continuous", "1")
	}
	if req.OI {
		q.Set("oi", "1")
	}

	path := "/instruments/historical/" +
		strconv.FormatUint(uint64(req.InstrumentToken), 10) + "/" + string(req.Interval)

	var out struct {
		Candles []HistoricalCandle `json:"candles"`
	}
	// Historical requests go through their own rate limiter so a long backfill
	// cannot starve order placement, which shares the client.
	if err := c.getLimited(ctx, c.histLimiter, path, q, &out); err != nil {
		return nil, err
	}
	return out.Candles, nil
}

// GetHistoricalRange fetches an arbitrary date range, splitting it into as many
// requests as Kite's per-interval cap requires and concatenating the results.
//
// Chunks are fetched sequentially and rate-limited. A multi-year minute-data
// pull is therefore slow by design — fetch it once and cache it.
func (c *Client) GetHistoricalRange(ctx context.Context, req HistoricalRequest) ([]HistoricalCandle, error) {
	if !req.To.After(req.From) {
		return nil, errors.New("kite: historical 'to' must be after 'from'")
	}

	span := time.Duration(req.Interval.MaxDays()) * 24 * time.Hour
	var all []HistoricalCandle

	for start := req.From; start.Before(req.To); {
		end := start.Add(span)
		if end.After(req.To) {
			end = req.To
		}

		page := req
		page.From, page.To = start, end

		candles, err := c.GetHistorical(ctx, page)
		if err != nil {
			// Return what we have alongside the error: a partial backfill is
			// still worth caching, and the caller can resume from the gap.
			return all, fmt.Errorf("historical %s..%s: %w",
				start.Format("2006-01-02"), end.Format("2006-01-02"), err)
		}
		all = append(all, candles...)

		if err := ctx.Err(); err != nil {
			return all, err
		}
		start = end
	}

	return dedupeCandles(all), nil
}

// dedupeCandles sorts by time and drops duplicates at chunk boundaries, where
// an inclusive range end can return the same candle twice.
func dedupeCandles(in []HistoricalCandle) []HistoricalCandle {
	if len(in) < 2 {
		return in
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Time.Before(in[j].Time) })

	out := in[:1]
	for _, c := range in[1:] {
		if !c.Time.Equal(out[len(out)-1].Time) {
			out = append(out, c)
		}
	}
	return out
}

// IsPermissionError reports whether err is Kite refusing for lack of the paid
// Historical Data subscription, as opposed to a transient failure.
//
// Worth distinguishing: without the subscription, retrying never helps and the
// platform should fall back to candles built from recorded ticks.
func IsPermissionError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorType == "PermissionException" || apiErr.StatusCode == 403
}
