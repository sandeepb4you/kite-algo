package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"kite-algo/internal/kite"
)

// realizedPnL must return the REALIZED part of a Kite position row and nothing
// else. The engine adds unrealized on top of whatever this returns, so any
// mark-to-market leaking through here is counted twice — see the note on
// realizedPnL itself.
func TestRealizedPnL(t *testing.T) {
	cases := []struct {
		name string
		pos  kite.KitePosition
		want float64
	}{{
		// Bought and still holding. The price has moved, but nothing is
		// realized: Kite's own `pnl` would report +200 here, and the engine's
		// mark would then add the same +200 again.
		name: "untouched long realizes nothing",
		pos: kite.KitePosition{
			Quantity: 100, AveragePrice: 10, LastPrice: 12,
			BuyValue: 1000, SellValue: 0, Multiplier: 1,
		},
		want: 0,
	}, {
		name: "untouched short realizes nothing",
		pos: kite.KitePosition{
			Quantity: -100, AveragePrice: 10, LastPrice: 8,
			BuyValue: 0, SellValue: 1000, Multiplier: 1,
		},
		want: 0,
	}, {
		// Sold 40 of 100 at 12, cost 10 → 40 * 2 realized, the other 60 open.
		name: "partially closed long realizes only the closed portion",
		pos: kite.KitePosition{
			Quantity: 60, AveragePrice: 10, LastPrice: 15,
			BuyValue: 1000, SellValue: 480, Multiplier: 1,
		},
		want: 80,
	}, {
		// Short 100 at 10, bought back 40 at 8 → 40 * 2 realized.
		name: "partially closed short realizes only the closed portion",
		pos: kite.KitePosition{
			Quantity: -60, AveragePrice: 10, LastPrice: 5,
			BuyValue: 320, SellValue: 1000, Multiplier: 1,
		},
		want: 80,
	}, {
		// The case the daily-loss limit cares about: fully closed at a loss.
		// Kite zeroes average_price on a flat row, so the whole figure is the
		// difference between what was sold and what was bought.
		name: "fully closed loss survives in full",
		pos: kite.KitePosition{
			Quantity: 0, AveragePrice: 0, LastPrice: 8,
			BuyValue: 1000, SellValue: 700, Multiplier: 1,
		},
		want: -300,
	}, {
		name: "fully closed gain survives in full",
		pos: kite.KitePosition{
			Quantity: 0, AveragePrice: 0, LastPrice: 12,
			BuyValue: 1000, SellValue: 1200, Multiplier: 1,
		},
		want: 200,
	}, {
		// A missing multiplier must behave as 1, not 0. At 0 the open leg's cost
		// basis drops out and the entire unrealized move is reported as
		// realized — the loud version of the bug this function exists to fix.
		name: "absent multiplier defaults to one",
		pos: kite.KitePosition{
			Quantity: 100, AveragePrice: 10, LastPrice: 12,
			BuyValue: 1000, SellValue: 0, Multiplier: 0,
		},
		want: 0,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := realizedPnL(tc.pos); got != tc.want {
				t.Errorf("realizedPnL() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A fully-closed live position must still come back from GetPositions. Dropping
// it discarded the day's realized P&L, which is what the daily-loss limit is
// computed from.
func TestLiveGetPositionsKeepsFlatRows(t *testing.T) {
	// One open short and one round trip closed at a 300 loss.
	payload := map[string]any{
		"status": "success",
		"data": map[string]any{
			"day": []any{},
			"net": []any{
				map[string]any{
					"tradingsymbol": "NIFTY2681824350CE", "exchange": "NFO",
					"product": "MIS", "quantity": -75, "multiplier": 1.0,
					"average_price": 120.0, "last_price": 110.0,
					"pnl": 750.0, "buy_value": 0.0, "sell_value": 9000.0,
				},
				map[string]any{
					"tradingsymbol": "NIFTY2681824350PE", "exchange": "NFO",
					"product": "MIS", "quantity": 0, "multiplier": 1.0,
					"average_price": 0.0, "last_price": 95.0,
					"pnl": -300.0, "buy_value": 1000.0, "sell_value": 700.0,
				},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/portfolio/positions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	b := NewLiveBroker(kite.New("k", "s", "token", srv.URL, nil), nil, -1)
	got, err := b.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d positions, want 2 (the flat row must survive): %+v", len(got), got)
	}

	bySymbol := make(map[string]Position, len(got))
	for _, p := range got {
		bySymbol[p.TradingSymbol] = p
	}

	open := bySymbol["NIFTY2681824350CE"]
	if !open.IsOpen() {
		t.Error("open short reported as flat")
	}
	// Realized only: Kite's pnl of 750 is entirely unrealized here, and the
	// engine's mark would add it a second time.
	if open.PnL != 0 {
		t.Errorf("open short PnL = %v, want 0 (realized only)", open.PnL)
	}

	closed := bySymbol["NIFTY2681824350PE"]
	if closed.IsOpen() {
		t.Error("flat row reported as open — it would be square-off'd and counted against the exposure limit")
	}
	if closed.PnL != -300 {
		t.Errorf("closed position PnL = %v, want -300", closed.PnL)
	}
}
