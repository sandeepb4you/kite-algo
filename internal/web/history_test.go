package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"kite-algo/internal/app"
	"kite-algo/internal/broker"
	"kite-algo/internal/history"
	"kite-algo/internal/storage"
)

// seedRoundTrip writes an order and its two fills — a sell and the buy that
// closes it — the shape the short-straddle strategy produces.
func seedRoundTrip(t *testing.T, a *app.App, strategyID, symbol string, entry time.Time, in, out float64) {
	t.Helper()
	store, ok := a.Store.(storage.TradeStore)
	if !ok {
		t.Fatal("store is not a TradeStore")
	}
	full, ok := a.Store.(interface {
		SaveOrder(context.Context, *broker.Order) error
		SaveFill(context.Context, *broker.Fill) error
	})
	if !ok {
		t.Fatal("store cannot write orders/fills")
	}
	_ = store
	ctx := context.Background()

	mk := func(idSuffix string, side broker.Side, price float64, at time.Time) {
		o := &broker.Order{
			ID: symbol + idSuffix, StrategyID: strategyID, Exchange: "NFO",
			TradingSymbol: symbol, Product: broker.ProductMIS,
			OrderType: broker.OrderTypeMarket, Side: side, Quantity: 65,
			FilledQuantity: 65, Validity: broker.ValidityDay,
			Status: broker.StatusComplete, Mode: "paper",
			CreatedAt: at, UpdatedAt: at,
		}
		if err := full.SaveOrder(ctx, o); err != nil {
			t.Fatalf("save order: %v", err)
		}
		f := &broker.Fill{
			ID: symbol + idSuffix + "-f", OrderID: o.ID, StrategyID: strategyID,
			Exchange: "NFO", TradingSymbol: symbol, Side: side,
			Quantity: 65, Price: price, Mode: "paper", Timestamp: at,
		}
		if err := full.SaveFill(ctx, f); err != nil {
			t.Fatalf("save fill: %v", err)
		}
	}
	mk("-s", broker.SideSell, in, entry)
	mk("-b", broker.SideBuy, out, entry.Add(4*time.Hour))
}

func TestHistoryPageShowsRoundTripsAndSummary(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)

	// Sold at 146.95, bought back at 133.40 → a profitable short.
	seedRoundTrip(t, a, "short-straddle", "NIFTY2681824350CE",
		time.Date(2026, 8, 14, 9, 15, 0, 0, history.IST), 146.95, 133.40)

	code, body := getStatusBody(t, c, ts.URL+"/history")
	if code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	if !strings.Contains(body, "NIFTY2681824350CE") {
		t.Error("round trip missing from the page")
	}
	if !strings.Contains(body, "short-straddle") {
		t.Error("strategy name missing")
	}
	if !strings.Contains(body, "Round trips") {
		t.Error("trade table missing")
	}
}

// The window must default to where the data actually is. A last-30-days default
// opens empty for anyone whose trading predates it, which reads as "no history"
// rather than "wrong month".
func TestHistoryDefaultsToTheSpanOfStoredFills(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	seedRoundTrip(t, a, "short-straddle", "NIFTY2681824350PE",
		time.Date(2026, 8, 14, 9, 15, 0, 0, history.IST), 74.95, 52.35)

	body := getBody(t, c, ts.URL+"/history")
	if !strings.Contains(body, "2026-08-14") {
		t.Errorf("date range did not default to the stored fills; body:\n%s", truncate(body, 700))
	}
}

func TestHistoryGroupsByPeriod(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	seedRoundTrip(t, a, "short-straddle", "NIFTY2681824350CE",
		time.Date(2026, 8, 10, 9, 15, 0, 0, history.IST), 100, 80)
	seedRoundTrip(t, a, "short-straddle", "NIFTY2681824400CE",
		time.Date(2026, 9, 15, 9, 15, 0, 0, history.IST), 100, 120)

	monthly := getBody(t, c, ts.URL+"/history?from=2026-08-01&to=2026-09-30&period=monthly")
	if !strings.Contains(monthly, "August 2026") || !strings.Contains(monthly, "September 2026") {
		t.Errorf("monthly buckets missing; body:\n%s", truncate(monthly, 900))
	}

	weekly := getBody(t, c, ts.URL+"/history?from=2026-08-01&to=2026-09-30&period=weekly")
	if !strings.Contains(weekly, "W33") {
		t.Errorf("weekly buckets missing; body:\n%s", truncate(weekly, 900))
	}
}

func TestHistoryFiltersByStrategy(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)
	entry := time.Date(2026, 8, 14, 9, 15, 0, 0, history.IST)
	seedRoundTrip(t, a, "short-straddle", "NIFTY2681824350CE", entry, 100, 90)
	seedRoundTrip(t, a, "manual", "NIFTY2681824400PE", entry, 50, 40)

	body := getBody(t, c, ts.URL+"/history?from=2026-08-14&to=2026-08-14&strategy=short-straddle")
	if !strings.Contains(body, "NIFTY2681824350CE") {
		t.Error("selected strategy's trade is missing")
	}
	if strings.Contains(body, "NIFTY2681824400PE") {
		t.Error("other strategy's trade leaked through the filter")
	}
}

// An open leg produces no round trip. Without saying so, a strategy still
// holding a position reads as one that did nothing.
func TestHistoryReportsStillOpenPositions(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)

	// One sell with no closing buy, plus a completed round trip so the page
	// renders its summary section at all.
	seedRoundTrip(t, a, "short-straddle", "NIFTY2681824350CE",
		time.Date(2026, 8, 14, 9, 15, 0, 0, history.IST), 100, 90)

	store := a.Store.(interface {
		SaveOrder(context.Context, *broker.Order) error
		SaveFill(context.Context, *broker.Fill) error
	})
	at := time.Date(2026, 8, 14, 11, 0, 0, 0, history.IST)
	o := &broker.Order{
		ID: "open-1", StrategyID: "short-straddle", Exchange: "NFO",
		TradingSymbol: "NIFTY2681824500PE", Product: broker.ProductMIS,
		OrderType: broker.OrderTypeMarket, Side: broker.SideSell, Quantity: 65,
		FilledQuantity: 65, Validity: broker.ValidityDay,
		Status: broker.StatusComplete, Mode: "paper", CreatedAt: at, UpdatedAt: at,
	}
	if err := store.SaveOrder(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFill(context.Background(), &broker.Fill{
		ID: "open-1-f", OrderID: "open-1", StrategyID: "short-straddle",
		Exchange: "NFO", TradingSymbol: "NIFTY2681824500PE", Side: broker.SideSell,
		Quantity: 65, Price: 80, Mode: "paper", Timestamp: at,
	}); err != nil {
		t.Fatal(err)
	}

	body := getBody(t, c, ts.URL+"/history?from=2026-08-14&to=2026-08-14")
	if !strings.Contains(body, "still had an open position") {
		t.Errorf("open leg not reported; body:\n%s", truncate(body, 900))
	}
}

// Rejected orders live only in the order log, and they are what answers "why
// did nothing happen?".
func TestHistoryShowsRejectedOrders(t *testing.T) {
	ts, a := newTestServer(t)
	c := loginClient(t, ts)

	store := a.Store.(interface {
		SaveOrder(context.Context, *broker.Order) error
	})
	at := time.Date(2026, 8, 14, 9, 20, 0, 0, history.IST)
	if err := store.SaveOrder(context.Background(), &broker.Order{
		ID: "rej-1", StrategyID: "short-straddle", Exchange: "NFO",
		TradingSymbol: "NIFTY2681824350CE", Product: broker.ProductMIS,
		OrderType: broker.OrderTypeMarket, Side: broker.SideSell, Quantity: 650,
		Validity: broker.ValidityDay, Status: broker.StatusRejected,
		RejectReason: "max_lots_per_trade exceeded", Mode: "paper",
		CreatedAt: at, UpdatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}

	body := getBody(t, c, ts.URL+"/history?from=2026-08-14&to=2026-08-14")
	if !strings.Contains(body, "max_lots_per_trade exceeded") {
		t.Errorf("rejected order's reason missing; body:\n%s", truncate(body, 900))
	}
}

func TestHistorySaysSoWhenThereAreNoFills(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loginClient(t, ts)

	body := getBody(t, c, ts.URL+"/history")
	if !strings.Contains(body, "No fills have been recorded yet") {
		t.Errorf("empty state not explained; body:\n%s", truncate(body, 700))
	}
}
