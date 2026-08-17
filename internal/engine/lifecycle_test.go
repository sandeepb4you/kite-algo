package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/events"
	"kite-algo/internal/marketdata"
	"kite-algo/internal/risk"
	"kite-algo/internal/strategy"
)

// fakeStrategy is a controllable strategy for lifecycle tests.
type fakeStrategy struct {
	name string

	ticks   atomic.Int64
	stopped atomic.Bool
	flatten atomic.Bool

	initErr     error
	panicOnTick bool

	mu     sync.Mutex
	trader strategy.Trader
}

func (f *fakeStrategy) Name() string { return f.name }

func (f *fakeStrategy) Init(ctx context.Context, t strategy.Trader, cfg config.StrategyCfg) error {
	if f.initErr != nil {
		return f.initErr
	}
	f.mu.Lock()
	f.trader = t
	f.mu.Unlock()
	return nil
}

func (f *fakeStrategy) OnTick(ctx context.Context, tick marketdata.Tick) {
	if f.panicOnTick {
		panic("deliberate strategy panic")
	}
	f.ticks.Add(1)
}

func (f *fakeStrategy) OnFill(ctx context.Context, fill broker.Fill) {}

func (f *fakeStrategy) Stop(ctx context.Context) error {
	f.stopped.Store(true)
	return nil
}

// SquareOff satisfies strategy.Flattener.
func (f *fakeStrategy) SquareOff(ctx context.Context, reason string) error {
	f.flatten.Store(true)
	return nil
}

// lifecycleEngine builds an engine with a registry containing one fake type.
func lifecycleEngine(t *testing.T) (*Engine, *strategy.Registry, map[string]*fakeStrategy) {
	t.Helper()
	built := make(map[string]*fakeStrategy)

	reg := strategy.NewRegistry()
	reg.Register(strategy.Descriptor{
		Type:  "fake",
		Title: "Fake",
		Factory: func(id string, _ *slog.Logger) (strategy.Strategy, error) {
			f := &fakeStrategy{name: id}
			built[id] = f
			return f, nil
		},
		Params: []strategy.ParamSpec{
			{Key: "lots", Kind: strategy.KindInt, Default: 1, Min: strategy.Ptr(1), Max: strategy.Ptr(5)},
		},
	})

	paper := broker.NewPaperBroker(nil, nil)
	e := New(paper, nullStore{}, risk.NewManager(risk.Limits{MaxLotsPerTrade: 10}), false, nil,
		WithPaperBroker(paper), WithEventPublisher(events.Nop{}), WithRegistry(reg))
	paper.SetOnFill(e.handleFill)
	e.runCtx = context.Background()
	return e, reg, built
}

func TestStartAndStopStrategy(t *testing.T) {
	e, _, built := lifecycleEngine(t)
	ctx := context.Background()

	st, err := e.StartStrategy(ctx, StrategySpec{Type: "fake", Params: map[string]any{"lots": "2"}})
	if err != nil {
		t.Fatalf("StartStrategy: %v", err)
	}
	if st.State != StateRunning {
		t.Errorf("state = %s, want running", st.State)
	}
	if st.Params["lots"] != 2 {
		t.Errorf("params were not normalized: %#v", st.Params)
	}

	// A running strategy receives ticks.
	e.handleTick(marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 100})
	if got := built["fake"].ticks.Load(); got != 1 {
		t.Fatalf("running strategy received %d ticks, want 1", got)
	}

	if _, err := e.StopStrategy(ctx, "fake", StopOptions{Reason: "test"}); err != nil {
		t.Fatalf("StopStrategy: %v", err)
	}
	if !built["fake"].stopped.Load() {
		t.Error("Stop was not called on the instance")
	}

	// A stopped strategy receives nothing further.
	e.handleTick(marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 101})
	if got := built["fake"].ticks.Load(); got != 1 {
		t.Errorf("stopped strategy received %d ticks, want it to stay at 1", got)
	}
}

// TestStopWithSquareOffPrefersStrategyUnwind checks the engine asks the strategy
// to flatten itself when it knows how — a short straddle must buy back its legs
// in its own order rather than being closed leg-by-leg generically.
func TestStopWithSquareOffPrefersStrategyUnwind(t *testing.T) {
	e, _, built := lifecycleEngine(t)
	ctx := context.Background()

	if _, err := e.StartStrategy(ctx, StrategySpec{Type: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StopStrategy(ctx, "fake", StopOptions{SquareOff: true, Reason: "test"}); err != nil {
		t.Fatalf("StopStrategy: %v", err)
	}
	if !built["fake"].flatten.Load() {
		t.Error("SquareOff was not called on a strategy implementing Flattener")
	}
}

// TestStopWithoutSquareOffLeavesPositions is the other half of the explicit
// choice: not asking to flatten must not flatten.
func TestStopWithoutSquareOffLeavesPositions(t *testing.T) {
	e, _, built := lifecycleEngine(t)
	ctx := context.Background()

	if _, err := e.StartStrategy(ctx, StrategySpec{Type: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StopStrategy(ctx, "fake", StopOptions{SquareOff: false}); err != nil {
		t.Fatal(err)
	}
	if built["fake"].flatten.Load() {
		t.Error("positions were squared off without being asked")
	}
}

func TestStartRejectsDuplicateInstance(t *testing.T) {
	e, _, _ := lifecycleEngine(t)
	ctx := context.Background()

	if _, err := e.StartStrategy(ctx, StrategySpec{Type: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartStrategy(ctx, StrategySpec{Type: "fake"}); err == nil {
		t.Error("starting the same instance twice was allowed")
	}
}

func TestStartRejectsInvalidParams(t *testing.T) {
	e, _, _ := lifecycleEngine(t)
	_, err := e.StartStrategy(context.Background(), StrategySpec{
		Type:   "fake",
		Params: map[string]any{"lots": "99"}, // above the declared max
	})
	var ve *strategy.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a *strategy.ValidationError", err)
	}
}

func TestStartRejectsUnknownType(t *testing.T) {
	e, _, _ := lifecycleEngine(t)
	if _, err := e.StartStrategy(context.Background(), StrategySpec{Type: "nope"}); err == nil {
		t.Error("unknown strategy type was accepted")
	}
}

// TestFailedInitNeverReceivesTicks ensures a strategy that could not initialize
// is not published to the fan-out.
func TestFailedInitNeverReceivesTicks(t *testing.T) {
	reg := strategy.NewRegistry()
	boom := errors.New("init failed")
	reg.Register(strategy.Descriptor{
		Type: "broken",
		Factory: func(id string, _ *slog.Logger) (strategy.Strategy, error) {
			return &fakeStrategy{name: id, initErr: boom}, nil
		},
	})

	paper := broker.NewPaperBroker(nil, nil)
	e := New(paper, nullStore{}, risk.NewManager(risk.Limits{}), false, nil,
		WithPaperBroker(paper), WithEventPublisher(events.Nop{}), WithRegistry(reg))
	e.runCtx = context.Background()

	st, err := e.StartStrategy(context.Background(), StrategySpec{Type: "broken"})
	if err == nil {
		t.Fatal("a failing Init should surface as an error")
	}
	if st.State != StateErrored {
		t.Errorf("state = %s, want errored", st.State)
	}
	if n := len(e.activeStrategies()); n != 0 {
		t.Errorf("%d strategies in the fan-out after a failed Init, want 0", n)
	}
}

// TestPanicInOnTickQuarantinesStrategy is the isolation guarantee. The fan-out
// runs on the market-data goroutine, so an unrecovered panic would kill the
// process — stopping market data and the web UI while positions stayed open.
func TestPanicInOnTickQuarantinesStrategy(t *testing.T) {
	reg := strategy.NewRegistry()
	reg.Register(strategy.Descriptor{
		Type: "panicky",
		Factory: func(id string, _ *slog.Logger) (strategy.Strategy, error) {
			return &fakeStrategy{name: id, panicOnTick: true}, nil
		},
	})
	reg.Register(strategy.Descriptor{
		Type: "healthy",
		Factory: func(id string, _ *slog.Logger) (strategy.Strategy, error) {
			return &fakeStrategy{name: id}, nil
		},
	})

	paper := broker.NewPaperBroker(nil, nil)
	e := New(paper, nullStore{}, risk.NewManager(risk.Limits{}), false, nil,
		WithPaperBroker(paper), WithEventPublisher(events.Nop{}), WithRegistry(reg))
	e.runCtx = context.Background()
	ctx := context.Background()

	if _, err := e.StartStrategy(ctx, StrategySpec{InstanceID: "bad", Type: "panicky"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartStrategy(ctx, StrategySpec{InstanceID: "good", Type: "healthy"}); err != nil {
		t.Fatal(err)
	}

	// Must not panic the test process.
	e.handleTick(marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 100})

	bad, _ := e.StrategyStatusByID("bad")
	if bad.State != StateErrored {
		t.Errorf("panicking strategy state = %s, want errored", bad.State)
	}
	if bad.Error == "" {
		t.Error("no error recorded for the quarantined strategy")
	}

	good, _ := e.StrategyStatusByID("good")
	if good.State != StateRunning {
		t.Errorf("healthy strategy state = %s, want it unaffected", good.State)
	}

	// The healthy strategy keeps receiving data afterwards.
	e.handleTick(marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 101})
	if good, _ = e.StrategyStatusByID("good"); good.TickCount < 2 {
		t.Errorf("healthy strategy tick count = %d, want it still receiving", good.TickCount)
	}
}

// TestConcurrentLifecycleAndTicks exercises the copy-on-write fan-out under
// simultaneous mutation and delivery, the exact shape of an operator clicking
// start/stop while the market is ticking.
func TestConcurrentLifecycleAndTicks(t *testing.T) {
	e, _, _ := lifecycleEngine(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				e.handleTick(marketdata.Tick{TradingSymbol: "NIFTY 50", LastPrice: 100})
			}
		}
	}()

	for i := 0; i < 50; i++ {
		if _, err := e.StartStrategy(ctx, StrategySpec{InstanceID: "churn", Type: "fake"}); err != nil {
			t.Errorf("start: %v", err)
			break
		}
		if _, err := e.StopStrategy(ctx, "churn", StopOptions{}); err != nil {
			t.Errorf("stop: %v", err)
			break
		}
	}

	close(stop)
	wg.Wait()
}

// TestListStrategiesIsSorted keeps the UI's card order stable between polls.
func TestListStrategiesIsSorted(t *testing.T) {
	e, _, _ := lifecycleEngine(t)
	ctx := context.Background()
	for _, id := range []string{"zulu", "alpha", "mike"} {
		if _, err := e.StartStrategy(ctx, StrategySpec{InstanceID: id, Type: "fake"}); err != nil {
			t.Fatal(err)
		}
	}
	got := e.ListStrategies()
	if len(got) != 3 || got[0].InstanceID != "alpha" || got[2].InstanceID != "zulu" {
		t.Errorf("ListStrategies order = %v, want alphabetical", got)
	}
}

func TestEngineNowReturnsWallClock(t *testing.T) {
	e, _, _ := lifecycleEngine(t)
	before := time.Now()
	got := e.Now()
	if got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Errorf("Now() = %v, want approximately wall-clock time", got)
	}
}

func TestSignalRecordsLastSignal(t *testing.T) {
	e, _, _ := lifecycleEngine(t)
	ctx := context.Background()
	if _, err := e.StartStrategy(ctx, StrategySpec{Type: "fake"}); err != nil {
		t.Fatal(err)
	}

	e.Signal(strategy.Signal{StrategyID: "fake", Kind: "enter", Message: "sold the straddle"})

	st, _ := e.StrategyStatusByID("fake")
	if st.LastSignal == nil {
		t.Fatal("no last signal recorded")
	}
	if st.LastSignal.Message != "sold the straddle" {
		t.Errorf("last signal = %q", st.LastSignal.Message)
	}
	if st.LastSignal.At.IsZero() {
		t.Error("signal timestamp was not filled in")
	}
}

// resumableStrategy records what it was handed to resume.
type resumableStrategy struct {
	fakeStrategy
	resumeErr error

	mu       sync.Mutex
	resumed  []broker.Position
	resumeAt int // how many ticks had arrived when Resume ran
}

func (r *resumableStrategy) Resume(_ context.Context, p []broker.Position) error {
	if r.resumeErr != nil {
		return r.resumeErr
	}
	r.mu.Lock()
	r.resumed = append(r.resumed, p...)
	r.resumeAt = int(r.ticks.Load())
	r.mu.Unlock()
	return nil
}

// TestResumeIsRefusedWhenTheStrategyCannotRebuildState is the engine-side half
// of the double-entry guard.
//
// A strategy that cannot reconstruct its state from its positions must not be
// started while it holds any: it would treat the open position as absent and
// trade on top of it. Refusing leaves the position unmanaged, which is bad — but
// it is visible, and the orphan banner says so. A doubled position is neither.
func TestResumeIsRefusedWhenTheStrategyCannotRebuildState(t *testing.T) {
	e, _, _ := lifecycleEngine(t)

	_, err := e.StartStrategy(context.Background(), StrategySpec{
		Type: "fake",
		Resume: []broker.Position{
			{TradingSymbol: "NIFTY24AUG24500CE", NetQuantity: -75},
		},
	})
	if err == nil {
		t.Fatal("started a non-Resumable strategy holding an open position; " +
			"it would have re-entered on top of it")
	}
	if !strings.Contains(err.Error(), "cannot rebuild its state") {
		t.Errorf("error = %q, want it to name the reason", err)
	}
	if len(e.ListStrategies()) != 0 {
		t.Error("refused instance was left registered")
	}
}

// TestResumeRunsBeforeAnyTick pins the ordering. Resume happens after Init and
// before the instance joins the fan-out snapshot, because a tick delivered to a
// strategy that still believes it is flat is the whole failure being prevented.
func TestResumeRunsBeforeAnyTick(t *testing.T) {
	e, reg, _ := lifecycleEngine(t)
	var built *resumableStrategy

	reg.Register(strategy.Descriptor{
		Type:  "resumable",
		Title: "Resumable",
		Factory: func(id string, _ *slog.Logger) (strategy.Strategy, error) {
			built = &resumableStrategy{fakeStrategy: fakeStrategy{name: id}}
			return built, nil
		},
	})

	held := []broker.Position{
		{TradingSymbol: "NIFTY24AUG24500CE", NetQuantity: -75},
		{TradingSymbol: "NIFTY24AUG24500PE", NetQuantity: -75},
	}
	if _, err := e.StartStrategy(context.Background(), StrategySpec{
		Type: "resumable", Resume: held,
	}); err != nil {
		t.Fatalf("StartStrategy: %v", err)
	}

	built.mu.Lock()
	defer built.mu.Unlock()
	if len(built.resumed) != 2 {
		t.Fatalf("Resume got %d positions, want 2", len(built.resumed))
	}
	if built.resumeAt != 0 {
		t.Errorf("Resume ran after %d ticks; it must run before any", built.resumeAt)
	}
}

// TestResumeFailureLeavesTheStrategyErrored rather than silently running. A
// strategy whose state could not be rebuilt is in exactly the condition the
// refusal above exists to prevent.
func TestResumeFailureLeavesTheStrategyErrored(t *testing.T) {
	e, reg, _ := lifecycleEngine(t)

	reg.Register(strategy.Descriptor{
		Type:  "badresume",
		Title: "Bad resume",
		Factory: func(id string, _ *slog.Logger) (strategy.Strategy, error) {
			return &resumableStrategy{
				fakeStrategy: fakeStrategy{name: id},
				resumeErr:    errors.New("chain unavailable"),
			}, nil
		},
	})

	_, err := e.StartStrategy(context.Background(), StrategySpec{
		Type:   "badresume",
		Resume: []broker.Position{{TradingSymbol: "X", NetQuantity: -75}},
	})
	if err == nil {
		t.Fatal("a strategy whose Resume failed was started anyway")
	}
	for _, s := range e.ListStrategies() {
		if s.State == StateRunning {
			t.Errorf("instance %s is running after a failed resume", s.InstanceID)
		}
	}
}
