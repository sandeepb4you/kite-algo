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
	"kite-algo/internal/charges"
	"kite-algo/internal/config"
	"kite-algo/internal/engine"
	"kite-algo/internal/events"
	"kite-algo/internal/history"
	"kite-algo/internal/notify"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage"
	"kite-algo/internal/strategy"

	// Links the built-in strategies into the binary so they self-register.
	_ "kite-algo/internal/strategy/catalog"
)

// App wires and supervises the platform.
type App struct {
	Cfg    *config.Config
	Log    *slog.Logger
	Store  storage.Store
	Bus    *events.Bus
	Engine *engine.Engine
	Risk   *risk.Manager
	// PaperRisk governs the simulated book, which runs alongside the real one.
	// This is the ONLY one the /risk page may change.
	PaperRisk *risk.Manager
	// LiveRisk is the real book's derived, UI-locked policy: a daily loss cap
	// expressed as a percentage of the day's opening balance, plus the
	// once-per-day lockout that follows a breach.
	LiveRisk *LiveRisk
	Kite     *KiteSession
	Sessions *auth.Sessions
	Guard    *auth.LoginGuard

	// paper is the simulated broker. It is ALWAYS the broker the engine starts
	// with, including when the config says live: real order routing is only
	// installed after an explicit runtime confirmation. See LiveArmed.
	paper *broker.PaperBroker

	bootAt time.Time

	// margins caches the account balance fetched from Zerodha.
	margins marginCache

	// capture is the daily option-candle job, nil when disabled in config.
	capture *history.CaptureScheduler

	// alerts is the outbound channel for operator notifications, or nil when
	// none is configured. Held rather than passed around because two unrelated
	// things now push to it — the missing-session watcher and the capture
	// summary — and they must not each build their own.
	alerts alerter

	mu       sync.RWMutex
	liveMode bool // true once the live broker has been swapped in
	// riskOverridden records that the active limits came from a saved override
	// rather than config.yaml, so the UI can say which is in force.
	riskOverridden bool
}

// Status is everything the UI header needs in one struct.
type Status struct {
	Mode       config.Mode `json:"mode"`
	LiveArmed  bool        `json:"live_armed"`  // configured for live, awaiting confirmation
	LiveActive bool        `json:"live_active"` // real orders are being routed
	Kite       SessionInfo `json:"kite"`
	Streaming  bool        `json:"streaming"`
	BootAt     time.Time   `json:"boot_at"`
	Uptime     string      `json:"uptime"`
	DayPnL     float64     `json:"day_pnl"`
	// DayCharges is the session's ESTIMATED transaction cost, modelled from the
	// published rate card. Zerodha exposes no real-time charges API; the
	// authoritative figures arrive on the contract note after the close.
	DayCharges charges.Breakdown `json:"day_charges"`
	// NetPnL is DayPnL less estimated charges — the figure that decides whether
	// a session actually made money.
	NetPnL float64 `json:"net_pnl"`
	// RealPnL and PaperPnL split DayPnL by book. Manual orders can be routed to
	// the exchange while strategies stay simulated, and a blended figure would
	// be neither real money nor a simulation result.
	RealPnL  float64 `json:"real_pnl"`
	PaperPnL float64 `json:"paper_pnl"`
	// Route is how orders are currently routed: "all-paper" or "manual-live".
	Route string `json:"route"`
	// PaperRiskLimits are the simulated book's limits.
	PaperRiskLimits risk.Limits      `json:"paper_risk_limits"`
	Margins         Margins          `json:"margins"`
	RiskLimits      risk.Limits      `json:"risk_limits"`
	Halt            engine.HaltState `json:"halt"`
	RedirectURL     string           `json:"redirect_url"`
}

// New builds the platform without touching the network.
//
// Nothing here can fail because Zerodha is unreachable or a token is missing —
// that is the entire point. The Kite session is restored opportunistically and
// its absence is a normal, recoverable state.
func New(ctx context.Context, cfg *config.Config, store storage.Store, log *slog.Logger) (*App, error) {
	bus := events.NewBus(log)

	// config.yaml supplies the defaults; a saved override wins. Deleting the
	// override restores the defaults, so there is always a way back to a known
	// state without remembering the original numbers.
	logf := func(format string, args ...any) {
		if log != nil {
			log.Warn(fmt.Sprintf(format, args...))
		}
	}
	// A saved override applies to the SIMULATED book only; the real book's
	// limits come from config and the derived percentage, never from storage.
	savedPaper, overridden := loadRiskLimits(ctx, store, cfg, logf)
	riskMgr := risk.NewManager(configuredLiveRiskLimits(cfg))
	limits := savedPaper
	// The simulated book gets its own manager, so a strategy exhausting its
	// daily-loss allowance blocks strategies and leaves real manual trading
	// untouched.
	paperLimits := configuredPaperRiskLimits(cfg)
	if overridden {
		paperLimits = savedPaper
	}
	paperRisk := risk.NewManager(paperLimits)
	liveRisk := NewLiveRisk(cfg.Risk.Live.MaxLossPct)
	liveRisk.Restore(ctx, store)

	if log != nil {
		log.Info("risk limits loaded",
			"source", map[bool]string{true: "saved override", false: "config.yaml"}[overridden],
			"max_daily_loss", limits.MaxDailyLoss,
			"max_order_value", limits.MaxOrderValue,
			"max_lots_per_trade", limits.MaxLotsPerTrade,
			"max_open_positions", limits.MaxOpenPositions)
	}

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
		Cfg:            cfg,
		Log:            log,
		Store:          store,
		Bus:            bus,
		Engine:         eng,
		Risk:           riskMgr,
		Sessions:       auth.NewSessions(store, cfg.Web.SessionTTL, secure, log),
		Guard:          auth.NewLoginGuard(),
		paper:          paper,
		bootAt:         time.Now(),
		riskOverridden: overridden,
	}
	eng.SetPaperRisk(paperRisk)
	a.PaperRisk = paperRisk
	a.LiveRisk = liveRisk

	// The real book's manager carries the config-derived caps; its daily loss
	// is refreshed from the opening balance as the margin loop learns it.
	// Live entries are gated on the derived policy in addition to the limits.
	// Exits are exempt — every rule here caps risk being taken on, and applied
	// to a flatten they would trap the operator in the position the rule exists
	// to protect them from.
	eng.SetLiveGate(func() (bool, string) {
		return liveRisk.Allow(time.Now())
	})
	a.Kite = NewKiteSession(cfg, store, eng, bus, log)

	// Restore strategies that were running when the process last stopped. Hung
	// off market data rather than boot because resuming an open position needs
	// the instrument master and a live ticker — see strategies.go for why this
	// is the one thing that starts without a click.
	a.Kite.OnMarketData(func(ctx context.Context) {
		if refused := a.RestoreStrategies(ctx); len(refused) > 0 && log != nil {
			for _, g := range refused {
				log.Error("strategy NOT restored; its positions are unmanaged",
					"strategy", g.StrategyID, "positions", len(g.Positions), "reason", g.Reason)
			}
		}
	})

	a.restorePaperBook(ctx)

	// The persisted token is deliberately NOT restored here. See Run.
	return a, nil
}

// Run starts the engine and background supervisors, blocking until ctx ends.
func (a *App) Run(ctx context.Context) error {
	go a.Kite.Supervise(ctx)
	go a.Sessions.GC(ctx, time.Hour)
	go a.guardSweep(ctx)
	go a.marginLoop(ctx)
	go a.WatchRealPnL(ctx)
	// Outbound alerting, before the jobs that push to it.
	if tg := notify.NewTelegram(
		a.Cfg.Notify.Telegram.BotToken, a.Cfg.Notify.Telegram.ChatID, a.Log,
	); a.Cfg.Notify.Telegram.Enabled && tg.Configured() {
		a.alerts = tg
	}

	a.startCapture(ctx)
	a.startExpirySweeper(ctx)
	// Started before the token restore below, so a boot that comes up with no
	// usable session still produces the alert about it.
	a.startSessionWatch(ctx, a.alerts)

	// Restore the persisted Zerodha token only once the engine is running.
	//
	// This used to happen in New, and the ordering was wrong in two ways that
	// compounded into a doubled position on 2026-08-17.
	//
	// Restoring a session brings market data up, which triggers strategy
	// restore. Doing that before Engine.Start meant:
	//
	//   1. The position cache was empty — syncLoop, which fills it, is started
	//      by Engine.Start. So a resuming strategy was handed no positions,
	//      concluded it was flat, and entered again on top of what it held.
	//   2. Engine.Start then re-ran Init on the instance it found already
	//      running, resetting the very state a resume would have rebuilt.
	//
	// In a goroutine because Engine.Start blocks until ctx ends. No sleep to
	// "let the engine settle": RestoreStrategies fetches positions itself rather
	// than trusting a cache that may not be populated, so this is correct
	// whichever order the two actually complete in.
	go func() {
		if err := a.Kite.Restore(ctx); err != nil && a.Log != nil {
			a.Log.Warn("restore kite session failed", "err", err)
		}
	}()

	return a.Engine.Start(ctx)
}

// Strategies are NEVER started automatically.
//
// An earlier version auto-started anything marked enabled: true in config.yaml
// once market data came up. That is the wrong default for something that places
// orders — a restart, a redeploy, or a crash-loop would silently begin trading
// with nobody watching, and the config flag is easy to leave set from a previous
// session.
//
// config.yaml's strategy blocks are now purely a source of DEFAULT PARAMETERS
// for the /strategies form. Starting one is always a deliberate click.

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
	chargesToday := a.Engine.DayCharges()
	return Status{
		Mode:       a.Cfg.Mode,
		LiveArmed:  a.Cfg.Mode == config.ModeLive && !live,
		LiveActive: live,
		Kite:       info,
		Streaming:  info.Streaming,
		BootAt:     a.bootAt,
		Uptime:     time.Since(a.bootAt).Round(time.Second).String(),
		DayPnL:     a.Engine.DayPnL(),
		DayCharges: chargesToday,
		NetPnL:     a.Engine.DayPnL() - chargesToday.Total,
		RealPnL:    a.Engine.BookPnL(broker.BookReal),
		PaperPnL:   a.Engine.BookPnL(broker.BookPaper),
		Route:      string(a.Engine.RouteMode()),
		Margins:    a.margins.get(),
		RiskLimits: a.Risk.Limits(),
		PaperRiskLimits: func() risk.Limits {
			if a.PaperRisk != nil {
				return a.PaperRisk.Limits()
			}
			return a.Risk.Limits()
		}(),
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

	// Install live routing for MANUAL orders only. The engine keeps every
	// strategy on the paper broker; see engine.bookFor. SwapBroker is
	// deliberately not used — that would take strategies live too.
	a.Engine.SetLiveBroker(live)

	if a.Log != nil {
		a.Log.Warn("LIVE TRADING CONFIRMED for MANUAL orders — " +
			"hand-typed orders now reach the exchange; strategies remain simulated")
	}
	a.Bus.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelWarn,
		Message: "LIVE manual trading confirmed — hand-typed orders are real; strategies stay simulated",
		Fields:  map[string]any{"live_active": true, "route": "manual-live"},
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

	a.Engine.SetLiveBroker(nil)
	if a.Log != nil {
		a.Log.Warn("live trading disarmed; new manual orders are simulated again")
	}
	a.Bus.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelWarn,
		Message: "live trading disarmed — new orders are simulated",
		Fields:  map[string]any{"live_active": false},
	})
}

// SetRiskLimits updates the runtime risk limits.
//
// Limits are runtime state, not config: when a position is moving against you,
// tightening the cap should not require editing a YAML file and restarting the
// process that is holding the position.
//
// It applies to the SIMULATED book only. The real book's limits are derived
// from the account's opening balance and are deliberately unreachable from the
// UI — a limit you can loosen from a browser at the moment it starts hurting is
// not a limit. Changing those means editing config.yaml and restarting.
func (a *App) SetRiskLimits(l risk.Limits) {
	old := a.PaperRisk.Limits()
	a.PaperRisk.SetLimits(l)

	if a.Log != nil {
		a.Log.Warn("PAPER risk limits changed",
			"max_daily_loss", l.MaxDailyLoss, "was", old.MaxDailyLoss,
			"max_order_value", l.MaxOrderValue,
			"max_lots_per_trade", l.MaxLotsPerTrade,
			"max_open_positions", l.MaxOpenPositions)
	}
	a.Bus.Publish(events.Event{
		Kind:    events.KindStatus,
		Level:   events.LevelWarn,
		Message: "paper risk limits updated (live limits are config-locked)",
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
