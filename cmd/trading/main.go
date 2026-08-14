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
	"kite-algo/internal/logger"
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
`
	if err := os.WriteFile(p, []byte(tmpl), 0o600); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
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
