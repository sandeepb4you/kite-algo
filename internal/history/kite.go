package history

import (
	"context"
	"fmt"
	"log/slog"

	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
)

// KiteProvider fetches candles from Zerodha's historical API.
type KiteProvider struct {
	client      *kite.Client
	instruments *kite.Instruments
	logger      *slog.Logger
}

// NewKiteProvider builds a provider over an authenticated client.
func NewKiteProvider(client *kite.Client, instruments *kite.Instruments, logger *slog.Logger) *KiteProvider {
	return &KiteProvider{client: client, instruments: instruments, logger: logger}
}

// Name identifies this provider.
func (p *KiteProvider) Name() string { return "kite" }

// Candles fetches from Kite, chunking the range as the interval requires.
func (p *KiteProvider) Candles(ctx context.Context, req Request) ([]marketdata.Candle, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if p.client == nil {
		return nil, fmt.Errorf("history: no Zerodha session; log in first")
	}

	token := req.Token
	if token == 0 {
		var err error
		token, err = p.resolve(req.Symbol)
		if err != nil {
			return nil, err
		}
	}

	raw, err := p.client.GetHistoricalRange(ctx, kite.HistoricalRequest{
		InstrumentToken: token,
		Interval:        req.Interval,
		From:            req.From,
		To:              req.To,
		OI:              true,
	})
	if err != nil {
		if kite.IsPermissionError(err) {
			// Retrying never helps, and the caller should fall through to the
			// tick-derived provider rather than treating this as an outage.
			return nil, fmt.Errorf("historical data subscription required: %w", err)
		}
		return raw2candles(raw, req, token), err
	}
	return raw2candles(raw, req, token), nil
}

// resolve looks a trading symbol up in the live instrument master.
//
// Note this only works for contracts that still exist. Expired options are
// unresolvable here by construction — that is what the instrument snapshots
// exist for.
func (p *KiteProvider) resolve(symbol string) (uint32, error) {
	// Index spot symbols ("NIFTY 50") are in no exchange CSV, so the instrument
	// master will never hold them; their tokens are hard-coded (see
	// kite/indices.go) and the ticker already subscribes through that table.
	// Without this, every index-driven backtest loaded zero candles and reported
	// a clean run of no trades — the strategy simply never saw a spot price.
	if token, ok := kite.IndexTokenFor(symbol); ok {
		return token, nil
	}
	if p.instruments == nil {
		return 0, fmt.Errorf("history: no instrument master loaded; log in first")
	}
	inst, ok := p.instruments.Lookup(symbol)
	if !ok {
		return 0, fmt.Errorf("history: %q is not in the current instrument master "+
			"(expired contracts need an instrument snapshot from the day they traded)", symbol)
	}
	return inst.InstrumentToken, nil
}

// raw2candles converts Kite's candles into the platform's storage type.
func raw2candles(raw []kite.HistoricalCandle, req Request, token uint32) []marketdata.Candle {
	if len(raw) == 0 {
		return nil
	}
	dur := req.Interval.Duration()
	out := make([]marketdata.Candle, 0, len(raw))
	for _, c := range raw {
		out = append(out, marketdata.Candle{
			InstrumentToken: token,
			TradingSymbol:   req.Symbol,
			Interval:        string(req.Interval),
			Open:            c.Open,
			High:            c.High,
			Low:             c.Low,
			Close:           c.Close,
			Volume:          c.Volume,
			OpenInterest:    c.OI,
			OpenTime:        c.Time,
			CloseTime:       c.Time.Add(dur),
		})
	}
	return out
}
