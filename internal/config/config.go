// Package config loads and validates the trading platform configuration.
//
// Config is loaded from a YAML file (default ./config.yaml). Secret values
// (Kite API key/secret/access token) may be overridden via environment
// variables, which is preferred so credentials never sit in a checked-in file:
//
//	KITE_API_KEY, KITE_API_SECRET, KITE_ACCESS_TOKEN
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode controls how the platform executes orders.
type Mode string

const (
	// ModeDryRun: no Kite credentials, no live data, no orders. Boots and logs.
	ModeDryRun Mode = "dryrun"
	// ModePaper: streams live market data but routes orders to a simulated broker.
	ModePaper Mode = "paper"
	// ModeLive: real orders via Kite. Requires live_confirm + manual startup ack.
	ModeLive Mode = "live"
)

// Config is the top-level configuration object.
type Config struct {
	Mode        Mode       `yaml:"mode"`
	Log         LogConfig  `yaml:"log"`
	Kite        KiteConfig `yaml:"kite"`
	LiveConfirm bool       `yaml:"live_confirm"`
	// SecretsPath is the location of an optional YAML secrets file holding Kite
	// credentials OUTSIDE the repo. Defaults to ~/.trading/secrets.yaml. Values
	// in the secrets file override config.yaml; env vars override both. Set to
	// "" to disable.
	SecretsPath string        `yaml:"secrets_path"`
	Recording   RecordConfig  `yaml:"recording"`
	Storage     StorageConfig `yaml:"storage"`
	Risk        RiskConfig    `yaml:"risk"`
	Web         WebConfig     `yaml:"web"`
	Strategies  []StrategyCfg `yaml:"strategies"`
}

// WebConfig controls the built-in web UI.
type WebConfig struct {
	// Addr is the listen address. Defaults to 127.0.0.1:8080 — loopback only,
	// because the app must sit behind a TLS-terminating reverse proxy rather
	// than face the internet itself. Binding to a routable address without a
	// password configured is refused at startup.
	Addr string `yaml:"addr"`

	// PublicURL is the externally-visible base URL (e.g. https://trade.example.com),
	// used to build the Kite redirect URL and to validate WebSocket origins.
	// Must match the Redirect URL registered in the Kite developer console
	// exactly, including scheme and path.
	PublicURL string `yaml:"public_url"`

	// PasswordHash authenticates the single operator. Generate it with
	// `tradebot -set-password`; it belongs in the secrets file, not here.
	PasswordHash string `yaml:"password_hash"`

	// SessionTTL is how long a browser login lasts. Default 720h (30 days).
	SessionTTL time.Duration `yaml:"session_ttl"`

	// TrustProxy makes the app read the client IP from X-Forwarded-For. Only
	// enable when a reverse proxy you control sets that header, otherwise
	// anyone can spoof it and defeat the login lockout.
	TrustProxy bool `yaml:"trust_proxy"`

	// TickIntervalMS is how often coalesced market-data frames are flushed to
	// browsers. Default 200ms (~5 updates/sec) — fast enough to read, far below
	// the raw tick rate of an option chain.
	TickIntervalMS int `yaml:"tick_interval_ms"`
}

// LogConfig controls logging.
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
}

// KiteConfig holds Zerodha Kite API credentials and endpoints.
type KiteConfig struct {
	APIKey      string `yaml:"api_key"`
	APISecret   string `yaml:"api_secret"`
	AccessToken string `yaml:"access_token"`
	BaseURL     string `yaml:"base_url"`
	TickerURL   string `yaml:"ticker_url"`
	// APIKeyHeader is the HTTP header name Kite expects for the API key.
	APIKeyHeader string `yaml:"-"`
}

// RecordConfig controls market-data recording.
type RecordConfig struct {
	Ticks bool `yaml:"ticks"` // record every tick (large) — off by default
}

// StorageConfig controls persistence.
type StorageConfig struct {
	SQLitePath string `yaml:"sqlite_path"`
}

// RiskConfig defines the pre-trade risk limits enforced by the risk manager.
type RiskConfig struct {
	MaxDailyLoss     float64 `yaml:"max_daily_loss"`     // rupees; halt trading if exceeded
	MaxOpenPositions int     `yaml:"max_open_positions"` // concurrent open positions
	MaxOrderValue    float64 `yaml:"max_order_value"`    // rupees per single order
	MaxLotsPerTrade  int     `yaml:"max_lots_per_trade"` // lots allowed in one order
}

// StrategyCfg is the declarative config for one strategy instance.
type StrategyCfg struct {
	Name    string         `yaml:"name"`
	Enabled bool           `yaml:"enabled"`
	Params  map[string]any `yaml:"params"`
}

// Load reads config from the given path, then merges an optional secrets file,
// then applies environment-variable overrides. Returns an error if the config is
// structurally invalid or fails safety checks.
//
// Credential precedence (most specific wins):
//
//  1. environment variables (KITE_API_KEY / KITE_API_SECRET / KITE_ACCESS_TOKEN)
//  2. secrets file (secrets_path, outside the repo — ~/.trading/secrets.yaml)
//  3. this config file (config.yaml)
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	c.applyDefaults()
	if err := c.loadSecrets(); err != nil {
		return nil, err
	}
	c.applyEnvOverrides()

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// loadSecrets reads the secrets file (if configured and present) and merges its
// kite credentials into this config. A missing file is not an error — dry-run
// and partial setups must still work.
func (c *Config) loadSecrets() error {
	if c.SecretsPath == "" {
		return nil
	}
	p := ExpandPath(c.SecretsPath)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // missing secrets file is fine
		}
		return fmt.Errorf("read secrets %s: %w", p, err)
	}
	var s struct {
		Kite KiteConfig `yaml:"kite"`
		Web  struct {
			PasswordHash string `yaml:"password_hash"`
		} `yaml:"web"`
	}
	if err := yaml.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("parse secrets %s: %w", p, err)
	}
	// Merge: non-empty secrets-file values override config.yaml values.
	if s.Kite.APIKey != "" {
		c.Kite.APIKey = s.Kite.APIKey
	}
	if s.Kite.APISecret != "" {
		c.Kite.APISecret = s.Kite.APISecret
	}
	if s.Kite.AccessToken != "" {
		c.Kite.AccessToken = s.Kite.AccessToken
	}
	if s.Web.PasswordHash != "" {
		c.Web.PasswordHash = s.Web.PasswordHash
	}
	return nil
}

// ExpandPath expands a leading "~" to the user's home directory. Paths without
// a leading "~" are returned unchanged. Used for secrets_path so it can be
// written as ~/.trading/secrets.yaml portably across Windows and Unix.
func ExpandPath(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~"+string(os.PathSeparator)) || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		// Strip the "~" (keep the separator, whatever it was).
		return home + p[1:]
	}
	return p
}

// applyDefaults fills in sensible defaults for any unset fields.
func (c *Config) applyDefaults() {
	if c.Mode == "" {
		c.Mode = ModeDryRun
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	if c.Kite.BaseURL == "" {
		c.Kite.BaseURL = "https://api.kite.trade"
	}
	if c.Kite.TickerURL == "" {
		c.Kite.TickerURL = "wss://ws.kite.trade"
	}
	c.Kite.APIKeyHeader = "X-Kite-Version"
	if c.Storage.SQLitePath == "" {
		c.Storage.SQLitePath = "./data/trading.db"
	}
	if c.SecretsPath == "" {
		c.SecretsPath = "~/.trading/secrets.yaml"
	}
	if c.Risk.MaxOpenPositions == 0 {
		c.Risk.MaxOpenPositions = 5
	}
	if c.Risk.MaxLotsPerTrade == 0 {
		c.Risk.MaxLotsPerTrade = 1
	}
	if c.Web.Addr == "" {
		c.Web.Addr = "127.0.0.1:8080"
	}
	if c.Web.SessionTTL == 0 {
		c.Web.SessionTTL = 30 * 24 * time.Hour
	}
	if c.Web.TickIntervalMS == 0 {
		c.Web.TickIntervalMS = 200
	}
	if c.Web.PublicURL == "" {
		c.Web.PublicURL = "http://" + c.Web.Addr
	}
	c.Web.PublicURL = strings.TrimRight(c.Web.PublicURL, "/")
}

// KiteRedirectURL is the URL Zerodha sends the browser back to after login.
// This exact string must be registered as the Redirect URL of your Kite Connect
// app — Zerodha requires an exact match, with no wildcards.
func (c *Config) KiteRedirectURL() string { return c.Web.PublicURL + "/kite/callback" }

// applyEnvOverrides lets secrets come from the environment rather than the YAML
// file. Env wins over file so a deploy can inject credentials safely.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("KITE_API_KEY"); v != "" {
		c.Kite.APIKey = v
	}
	if v := os.Getenv("KITE_API_SECRET"); v != "" {
		c.Kite.APISecret = v
	}
	if v := os.Getenv("KITE_ACCESS_TOKEN"); v != "" {
		c.Kite.AccessToken = v
	}
}

// validate enforces safety invariants. Live mode without confirm, or live mode
// without credentials, is rejected loudly — we never want to accidentally place
// real orders.
func (c *Config) validate() error {
	switch c.Mode {
	case ModeDryRun, ModePaper, ModeLive:
	default:
		return fmt.Errorf("invalid mode %q (want dryrun|paper|live)", c.Mode)
	}

	if c.Mode == ModeLive {
		if !c.LiveConfirm {
			return fmt.Errorf("mode is 'live' but live_confirm is false; refusing to start in live mode by accident")
		}
		// api_key and api_secret must be provisioned ahead of time; the access
		// token deliberately is NOT required here. It is short-lived (Zerodha
		// expires it daily around 06:00 IST) and is now obtained through the
		// browser login flow, so demanding it at startup would leave the server
		// unable to boot and serve the very page that acquires it.
		//
		// This is safe because live mode is gated a second time at runtime: the
		// engine starts with a paper broker installed and only swaps in the live
		// broker after an explicit confirmation in the UI.
		if c.Kite.APIKey == "" || c.Kite.APISecret == "" {
			return fmt.Errorf("mode is 'live' but kite credentials are incomplete (need api_key and api_secret)")
		}
	}

	if c.Mode != ModeDryRun {
		if c.Kite.APIKey == "" {
			return fmt.Errorf("kite.api_key is required for %s mode (or set KITE_API_KEY env)", c.Mode)
		}
	}

	return c.validateWeb()
}

// validateWeb enforces that the web UI is never exposed without a password.
// An unauthenticated UI that can place orders on a routable address is the
// worst failure mode this application has, so it is a hard startup error rather
// than a warning.
func (c *Config) validateWeb() error {
	if c.Web.PasswordHash != "" {
		return nil
	}
	if IsLoopbackAddr(c.Web.Addr) {
		return nil // localhost-only with no password is fine for development
	}
	return fmt.Errorf(
		"web.addr is %q (not loopback) but no web password is set; run 'tradebot -set-password' first, "+
			"or bind to 127.0.0.1 and reach it through an authenticated tunnel", c.Web.Addr)
}

// IsLoopbackAddr reports whether a "host:port" listen address binds only to the
// loopback interface. An empty host (":8080") binds every interface and is not
// loopback.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HasWebPassword reports whether an operator password is configured.
func (c *Config) HasWebPassword() bool { return c.Web.PasswordHash != "" }

// TickInterval is the browser market-data flush interval.
func (c *Config) TickInterval() time.Duration {
	return time.Duration(c.Web.TickIntervalMS) * time.Millisecond
}

// LogLevel converts the string level to slog.Level.
func (c *Config) LogLevel() slog.Level {
	switch strings.ToLower(c.Log.Level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// HasCredentials reports whether enough Kite credentials are present to attempt
// a real connection. Dry-run mode tolerates missing credentials.
func (c *Config) HasCredentials() bool {
	return c.Kite.APIKey != "" && c.Kite.APISecret != ""
}

// ParamString fetches a string parameter from a strategy's params map.
func (s StrategyCfg) ParamString(key string) string {
	v, ok := s.Params[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// ParamFloat fetches a float parameter from a strategy's params map.
func (s StrategyCfg) ParamFloat(key string) float64 {
	v, ok := s.Params[key]
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}

// ParamInt fetches an int parameter from a strategy's params map.
func (s StrategyCfg) ParamInt(key string) int {
	v, ok := s.Params[key]
	if !ok {
		return 0
	}
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	case string:
		i, _ := strconv.Atoi(t)
		return i
	}
	return 0
}
