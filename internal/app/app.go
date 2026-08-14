// Package app is the supervisor that owns the trading platform's runtime.
//
// It exists because the process changed shape: it used to be a CLI that wired
// everything at startup and exited if credentials were missing, and it is now a
// long-running server that must come up with no credentials at all, serve a
// login page, and acquire its Zerodha session afterwards.
//
// App holds that lifecycle so neither cmd/trading nor internal/web has to. The
// web layer talks to App and Engine; it never reaches into the Kite client.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"kite-algo/internal/auth"
	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/engine"
	"kite-algo/internal/events"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
	"kite-algo/internal/strategy"

	// Links the built-in strategies into the binary so they self-register.
	_ "kite-algo/internal/strategy/catalog"
)

// App wires and supervises the platform.
type App struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Store    storage.Store
	Bus      *events.Bus
	Engine   *engine.Engine
	Risk     *risk.Manager
	Kite     *KiteSession
	Sessions *auth.Sessions
	Guard    *auth.LoginGuard

	// paper is the simulated broker. It is ALWAYS the broker the engine starts
	// with, including when the config says live: real order routing is only
	// installed after an explicit runtime confirmation. See LiveArmed.
	paper *broker.PaperBroker

	bootAt time.Time

	mu       sync.RWMutex
	liveMode bool // true once the live broker has been swapped in
}

// Status is everything the UI header needs in one struct.
type Status struct {
	Mode        config.Mode      `json:"mode"`
	LiveArmed   bool             `json:"live_armed"`  // configured for live, awaiting confirmation
	LiveActive  bool             `json:"live_active"` // real orders are being routed
	Kite        SessionInfo      `json:"kite"`
	Streaming   bool             `json:"streaming"`
	BootAt      time.Time        `json:"boot_at"`
	Uptime      string           `json:"uptime"`
	DayPnL      float64          `json:"day_pnl"`
	RiskLimits  risk.Limits      `json:"risk_limits"`
	Halt        engine.HaltState `json:"halt"`
	RedirectURL string           `json:"redirect_url"`
}

// New builds the platform without touching the network.
//
// Nothing here can fail because Zerodha is unreachable or a token is missing —
// that is the entire point. The Kite session is restored opportunistically and
// its absence is a normal, recoverable state.
func New(ctx context.Context, cfg *config.Config, store storage.Store, log *slog.Logger) (*App, error) {
	bus := events.NewBus(log)

	riskMgr := risk.NewManager(risk.Limits{
		MaxDailyLoss:     cfg.Risk.MaxDailyLoss,
		MaxOpenPositions: cfg.Risk.MaxOpenPositions,
		MaxOrderValue:    cfg.Risk.MaxOrderValue,
		MaxLotsPerTrade:  cfg.Risk.MaxLotsPerTrade,
	})

	// Always start on the paper broker, whatever the configured mode. In live
	// mode this is gate two of three: the process boots "armed but not
	// confirmed", so nothing can reach the exchange until the operator confirms
	// in the UI. Previously a live broker was constructed at startup, before any
	// human had seen anything.
	paper := broker.NewPaperBroker(nil, log)

	eng := engine.New(paper, store, riskMgr, cfg.Recording.Ticks, log,
		engine.WithEventPublisher(bus),
		engine.WithStrategyConfigs(strategyConfigs(cfg)),
		engine.WithRegistry(strategy.Default),
	)

	secure := !config.IsLoopbackAddr(cfg.Web.Addr)
	a := &App{
		Cfg:      cfg,
		Log:      log,
		Store:    store,
		Bus:      bus,
		Engine:   eng,
		Risk:     riskMgr,
		Sessions: auth.NewSessions(store, cfg.Web.SessionTTL, secure, log),
		Guard:    auth.NewLoginGuard(),
		paper:    paper,
		bootAt:   time.Now(),
	}
	a.Kite = NewKiteSession(cfg, store, eng, bus, log)

	// Best-effort: pick up a token persisted earlier today.
	if err := a.Kite.Restore(ctx); err != nil && log != nil {
		log.Warn("restore kite session failed", "err", err)
	}
	return a, nil
}

// Run starts the engine and background supervisors, blocking until ctx ends.
func (a *App) Run(ctx context.Context) error {
	go a.Kite.Supervise(ctx)
	go a.Sessions.GC(ctx, time.Hour)
	go a.guardSweep(ctx)
	go a.autoStartStrategies(ctx)
	return a.Engine.Start(ctx)
}

// autoStartStrategies starts the strategies marked enabled in config.yaml.
//
// It waits for a Zerodha session first: a strategy started without an
// instrument master cannot resolve its option chain, so it would sit idle and
// look broken. Once market data is up, config-enabled strategies come online by
// themselves — which is what makes an unattended restart resume trading.
func (a *App) autoStartStrategies(ctx context.Context) {
	enabled := make([]config.StrategyCfg, 0, len(a.Cfg.Strategies))
	for _, sc := range a.Cfg.Strategies {
		if sc.Enabled {
			enabled = append(enabled, sc)
		}
	}
	if len(enabled) == 0 {
		return
	}

	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !a.Engine.HasMarketData() {
				continue
			}
			for _, sc := range enabled {
				if _, running := a.Engine.StrategyStatusByID(sc.Name); running {
					continue
				}
				st, err := a.Engine.StartStrategy(ctx, engine.StrategySpec{
					InstanceID: sc.Name,
					Type:       sc.Name,
					Params:     sc.Params,
				})
				if err != nil {
					a.Log.Error("could not auto-start strategy from config",
						"name", sc.Name, "err", err)
					continue
				}
				a.Log.Info("auto-started strategy from config",
					"name", sc.Name, "state", st.State)
			}
			return
		}
	}
}

// Shutdown stops strategies and releases resources.
func (a *App) Shutdown(ctx context.Context) error {
	a.Engine.Stop(ctx)
	a.Bus.Close()
	return nil
}

// Status snapshots the platform for rendering.
func (a *App) Status() Status {
	a.mu.RLock()
	live := a.liveMode
	a.mu.RUnlock()

	info := a.Kite.Snapshot()
	return Status{
		Mode:        a.Cfg.Mode,
		LiveArmed:   a.Cfg.Mode == config.ModeLive && !live,
		LiveActive:  live,
		Kite:        info,
		Streaming:   info.Streaming,
		BootAt:      a.bootAt,
		Uptime:      time.Since(a.bootAt).Round(time.Second).String(),
		DayPnL:      a.Engine.DayPnL(),
		RiskLimits:  a.Risk.Limits(),
		Halt:        a.Engine.HaltState(),
		RedirectURL: a.Cfg.KiteRedirectURL(),
	}
}

// LiveActive reports whether real orders are currently being routed.
func (a *App) LiveActive() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.liveMode
}

// ConfirmLive is the third and final gate before real money moves.
//
// The gates are, in order: live_confirm in the config file; booting with a paper
// broker despite that; and this call, which requires the exact phrase and a
// re-entry of the operator password. Only here is a live broker installed.
//
// This replaces the old stdin "I UNDERSTAND" prompt, which could not work in a
// service that starts unattended under systemd.
func (a *App) ConfirmLive(ctx context.Context, phrase, password string) error {
	const required = "I UNDERSTAND"

	if a.Cfg.Mode != config.ModeLive {
		return fmt.Errorf("mode is %q, not live; set mode: live and live_confirm: true in the config first", a.Cfg.Mode)
	}
	if phrase != required {
		return fmt.Errorf("confirmation phrase did not match; type exactly %q", required)
	}
	// Re-entering the password defeats someone walking up to an unlocked,
	// already-logged-in browser.
	if !a.Cfg.HasWebPassword() || !auth.VerifyPassword(a.Cfg.Web.PasswordHash, password) {
		auth.DummyVerify(password)
		return fmt.Errorf("password incorrect")
	}

	live, err := a.Kite.LiveBrokerFor()
	if err != nil {
		return err
	}

	a.mu.Lock()
	if a.liveMode {
		a.mu.Unlock()
		return nil
	}
	a.liveMode = true
	a.mu.Unlock()

	a.Engine.SwapBroker(live)

	if a.Log != nil {
		a.Log.Warn("LIVE TRADING CONFIRMED — real orders will now be routed to the exchange")
	}
	a.Bus.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelWarn,
		Message: "LIVE trading confirmed — real orders are now active",
		Fields:  map[string]any{"live_active": true},
	})
	return nil
}

// DisarmLive returns order routing to the paper broker without a restart.
// Open positions are untouched; only new orders are affected.
func (a *App) DisarmLive(ctx context.Context) {
	a.mu.Lock()
	if !a.liveMode {
		a.mu.Unlock()
		return
	}
	a.liveMode = false
	a.mu.Unlock()

	a.Engine.SwapBroker(a.paper)
	if a.Log != nil {
		a.Log.Warn("live trading disarmed; new orders are simulated again")
	}
	a.Bus.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelWarn,
		Message: "live trading disarmed — new orders are simulated",
		Fields:  map[string]any{"live_active": false},
	})
}

// SetRiskLimits updates the live risk limits.
//
// Limits are runtime state, not config: when a position is moving against you,
// tightening the daily-loss cap should not require editing a YAML file and
// restarting the process that is holding the position.
func (a *App) SetRiskLimits(l risk.Limits) {
	old := a.Risk.Limits()
	a.Risk.SetLimits(l)

	if a.Log != nil {
		a.Log.Warn("risk limits changed",
			"max_daily_loss", l.MaxDailyLoss, "was", old.MaxDailyLoss,
			"max_order_value", l.MaxOrderValue,
			"max_lots_per_trade", l.MaxLotsPerTrade,
			"max_open_positions", l.MaxOpenPositions)
	}
	a.Bus.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelWarn,
		Message: "risk limits updated",
	})
}

// guardSweep periodically prunes the login-attempt table.
func (a *App) guardSweep(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Guard.Sweep()
		}
	}
}

// strategyConfigs converts the config's strategy slice into a name->config map.
func strategyConfigs(cfg *config.Config) map[string]config.StrategyCfg {
	out := make(map[string]config.StrategyCfg, len(cfg.Strategies))
	for _, s := range cfg.Strategies {
		out[s.Name] = s
	}
	return out
}
