// Package history supplies historical candles to research and backtesting.
//
// It sits between the Kite historical API and the rest of the platform, and
// exists mainly to avoid re-fetching. Kite meters historical requests, caps the
// date range per request, and charges for the subscription; a backtest that
// re-downloads a year of minute bars on every run is unusable.
//
// The central abstraction is Provider. Implementations chain: a cache in front
// of the Kite API, with recorded ticks as a fallback for anyone without the
// historical subscription.
package history

import (
	"context"
	"fmt"
	"time"

	"kite-algo/internal/kite"
	"kite-algo/internal/marketdata"
)

// Request asks for candles for one instrument over one window.
type Request struct {
	Symbol   string // trading symbol; the key everything is stored under
	Token    uint32 // instrument token; resolved from Symbol when zero
	Interval kite.Interval
	From     time.Time
	To       time.Time
}

// Validate checks a request is answerable.
func (r Request) Validate() error {
	if r.Symbol == "" {
		return fmt.Errorf("history: request needs a trading symbol")
	}
	if r.Interval == "" {
		return fmt.Errorf("history: request needs an interval")
	}
	if !r.To.After(r.From) {
		return fmt.Errorf("history: 'to' must be after 'from'")
	}
	return nil
}

// Provider returns candles for a request.
type Provider interface {
	// Candles returns bars in [From, To), ordered by open time.
	Candles(ctx context.Context, req Request) ([]marketdata.Candle, error)
	// Name identifies the provider in logs and in a backtest's provenance.
	Name() string
}

// Chain tries providers in order and returns the first non-empty result.
//
// The intended arrangement is cache → Kite → recorded ticks: an operator
// without the historical subscription still gets candles built from data the
// platform recorded itself, and the run is labelled with where the data came
// from so nobody mistakes tick-derived bars for exchange data.
type Chain struct {
	providers []Provider
}

// NewChain builds a fallback chain.
func NewChain(providers ...Provider) *Chain { return &Chain{providers: providers} }

// Name reports the chain's members.
func (c *Chain) Name() string {
	names := make([]string, 0, len(c.providers))
	for _, p := range c.providers {
		names = append(names, p.Name())
	}
	return "chain(" + join(names, "→") + ")"
}

// Candles asks each provider in turn.
func (c *Chain) Candles(ctx context.Context, req Request) ([]marketdata.Candle, error) {
	var lastErr error
	for _, p := range c.providers {
		candles, err := p.Candles(ctx, req)
		if err != nil {
			lastErr = err
			continue
		}
		if len(candles) > 0 {
			return candles, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
