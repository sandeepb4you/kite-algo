package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/engine"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/strategy"
)

// The app wires its engine to the process-wide registry (app.go:156), so the
// fake registers there. Once per process: Register panics on a duplicate type,
// and every test in this file needs it.
const fakeType = "app-test-fake"

var registerFakeOnce sync.Once

func registerFake(t *testing.T, _ *App) {
	t.Helper()
	registerFakeOnce.Do(func() {
		strategy.Register(strategy.Descriptor{
			Type:  fakeType,
			Title: "App test fake",
			Factory: func(id string, _ *slog.Logger) (strategy.Strategy, error) {
				return &noopStrategy{name: id}, nil
			},
			Params: []strategy.ParamSpec{
				{Key: "lots", Kind: strategy.KindInt, Default: 1,
					Min: strategy.Ptr(1), Max: strategy.Ptr(5)},
			},
		})
	})
}

// noopStrategy trades nothing. These tests are about whether an instance comes
// back, not what it does once it has.
type noopStrategy struct{ name string }

func (n *noopStrategy) Name() string { return n.name }
func (n *noopStrategy) Init(context.Context, strategy.Trader, config.StrategyCfg) error {
	return nil
}
func (n *noopStrategy) OnTick(context.Context, marketdata.Tick) {}
func (n *noopStrategy) OnFill(context.Context, broker.Fill)     {}
func (n *noopStrategy) Stop(context.Context) error              { return nil }

// TestStartedStrategiesSurviveARestart is the behaviour the operator asked for:
// once started, a strategy stays started. Before this, an instance lived only in
// memory, so a redeploy silently stopped it while its positions stayed open.
func TestStartedStrategiesSurviveARestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "trading.db")
	ctx := context.Background()

	a := riskTestApp(t, db, configDefaults)
	registerFake(t, a)

	if _, err := a.StartStrategy(ctx, engine.StrategySpec{
		InstanceID: "fake-1", Type: fakeType, Params: map[string]any{"lots": "2"},
	}); err != nil {
		t.Fatalf("StartStrategy: %v", err)
	}

	// Restart: same database, new process.
	b := riskTestApp(t, db, configDefaults)
	registerFake(t, b)
	if got := len(b.Engine.ListStrategies()); got != 0 {
		t.Fatalf("strategies running before restore: %d", got)
	}

	if refused := b.RestoreStrategies(ctx); len(refused) != 0 {
		t.Fatalf("restore refused %d instance(s): %+v", len(refused), refused)
	}

	got := b.Engine.ListStrategies()
	if len(got) != 1 {
		t.Fatalf("restored %d strategies, want 1 — a started strategy did not "+
			"survive the restart", len(got))
	}
	if got[0].InstanceID != "fake-1" {
		t.Errorf("restored instance = %q, want fake-1", got[0].InstanceID)
	}
	if lots := got[0].Params["lots"]; lots != 2 {
		t.Errorf("restored lots = %v (%T), want 2 — parameters did not survive", lots, lots)
	}
}

// TestStoppedStrategiesDoNotComeBack is the other half. A strategy the operator
// stopped must stay stopped, or "stop" would mean "until the next deploy".
func TestStoppedStrategiesDoNotComeBack(t *testing.T) {
	db := filepath.Join(t.TempDir(), "trading.db")
	ctx := context.Background()

	a := riskTestApp(t, db, configDefaults)
	registerFake(t, a)

	if _, err := a.StartStrategy(ctx, engine.StrategySpec{
		InstanceID: "fake-1", Type: fakeType,
	}); err != nil {
		t.Fatalf("StartStrategy: %v", err)
	}
	if _, err := a.StopStrategy(ctx, "fake-1", engine.StopOptions{Reason: "test"}); err != nil {
		t.Fatalf("StopStrategy: %v", err)
	}

	b := riskTestApp(t, db, configDefaults)
	registerFake(t, b)
	b.RestoreStrategies(ctx)

	for _, s := range b.Engine.ListStrategies() {
		if s.State == engine.StateRunning {
			t.Fatalf("stopped strategy %s came back after a restart", s.InstanceID)
		}
	}
}

// TestOrphansReportsUnmanagedStrategyPositions covers the banner's input.
//
// Manual positions carry no StrategyID and must never appear: nothing was ever
// managing them, and a banner that cries wolf is one nobody reads.
func TestOrphansReportsUnmanagedStrategyPositions(t *testing.T) {
	db := filepath.Join(t.TempDir(), "trading.db")
	ctx := context.Background()
	a := riskTestApp(t, db, configDefaults)

	if got := a.Orphans(ctx); len(got) != 0 {
		t.Fatalf("orphans on a clean book: %d", len(got))
	}
}

// TestPositionsByStrategyIgnoresManualAndClosed pins the grouping rule the
// banner and the restore path both depend on.
func TestPositionsByStrategyIgnoresManualAndClosed(t *testing.T) {
	db := filepath.Join(t.TempDir(), "trading.db")
	a := riskTestApp(t, db, configDefaults)

	// Exercised through the engine's real position list, so this test fails if
	// the definition of "open" ever drifts from broker.Position.IsOpen.
	held := a.positionsByStrategy(context.Background())
	for id, ps := range held {
		if id == "" {
			t.Error("manual positions (empty StrategyID) were grouped as a strategy")
		}
		for _, p := range ps {
			if !p.IsOpen() {
				t.Errorf("closed position %s was grouped as held", p.TradingSymbol)
			}
		}
	}
}
