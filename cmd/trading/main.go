// Command trading is the entry point for the algo options trading platform.
//
// It loads config, sets up the logger, storage, Kite client, broker (paper or
// live), risk manager, and the trading engine with the example short-straddle
// strategy, then runs until interrupted.
//
// Usage:
//
//	go run ./cmd/trading -config ./config.yaml
//
// Modes:
//
//	dryrun : no credentials; boots and idles (smoke test).
//	paper  : live market data, simulated orders. Default for development.
//	live   : real orders. Requires live_confirm: true AND typing "I UNDERSTAND".
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"log/slog"

	"kite-algo/internal/broker"
	"kite-algo/internal/config"
	"kite-algo/internal/engine"
	"kite-algo/internal/kite"
	"kite-algo/internal/logger"
	"kite-algo/internal/risk"
	"kite-algo/internal/storage/sqlite"
	shortstraddle "kite-algo/internal/strategy/examples/shortstraddle"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	initSecrets := flag.Bool("init-secrets", false,
		"write a template secrets file to secrets_path (default ~/.trading/secrets.yaml) and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	// Convenience: scaffold the secrets file at the configured (expanded) path.
	if *initSecrets {
		if err := writeSecretsTemplate(cfg.SecretsPath); err != nil {
			fmt.Fprintf(os.Stderr, "init-secrets failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("wrote secrets template to %s — edit it and add your Kite credentials\n", config.ExpandPath(cfg.SecretsPath))
		return
	}

	log := logger.New(os.Stderr, cfg.LogLevel(), cfg.Log.Format)
	logger.Init(log)

	log.Info("=== trading platform starting ===",
		"mode", cfg.Mode, "pid", os.Getpid(),
		"secrets_path", config.ExpandPath(cfg.SecretsPath))
	if cfg.HasCredentials() {
		log.Info("kite credentials loaded",
			"source", credentialSource(cfg))
	} else if cfg.Mode != config.ModeDryRun {
		log.Warn("no kite credentials found — set them in the secrets file, config.yaml, or env vars")
	}

	// Live double-gate #1 (config flag) is enforced by config.Load/validate.
	// Live double-gate #2: interactive confirmation.
	if cfg.Mode == config.ModeLive {
		if !confirmLive(log) {
			log.Warn("live confirmation declined; aborting")
			os.Exit(1)
		}
	}

	// Graceful shutdown on Ctrl-C / terminate.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Storage.
	if err := os.MkdirAll(filepath.Dir(cfg.Storage.SQLitePath), 0o755); err != nil {
		log.Error("create data dir failed", "err", err)
		os.Exit(1)
	}
	store, err := sqlite.New(ctx, cfg.Storage.SQLitePath, log)
	if err != nil {
		log.Error("storage init failed", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	// Risk manager.
	riskMgr := risk.NewManager(risk.Limits{
		MaxDailyLoss:     cfg.Risk.MaxDailyLoss,
		MaxOpenPositions: cfg.Risk.MaxOpenPositions,
		MaxOrderValue:    cfg.Risk.MaxOrderValue,
		MaxLotsPerTrade:  cfg.Risk.MaxLotsPerTrade,
	})
	log.Info("risk limits loaded",
		"max_daily_loss", cfg.Risk.MaxDailyLoss,
		"max_positions", cfg.Risk.MaxOpenPositions,
		"max_order_value", cfg.Risk.MaxOrderValue,
		"max_lots", cfg.Risk.MaxLotsPerTrade)

	// Broker + market data wiring depends on mode.
	var br broker.Broker
	var paperBroker *broker.PaperBroker // non-nil for paper/dryrun, nil for live
	var ticker *kite.Ticker
	var instruments *kite.Instruments

	switch cfg.Mode {
	case config.ModeLive:
		client, t, ins, err := connectKite(ctx, cfg, log)
		if err != nil {
			log.Error("kite setup failed", "err", err)
			os.Exit(1)
		}
		br = broker.NewLiveBroker(client, log)
		ticker, instruments = t, ins
	case config.ModePaper:
		client, t, ins, err := connectKite(ctx, cfg, log)
		if err != nil {
			log.Error("kite setup failed", "err", err)
			os.Exit(1)
		}
		_ = client
		paperBroker = broker.NewPaperBroker(nil, log)
		br = paperBroker
		ticker, instruments = t, ins
	case config.ModeDryRun:
		// No network. Use a paper broker so the engine has something to talk to;
		// the strategy will idle (no instruments/spots to act on).
		paperBroker = broker.NewPaperBroker(nil, log)
		br = paperBroker
	}

	engineOpts := []engine.Option{
		engine.WithInstruments(instruments),
		engine.WithTicker(ticker),
		engine.WithStrategyConfigs(strategyConfigs(cfg)),
	}
	if paperBroker != nil {
		engineOpts = append(engineOpts, engine.WithPaperBroker(paperBroker))
	}

	eng := engine.New(br, store, riskMgr, cfg.Recording.Ticks, log, engineOpts...)

	// Register strategies from config. Only the example is wired for v1.
	for _, sc := range cfg.Strategies {
		if !sc.Enabled {
			continue
		}
		switch sc.Name {
		case "short-straddle":
			eng.AddStrategy(shortstraddle.New(sc.Name, log))
		default:
			log.Warn("unknown strategy in config; skipped", "name", sc.Name)
		}
	}

	// Run until the context is canceled (signal or fatal error).
	runErr := make(chan error, 1)
	go func() {
		runErr <- eng.Start(ctx)
	}()

	select {
	case err := <-runErr:
		if err != nil && err != context.Canceled {
			log.Error("engine exited with error", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
	}

	log.Info("shutting down...")
	eng.Stop(context.Background())
	log.Info("=== trading platform stopped ===")
}

// connectKite authenticates with Kite, validates the token, and returns the
// client plus a fresh ticker and the NFO instrument master. Used by both paper
// and live modes (paper ignores the returned client for order placement).
func connectKite(ctx context.Context, cfg *config.Config, log *slog.Logger) (*kite.Client, *kite.Ticker, *kite.Instruments, error) {
	if cfg.Kite.AccessToken == "" {
		fmt.Printf("Login first, then set the access token:\n  %s\n",
			kite.New(cfg.Kite.APIKey, cfg.Kite.APISecret, "", cfg.Kite.BaseURL, nil).LoginURL())
		return nil, nil, nil, fmt.Errorf("kite access token missing; complete login and set kite.access_token (or KITE_ACCESS_TOKEN env)")
	}
	client := kite.New(cfg.Kite.APIKey, cfg.Kite.APISecret, cfg.Kite.AccessToken, cfg.Kite.BaseURL, log)

	// Validate the token so we fail fast on bad/expired credentials.
	if prof, err := client.GetProfile(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("kite auth check failed: %w", err)
	} else {
		log.Info("kite auth ok", "user", prof.UserID)
	}

	instruments, err := loadInstruments(ctx, client, log)
	if err != nil {
		return nil, nil, nil, err
	}
	ticker := kite.NewTicker(cfg.Kite.APIKey, cfg.Kite.AccessToken, cfg.Kite.TickerURL, instruments, log)
	return client, ticker, instruments, nil
}

// loadInstruments fetches the NFO option chain master (options live here).
func loadInstruments(ctx context.Context, client *kite.Client, log *slog.Logger) (*kite.Instruments, error) {
	m, err := client.FetchInstrumentsExchange(ctx, "NFO")
	if err != nil {
		return nil, fmt.Errorf("fetch instruments: %w", err)
	}
	client.LogInstrumentsSummary(m)
	return m, nil
}

// strategyConfigs converts the config's strategy slice into a name->config map.
func strategyConfigs(cfg *config.Config) map[string]config.StrategyCfg {
	out := make(map[string]config.StrategyCfg, len(cfg.Strategies))
	for _, s := range cfg.Strategies {
		out[s.Name] = s
	}
	return out
}

// confirmLive is the second safety gate: the operator must type the exact
// phrase. Prevents accidental real-money trading.
func confirmLive(log *slog.Logger) bool {
	fmt.Println("\n" + strings.Repeat("!", 70))
	fmt.Println("!!  LIVE TRADING MODE — REAL MONEY WILL BE AT RISK  !!")
	fmt.Println("!!  Options can lose your entire capital fast.     !!")
	fmt.Println(strings.Repeat("!", 70))
	fmt.Print("\nTo proceed, type exactly: I UNDERSTAND\n> ")
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	if scanner.Text() != "I UNDERSTAND" {
		log.Warn("confirmation text did not match", "got", scanner.Text())
		return false
	}
	log.Warn("LIVE trading confirmed by operator")
	return true
}

// writeSecretsTemplate creates the secrets file with 0600 permissions at the
// (expanded) configured path, plus its parent directory. Refuses to overwrite an
// existing file so real credentials are never clobbered.
func writeSecretsTemplate(secretsPath string) error {
	p := config.ExpandPath(secretsPath)
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("secrets file already exists at %s (not overwriting)", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}
	const tmpl = `# Kite credentials — kept OUTSIDE the repo for safety.
# Generated by 'tradebot -init-secrets'. Edit and fill in real values.
# This file overrides config.yaml; env vars (KITE_API_KEY, KITE_API_SECRET,
# KITE_ACCESS_TOKEN) override this file.
kite:
  api_key: ""
  api_secret: ""
  access_token: ""
`
	if err := os.WriteFile(p, []byte(tmpl), 0o600); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	return nil
}

// credentialSource reports where the API key came from, for the startup log.
// Env wins; then the secrets file; then config.yaml.
func credentialSource(cfg *config.Config) string {
	switch {
	case os.Getenv("KITE_API_KEY") != "":
		return "env"
	case secretsFileHasKey(cfg):
		return "secrets-file"
	default:
		return "config.yaml"
	}
}

// secretsFileHasKey reports whether the secrets file defines api_key.
func secretsFileHasKey(cfg *config.Config) bool {
	if cfg.SecretsPath == "" {
		return false
	}
	data, err := os.ReadFile(config.ExpandPath(cfg.SecretsPath))
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte("api_key:")) && !bytes.Contains(data, []byte(`api_key: ""`))
}
