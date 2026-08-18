// Command trading runs the algo options trading platform and its web UI.
//
// It is a long-running server. Unlike earlier versions it does NOT require a
// Zerodha access token to start: it boots, serves the web UI, and acquires a
// session when the operator completes the browser login. That inversion is what
// makes it deployable as a service — a token expires every morning, and a
// process that exits without one cannot serve the page that obtains it.
//
// Usage:
//
//	tradebot -config config.yaml       # run the server
//	tradebot -init-secrets             # scaffold the secrets file
//	tradebot -set-password             # set the web UI password
//
// Modes:
//
//	dryrun : no credentials needed; boots and idles.
//	paper  : live market data, simulated orders. The default for development.
//	live   : real orders — and even then only after confirming in the UI.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"kite-algo/internal/app"
	"kite-algo/internal/auth"
	"kite-algo/internal/config"
	"kite-algo/internal/history"
	"kite-algo/internal/logger"
	"kite-algo/internal/notify"
	"kite-algo/internal/storage/sqlite"
	"kite-algo/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to config file")
	initSecrets := flag.Bool("init-secrets", false,
		"write a template secrets file to secrets_path (default ~/.trading/secrets.yaml) and exit")
	setPassword := flag.Bool("set-password", false,
		"set the web UI password and exit")
	dev := flag.Bool("dev", false,
		"reload templates and static assets from disk on every request")
	captureDay := flag.String("capture", "",
		"capture option candles for one day (YYYY-MM-DD, or 'last' for the most "+
			"recent trading day) and exit; requires a persisted Zerodha session")
	notifyTest := flag.Bool("notify-test", false,
		"send a test message to the configured alert channel and exit")
	notifySend := flag.String("notify-send", "",
		"send this message to the configured alert channel and exit "+
			"(used by the backup job to report its own failures)")
	backupTo := flag.String("backup", "",
		"write a verified, compacted copy of the database to this path and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	if *initSecrets {
		if err := writeSecretsTemplate(cfg.SecretsPath); err != nil {
			return err
		}
		fmt.Printf("wrote secrets template to %s — edit it and add your Kite credentials\n",
			config.ExpandPath(cfg.SecretsPath))
		return nil
	}

	if *setPassword {
		return setWebPassword(cfg.SecretsPath)
	}

	log := logger.New(os.Stderr, cfg.LogLevel(), cfg.Log.Format)
	logger.Init(log)

	// Before the database, the engine and the network: this is a one-shot check
	// of one credential, and it has to be runnable on a box where the rest is
	// still broken.
	if *notifyTest {
		return sendTestAlert(cfg, log)
	}
	if *notifySend != "" {
		return sendAlert(cfg, log, *notifySend)
	}

	// Before the engine, the web server and the network: a backup must be
	// runnable on a box where the rest is broken, which is exactly when someone
	// reaches for one.
	if *backupTo != "" {
		return runBackup(cfg, log, *backupTo)
	}

	log.Info("=== trading platform starting ===",
		"mode", cfg.Mode, "pid", os.Getpid(),
		"secrets_path", config.ExpandPath(cfg.SecretsPath))

	if cfg.FileMissing() {
		log.Info("no config file found; running on defaults and TRADING_* environment variables",
			"looked_for", *configPath)
	}
	// A zero limit means "no limit". That is a legitimate choice, but arriving at
	// it by deleting a config file is not — so say it out loud every start.
	if off := cfg.DisabledRiskLimits(); len(off) > 0 {
		log.Warn("RISK LIMITS DISABLED — these checks will not block any order",
			"disabled", off,
			"fix", "set them in config.yaml, via TRADING_MAX_* env vars, or on the /risk page")
	}
	if cfg.HasCredentials() {
		log.Info("kite credentials loaded", "source", credentialSource(cfg))
	} else if cfg.Mode != config.ModeDryRun {
		log.Warn("no kite api credentials found — set them in the secrets file or via KITE_API_KEY/KITE_API_SECRET")
	}
	if cfg.Mode == config.ModeLive {
		log.Warn("configured for LIVE trading — orders stay simulated until you confirm in the web UI")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := os.MkdirAll(filepath.Dir(cfg.Storage.SQLitePath), 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	store, err := sqlite.New(ctx, cfg.Storage.SQLitePath, log)
	if err != nil {
		return fmt.Errorf("storage init: %w", err)
	}
	defer store.Close()

	application, err := app.New(ctx, cfg, store, log)
	if err != nil {
		return fmt.Errorf("app init: %w", err)
	}

	// One-shot capture: no web server, no engine, no strategies. Runs the
	// download and exits, so it can be driven from cron or run by hand after a
	// missed day — neither of which should have to authenticate to a browser UI.
	if *captureDay != "" {
		return runCapture(ctx, application, cfg, *captureDay, log)
	}

	srv, err := web.New(application, log, web.Options{Dev: *dev})
	if err != nil {
		return fmt.Errorf("web init: %w", err)
	}

	// Engine and HTTP server run as peers; whichever fails first stops the other.
	errCh := make(chan error, 2)
	go func() {
		err := application.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()
	go func() { errCh <- srv.Start(ctx) }()

	var runErr error
	select {
	case runErr = <-errCh:
		cancel() // bring the other half down too
	case <-ctx.Done():
	}

	log.Info("shutting down...")

	// Stop accepting HTTP first so no new orders can be submitted while
	// strategies are being unwound.
	shutdownCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	if err := application.Shutdown(shutdownCtx); err != nil {
		log.Warn("app shutdown", "err", err)
	}
	log.Info("=== trading platform stopped ===")
	return runErr
}

// runCapture performs a single option-candle capture and reports what it stored.
//
// A non-trading day is refused rather than quietly skipped: the caller asked for
// a specific date, and "completed successfully, stored nothing" is precisely the
// outcome that lets an operator believe a day was captured when it was not.
func runCapture(ctx context.Context, application *app.App, cfg *config.Config, raw string, log *slog.Logger) error {
	cal := history.NSE()
	cal.SetHolidays(cfg.Capture.Holidays)

	var day time.Time
	if strings.EqualFold(strings.TrimSpace(raw), "last") {
		d, ok := cal.MostRecentTradingDay(time.Now())
		if !ok {
			return fmt.Errorf("no trading day found in the last fortnight")
		}
		day = d
	} else {
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(raw), history.IST)
		if err != nil {
			return fmt.Errorf("-capture wants YYYY-MM-DD or 'last', got %q", raw)
		}
		if !cal.IsTradingDay(d) {
			return fmt.Errorf("%s is a %s — the exchange was shut, nothing to capture",
				d.Format("2006-01-02"), d.Weekday())
		}
		day = d
	}

	log.Info("capturing option candles", "day", day.Format("2006-01-02"),
		"interval", cfg.Capture.Interval, "strikes", cfg.Capture.Strikes,
		"expiries", cfg.Capture.Expiries, "lookback_days", cfg.Capture.LookbackDays)

	rep, err := application.CaptureOnce(ctx, day)
	if err != nil {
		return fmt.Errorf("capture %s: %w", day.Format("2006-01-02"), err)
	}
	if rep.Skipped != "" {
		return fmt.Errorf("capture %s skipped: %s", day.Format("2006-01-02"), rep.Skipped)
	}

	for _, u := range rep.Underlying {
		if u.Err != "" {
			log.Warn("chain incomplete", "underlying", u.Underlying, "err", u.Err)
			continue
		}
		log.Info("chain captured", "underlying", u.Underlying, "spot", u.Spot,
			"expiries", len(u.Expiries), "contracts", u.Contracts,
			"candles", u.Candles, "failures", u.Failures)
	}
	log.Info("capture complete", "day", day.Format("2006-01-02"),
		"contracts", rep.Contracts, "candles", rep.Candles,
		"failures", rep.Failures, "took", rep.Duration.Round(time.Second))

	if rep.Contracts == 0 {
		return fmt.Errorf("capture stored nothing for %s", day.Format("2006-01-02"))
	}
	return nil
}

// setWebPassword prompts for a password and writes its hash to the secrets file.
func setWebPassword(secretsPath string) error {
	p := config.ExpandPath(secretsPath)
	if p == "" {
		return errors.New("secrets_path is empty; set it in your config first")
	}

	fmt.Println("Set the web UI password for this trading server.")

	pw, err := readSecret("Password: ")
	if err != nil {
		return err
	}
	if len(pw) < 12 {
		return errors.New("password must be at least 12 characters; this server can place real orders")
	}
	confirm, err := readSecret("Confirm:  ")
	if err != nil {
		return err
	}
	if confirm != pw {
		return errors.New("passwords did not match")
	}

	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := upsertSecretsWebHash(p, hash); err != nil {
		return err
	}
	fmt.Printf("\npassword set in %s\n", p)
	return nil
}

// readSecret prompts for a secret without echoing it.
//
// Echoing a credential to the terminal leaves it in scrollback, in screen
// shares, and in any transcript of the session — for a password that unlocks
// order placement, that is not acceptable. When stdin is not a terminal (a pipe,
// or a test), it falls back to a plain read, which is the only thing possible
// there and is fine because nothing is being displayed.
func readSecret(prompt string) (string, error) {
	fmt.Print(prompt)

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Println() // ReadPassword swallows the newline the user typed
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}

	line, err := stdinReader().ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no password entered")
	}
	return strings.TrimSpace(line), nil
}

// stdin is a single shared buffered reader.
//
// It must be shared: a bufio reader reads ahead, so constructing a fresh one per
// prompt lets the first read swallow the second line and the confirmation then
// sees EOF. That only shows up on piped input, which is exactly how the tests
// and any scripted setup drive this.
var stdin *bufio.Reader

func stdinReader() *bufio.Reader {
	if stdin == nil {
		stdin = bufio.NewReader(os.Stdin)
	}
	return stdin
}

// upsertSecretsWebHash writes web.password_hash into the secrets file, creating
// it if needed and replacing any existing value.
//
// The file is rewritten line-wise rather than through a YAML round-trip because
// it is hand-maintained and comment-rich; marshalling it would silently discard
// the operator's own notes.
func upsertSecretsWebHash(path, hash string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read secrets: %w", err)
	}

	line := "  password_hash: \"" + hash + "\""
	lines := strings.Split(strings.TrimRight(string(existing), "\n"), "\n")

	var (
		out        []string
		inWeb      bool
		replaced   bool
		sawWebRoot bool
	)
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		switch {
		case trimmed == "web:":
			inWeb, sawWebRoot = true, true
			out = append(out, l)
			continue
		case inWeb && strings.HasPrefix(trimmed, "password_hash:"):
			out = append(out, line)
			replaced = true
			continue
		case l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "#") && trimmed != "web:":
			inWeb = false
		}
		out = append(out, l)
	}

	if !replaced {
		if !sawWebRoot {
			out = append(out, "", "# Web UI operator password (set by 'tradebot -set-password').", "web:")
		}
		out = append(out, line)
	}

	body := strings.TrimLeft(strings.Join(out, "\n"), "\n") + "\n"
	// 0600: this file holds the credential to a system that can spend money.
	return os.WriteFile(path, []byte(body), 0o600)
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
#
# access_token is optional: the web UI obtains one through the Zerodha browser
# login and stores it in the database, refreshing it each trading day.
kite:
  api_key: ""
  api_secret: ""
  access_token: ""

# Web UI operator password. Set it with 'tradebot -set-password'.
web:
  password_hash: ""

# Telegram bot token for operator alerts, from @BotFather. Optional.
# The rest of the alert settings (enabled, chat_id, repeat_every) live in
# config.yaml; only the token is a credential and belongs here.
# Verify delivery with 'tradebot -notify-test'.
notify:
  telegram:
    bot_token: ""
`
	if err := os.WriteFile(p, []byte(tmpl), 0o600); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	return nil
}

// sendTestAlert proves the alert channel works, which is the one thing about it
// an operator cannot verify by waiting.
//
// The alert this exists for fires at most once a day and only when something has
// already gone wrong, so a wrong bot token would otherwise be discovered on the
// morning it was needed, by its silence. Failures here name what to fix: a wrong
// chat_id and a bot the user never pressed Start on are both an HTTP 400 from
// Telegram, and they have different remedies.
func sendTestAlert(cfg *config.Config, log *slog.Logger) error {
	t := cfg.Notify.Telegram
	if !t.Enabled {
		log.Warn("notify.telegram.enabled is false — sending anyway, since you asked")
	}
	tg := notify.NewTelegram(t.BotToken, t.ChatID, log)
	if !tg.Configured() {
		return fmt.Errorf("telegram is not configured: needs notify.telegram.bot_token " +
			"in the secrets file and notify.telegram.chat_id in config.yaml")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	msg := "kite-algo alert test — if you can read this, the daily login alert will reach you."
	if url := strings.TrimRight(strings.TrimSpace(cfg.Web.PublicURL), "/"); url != "" {
		msg += "\n\nLog in: " + url + "/connect"
	} else {
		msg += "\n\nNote: web.public_url is unset, so real alerts will carry no login link."
	}

	if err := tg.Send(ctx, msg); err != nil {
		return fmt.Errorf("%w\n\nCommon causes:\n"+
			"  - chat_id is wrong: it is a NUMBER, not the @name — message @userinfobot to get yours\n"+
			"  - the bot has never been started: open it in Telegram and press Start\n"+
			"  - bot_token is wrong or revoked: re-issue it with @BotFather", err)
	}
	log.Info("test alert delivered", "chat_id", t.ChatID)
	fmt.Println("sent — check Telegram")
	return nil
}

// sendAlert pushes one message to the configured channel and exits.
//
// This exists so the backup job can report its own failure through the same
// channel as everything else. The alternative was parsing YAML in shell to find a
// bot token, which is both fragile and a second place the credential is read.
func sendAlert(cfg *config.Config, log *slog.Logger, msg string) error {
	t := cfg.Notify.Telegram
	tg := notify.NewTelegram(t.BotToken, t.ChatID, log)
	if !tg.Configured() {
		// Not an error. A backup job on a box with no alert channel configured
		// must still back up, and must not fail because it could not narrate.
		log.Warn("no alert channel configured; message not sent", "message", msg)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := tg.Send(ctx, msg); err != nil {
		return err
	}
	log.Info("alert sent", "chars", len(msg))
	return nil
}

// runBackup writes a verified copy of the database and reports what it contains.
//
// The counts in the summary are the point of it. "Backup complete" says a file was
// written; "412 snapshot days, 3.1M candles" says how much of the irreplaceable
// data is actually in there, which is the only version of the claim worth trusting
// — an empty database backs up perfectly and passes every structural check.
func runBackup(cfg *config.Config, log *slog.Logger, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	res, err := sqlite.BackupInto(ctx, cfg.Storage.SQLitePath, dest)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	log.Info("backup complete",
		"path", res.Path, "bytes", res.Bytes,
		"snapshot_days", res.SnapshotDays, "candles", res.Candles,
		"took", res.Took.Round(time.Millisecond))
	fmt.Println(res.Summary())
	return nil
}

// credentialSource reports where the API key came from, for the startup log.
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
