package shortstraddle

import (
	"log/slog"

	"kite-algo/internal/strategy"
)

// init registers this strategy with the process-wide registry. Importing the
// package (see internal/strategy/catalog) is enough to make it appear in the UI.
func init() { strategy.Register(Descriptor()) }

// Descriptor declares the strategy's parameters as data, so the web UI can
// render and validate a configuration form without knowing anything about short
// straddles specifically.
//
// The defaults here are the single source of truth: Init no longer applies its
// own fallbacks, because two sets of defaults that can drift apart is a bug
// waiting to happen.
func Descriptor() strategy.Descriptor {
	return strategy.Descriptor{
		Type:    "short-straddle",
		Title:   "Delta-managed short straddle",
		Summary: "Sells the ATM call and put, then buys them back when net delta drifts past a threshold or the square-off time arrives.",
		Factory: func(instanceID string, logger *slog.Logger) (strategy.Strategy, error) {
			return New(instanceID, logger), nil
		},
		Params: []strategy.ParamSpec{
			{
				Key: "index_symbol", Label: "Spot index", Kind: strategy.KindString,
				Default: "NIFTY 50", Group: "Instrument",
				Description: "Index whose ticks drive entry.",
			},
			{
				Key: "underlying", Label: "Option underlying", Kind: strategy.KindString,
				Default: "NIFTY", Group: "Instrument",
				Description: "Underlying name as it appears in the instrument master.",
			},
			{
				Key: "strike_step", Label: "Strike step", Kind: strategy.KindFloat,
				Default: 50.0, Min: strategy.Ptr(1), Max: strategy.Ptr(1000), Group: "Instrument",
				Description: "Strike grid: 50 for NIFTY, 100 for BANKNIFTY.",
			},
			{
				Key: "lots", Label: "Lots per leg", Kind: strategy.KindInt,
				Default: 1, Min: strategy.Ptr(1), Max: strategy.Ptr(50), Group: "Execution",
				Description: "Start at 1. A short straddle has unlimited risk.",
			},
			{
				Key: "product", Label: "Product", Kind: strategy.KindEnum,
				Options: []string{"MIS", "NRML"}, Default: "MIS", Group: "Execution",
				Description: "MIS is intraday; NRML carries overnight.",
			},
			{
				Key: "exit_delta", Label: "Exit at |net delta|", Kind: strategy.KindFloat,
				Default: 0.25, Min: strategy.Ptr(0.01), Max: strategy.Ptr(2), Group: "Exit",
				Description: "Square off once the position's net delta drifts past this.",
			},
			{
				Key: "square_off_time", Label: "Square off by (IST)", Kind: strategy.KindTime,
				Default: "15:15", Group: "Exit",
				Description: "Flat by this time regardless of delta.",
			},
			{
				Key: "risk_free_rate", Label: "Risk-free rate", Kind: strategy.KindFloat,
				Default: 0.06, Min: strategy.Ptr(0), Max: strategy.Ptr(0.25), Group: "Exit",
				Description: "Annualized, used by the Black-Scholes greeks.",
			},
		},
	}
}
