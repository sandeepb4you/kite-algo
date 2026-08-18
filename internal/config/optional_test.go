package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestMissingConfigFileUsesDefaults is the behaviour that lets the platform run
// from environment variables alone, with no config file on disk — the usual
// shape for a container or a systemd unit.
func TestMissingConfigFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("a missing config file should not be an error: %v", err)
	}
	if !cfg.FileMissing() {
		t.Error("FileMissing() = false; startup would not report that defaults are in use")
	}

	// Defaults must be safe and complete enough to boot.
	if cfg.Mode != ModeDryRun {
		t.Errorf("default mode = %q, want dryrun — the safe default", cfg.Mode)
	}
	if cfg.Web.Addr != "127.0.0.1:8080" {
		t.Errorf("default web addr = %q, want loopback", cfg.Web.Addr)
	}
	if cfg.Storage.SQLitePath == "" {
		t.Error("no default database path")
	}
	if cfg.SecretsPath == "" {
		t.Error("no default secrets path")
	}
}

// TestMalformedConfigIsStillAnError distinguishes "no file" from "a file the
// operator wrote that is being ignored". The second must never pass silently.
func TestMalformedConfigIsStillAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	writeFile(t, path, "mode: paper\n  this: is not: valid yaml\n")

	if _, err := Load(path); err == nil {
		t.Fatal("a malformed config file was accepted; the operator's settings would be silently ignored")
	}
}

// TestDisabledRiskLimitsAreReported covers the trap in running without a config
// file: every risk cap is then zero, which means "no limit", and nothing else
// would tell the operator.
//
// All four must be reported. MaxOpenPositions and MaxLotsPerTrade used to be
// defaulted to 5 and 1 by applyDefaults, which made them invisible here — and
// worse, made an explicit `max_lots_per_trade: 0` come out as a one-lot cap.
func TestDisabledRiskLimitsAreReported(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	off := cfg.DisabledRiskLimits()
	for _, name := range []string{
		"max_daily_loss", "max_order_value", "max_open_positions", "max_lots_per_trade",
	} {
		if !slices.Contains(off, name) {
			t.Errorf("disabled limits = %v; %s is 0 (unlimited) and must be reported", off, name)
		}
	}
}

// A zero sizing cap must survive Load, or the operator cannot switch one off.
//
// Writing 0 to disable max_lots_per_trade produced a cap of ONE LOT: applyDefaults
// rewrote it before anything read it, so every order above a single lot was
// refused on a limit that had been explicitly disabled, and /risk showed 5 and 1
// as though they were the configured values.
func TestZeroSizingCapsSurviveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "off.yaml")
	writeFile(t, path, `mode: paper
risk:
  max_daily_loss: 5000
  max_open_positions: 0
  max_order_value: 0
  max_lots_per_trade: 0
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Risk.MaxOpenPositions != 0 {
		t.Errorf("max_open_positions = %d, want 0 — a cap the operator switched off was re-imposed",
			cfg.Risk.MaxOpenPositions)
	}
	if cfg.Risk.MaxLotsPerTrade != 0 {
		t.Errorf("max_lots_per_trade = %d, want 0 — every order above %d lot(s) would be refused",
			cfg.Risk.MaxLotsPerTrade, cfg.Risk.MaxLotsPerTrade)
	}
	// The cap that is deliberately still on must be untouched.
	if cfg.Risk.MaxDailyLoss != 5000 {
		t.Errorf("max_daily_loss = %v, want 5000", cfg.Risk.MaxDailyLoss)
	}
}

func TestEnvOverridesNonSecretSettings(t *testing.T) {
	t.Setenv("TRADING_MODE", "paper")
	t.Setenv("TRADING_WEB_ADDR", "0.0.0.0:9000")
	t.Setenv("TRADING_PUBLIC_URL", "https://trade.example.com/")
	t.Setenv("TRADING_SQLITE_PATH", "/var/lib/kite/x.db")
	t.Setenv("TRADING_MAX_DAILY_LOSS", "7500")
	t.Setenv("TRADING_MAX_LOTS_PER_TRADE", "3")
	t.Setenv("TRADING_RECORD_TICKS", "true")
	t.Setenv("KITE_API_KEY", "envkey")
	// A password is required to bind a non-loopback address.
	t.Setenv("TRADING_SECRETS_PATH", writeSecrets(t))

	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Mode != ModePaper {
		t.Errorf("mode = %q, want paper", cfg.Mode)
	}
	if cfg.Web.Addr != "0.0.0.0:9000" {
		t.Errorf("web addr = %q", cfg.Web.Addr)
	}
	if cfg.Web.PublicURL != "https://trade.example.com" {
		t.Errorf("public url = %q, want the trailing slash trimmed", cfg.Web.PublicURL)
	}
	if cfg.Storage.SQLitePath != "/var/lib/kite/x.db" {
		t.Errorf("sqlite path = %q", cfg.Storage.SQLitePath)
	}
	if cfg.Risk.MaxDailyLoss != 7500 {
		t.Errorf("max daily loss = %v, want 7500", cfg.Risk.MaxDailyLoss)
	}
	if cfg.Risk.MaxLotsPerTrade != 3 {
		t.Errorf("max lots = %d, want 3", cfg.Risk.MaxLotsPerTrade)
	}
	if !cfg.Recording.Ticks {
		t.Error("tick recording was not enabled from the environment")
	}
}

// TestLiveCannotBeArmedFromTheEnvironment is a safety property. Enabling live
// trading must require editing a file on the machine; a stray exported variable
// in a shell profile or a CI job must not be able to do it.
func TestLiveCannotBeArmedFromTheEnvironment(t *testing.T) {
	t.Setenv("TRADING_MODE", "live")
	t.Setenv("KITE_API_KEY", "k")
	t.Setenv("KITE_API_SECRET", "s")

	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("live mode was armed from the environment alone, with no live_confirm in a file")
	}
}

// TestBadNumericEnvIsIgnoredNotFatal keeps a typo in a deployment variable from
// preventing the platform starting; the configured or default value stands.
func TestBadNumericEnvIsIgnoredNotFatal(t *testing.T) {
	t.Setenv("TRADING_MAX_DAILY_LOSS", "five thousand")

	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("a malformed numeric env var should not stop startup: %v", err)
	}
	if cfg.Risk.MaxDailyLoss != 0 {
		t.Errorf("max daily loss = %v; an unparseable value should leave it untouched", cfg.Risk.MaxDailyLoss)
	}
}

// --- helpers --------------------------------------------------------------

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeSecrets creates a secrets file carrying a web password hash, so tests
// that bind a non-loopback address pass validation.
func writeSecrets(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.yaml")
	writeFile(t, path, "web:\n  password_hash: \"pbkdf2-sha256$600000$c2FsdA$a2V5\"\n")
	return path
}
