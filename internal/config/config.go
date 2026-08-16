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
	"sort"
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
	Capture     CaptureConfig `yaml:"capture"`
	Storage     StorageConfig `yaml:"storage"`
	Risk        RiskConfig    `yaml:"risk"`
	Web         WebConfig     `yaml:"web"`
	Strategies  []StrategyCfg `yaml:"strategies"`

	// fileMissing records that no config file was found, so startup can say so
	// rather than leaving the operator to wonder why their settings had no effect.
	fileMissing bool
}

// FileMissing reports whether the config file was absent and defaults were used.
func (c *Config) FileMissing() bool { return c.fileMissing }

// DisabledRiskLimits names the risk checks currently switched off.
//
// A zero limit means "no limit", which is a legitimate choice but a dangerous
// default to arrive at by accident — deleting a config file silently disables
// the daily-loss and order-value caps, and nothing else would say so.
func (c *Config) DisabledRiskLimits() []string {
	var off []string
	if c.Risk.MaxDailyLoss <= 0 {
		off = append(off, "max_daily_loss")
	}
	if c.Risk.MaxOrderValue <= 0 {
		off = append(off, "max_order_value")
	}
	if c.Risk.MaxOpenPositions <= 0 {
		off = append(off, "max_open_positions")
	}
	if c.Risk.MaxLotsPerTrade <= 0 {
		off = append(off, "max_lots_per_trade")
	}
	return off
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

// CaptureConfig controls the daily option-candle capture job.
//
// This exists because Kite sells no historical data for expired contracts. A
// weekly option's candles are purchasable right up to expiry and unobtainable
// at any price the day after, so the only way to ever backtest against them is
// to have downloaded them while the contract was alive. The job below is that
// download, and a day it does not run is a day of option data that is gone.
type CaptureConfig struct {
	// Enabled turns the daily job on. Off by default: it spends historical-data
	// quota, and that should be a deliberate choice.
	Enabled bool `yaml:"enabled"`

	// RunAt is the daily trigger in IST, "HH:MM". Should sit after the 15:30
	// close so the final bar of the session is settled.
	RunAt string `yaml:"run_at"`

	// Interval is the candle interval to capture. "5minute" is the intended
	// setting; "minute" gives finer data at 5x the rows and the same request
	// count.
	Interval string `yaml:"interval"`

	// Strikes is how many strikes to capture on EACH side of the day's traded
	// range — 20 means roughly 41 strikes per expiry on a quiet day, more when
	// the underlying travelled.
	Strikes int `yaml:"strikes"`

	// Expiries is how many expiries deep to go, nearest first.
	Expiries int `yaml:"expiries"`

	// LookbackDays is how far back to reach the first time a contract is seen.
	// Coverage tracking means this cost is paid once per contract, not daily —
	// and it retroactively recovers the history of contracts that are still
	// alive today, which is the only backfill Kite still permits.
	LookbackDays int `yaml:"lookback_days"`

	// Underlyings are the option chains to capture.
	Underlyings []CaptureUnderlying `yaml:"underlyings"`

	// Holidays are exchange holidays as YYYY-MM-DD. Weekends are skipped
	// structurally; holidays are not derivable and must be listed. An unlisted
	// holiday costs a few empty requests, never wrong data.
	Holidays []string `yaml:"holidays"`
}

// CaptureUnderlying is one option chain to capture.
type CaptureUnderlying struct {
	// Name is the underlying as it appears in the instrument master: "NIFTY",
	// "SENSEX".
	Name string `yaml:"name"`

	// Index is the spot symbol whose price centres the strike window:
	// "NIFTY 50", "SENSEX". Must be a known index (see kite.IndexTokens).
	Index string `yaml:"index"`
}

// CaptureExchanges returns the instrument-master exchanges the configured
// underlyings need. NSE derivatives are in NFO, BSE derivatives in BFO, and the
// two are separate downloads.
func (c *Config) CaptureExchanges() []string {
	need := map[string]bool{"NFO": true} // always loaded; the platform trades it
	for _, u := range c.Capture.Underlyings {
		switch strings.ToUpper(u.Name) {
		case "SENSEX", "BANKEX":
			need["BFO"] = true
		}
	}
	out := make([]string, 0, len(need))
	for ex := range need {
		out = append(out, ex)
	}
	sort.Strings(out)
	return out
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

	// Live are the REAL book's limits. Derived and locked: unlike Paper, these
	// are not editable from the UI. A limit you can loosen from a browser at
	// the moment it starts hurting is not a limit.
	Live LiveRiskConfig `yaml:"live"`

	// Paper are the limits applied to the SIMULATED book, which runs alongside
	// the real one: manual orders can be routed to the exchange while every
	// strategy stays simulated.
	//
	// They are separate because the two hold different money. A strategy under
	// evaluation is supposed to be allowed to lose more than you would risk by
	// hand, and a simulated blow-up must not block real manual trading. Unset
	// fields fall back to the real limits above.
	Paper PaperRiskConfig `yaml:"paper"`
}

// LiveRiskConfig is the real book's policy. Changing any of it requires
// editing this file and restarting — deliberately.
type LiveRiskConfig struct {
	// MaxLossPct is the daily loss cap as a percentage of the account's
	// OPENING balance, snapshotted once per trading day. A percentage rather
	// than a rupee figure so the limit tracks the account; snapshotted rather
	// than live because available margin falls as a position moves against you,
	// and a limit derived from it would tighten exactly when you are already
	// hurting. Default 1.0.
	MaxLossPct float64 `yaml:"max_loss_pct"`

	// ExpirySquareOffTime is the IST time ("15:00") at which open REAL
	// positions in contracts expiring TODAY are flattened. Expiry-day gamma
	// makes a position that looked small at noon very large by the close.
	ExpirySquareOffTime string `yaml:"expiry_square_off_time"`

	// MaxOpenPositions, MaxOrderValue and MaxLotsPerTrade mirror the fields
	// above and apply to the real book only. Unset inherits the top-level
	// values.
	MaxOpenPositions int     `yaml:"max_open_positions"`
	MaxOrderValue    float64 `yaml:"max_order_value"`
	MaxLotsPerTrade  int     `yaml:"max_lots_per_trade"`
}

// PaperRiskConfig overrides risk limits for the simulated book.
type PaperRiskConfig struct {
	MaxDailyLoss     float64 `yaml:"max_daily_loss"`
	MaxOpenPositions int     `yaml:"max_open_positions"`
	MaxOrderValue    float64 `yaml:"max_order_value"`
	MaxLotsPerTrade  int     `yaml:"max_lots_per_trade"`
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
// The config file is OPTIONAL. A missing one is not an error: the platform runs
// on defaults plus environment variables, which is what a container or systemd
// deployment usually wants. A file that exists but cannot be parsed IS an error,
// because that means the operator wrote settings that are being ignored.
//
// Credential precedence (most specific wins):
//
//  1. environment variables (KITE_API_KEY / KITE_API_SECRET / KITE_ACCESS_TOKEN)
//  2. secrets file (secrets_path, outside the repo — ~/.trading/secrets.yaml)
//  3. this config file (config.yaml)
//
// Non-secret settings follow the same shape: TRADING_* environment variables
// override the file. Note there is deliberately NO environment override for
// live_confirm — arming live trading must be a deliberate edit to a file on the
// machine, not something an exported variable can flip.
func Load(path string) (*Config, error) {
	c := &Config{}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	case os.IsNotExist(err):
		c.fileMissing = true // reported at startup so it is never a silent surprise
	default:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	c.applyDefaults()

	// The secrets path must be resolved BEFORE the secrets are read, or
	// TRADING_SECRETS_PATH would be accepted and then silently ignored — the
	// file would still be loaded from the default location.
	if v := strings.TrimSpace(os.Getenv("TRADING_SECRETS_PATH")); v != "" {
		c.SecretsPath = v
	}
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

	// Capture defaults are applied whether or not the job is enabled, so that
	// turning it on is a one-line change rather than a block of boilerplate the
	// operator has to get right.
	if c.Capture.RunAt == "" {
		c.Capture.RunAt = "15:40"
	}
	if c.Capture.Interval == "" {
		c.Capture.Interval = "5minute"
	}
	if c.Capture.Strikes == 0 {
		c.Capture.Strikes = 20
	}
	if c.Capture.Expiries == 0 {
		c.Capture.Expiries = 4
	}
	if c.Capture.LookbackDays == 0 {
		c.Capture.LookbackDays = 30
	}
	if c.Risk.Live.MaxLossPct == 0 {
		c.Risk.Live.MaxLossPct = 1.0
	}
	if c.Risk.Live.ExpirySquareOffTime == "" {
		c.Risk.Live.ExpirySquareOffTime = "15:00"
	}
	if len(c.Capture.Underlyings) == 0 {
		c.Capture.Underlyings = []CaptureUnderlying{
			{Name: "NIFTY", Index: "NIFTY 50"},
			{Name: "SENSEX", Index: "SENSEX"},
		}
	}
	for i := range c.Capture.Underlyings {
		u := &c.Capture.Underlyings[i]
		u.Name = strings.ToUpper(strings.TrimSpace(u.Name))
		u.Index = strings.TrimSpace(u.Index)
	}
}

// KiteRedirectURL is the URL Zerodha sends the browser back to after login.
// This exact string must be registered as the Redirect URL of your Kite Connect
// app — Zerodha requires an exact match, with no wildcards.
func (c *Config) KiteRedirectURL() string { return c.Web.PublicURL + "/kite/callback" }

// applyEnvOverrides lets settings come from the environment rather than the YAML
// file. Env wins over file so a deploy can inject configuration safely, and so
// the platform is fully runnable with no config file at all.
//
// live_confirm is deliberately absent: arming live trading should require
// editing a file on the machine, not exporting a variable that a shell profile,
// a CI job, or a stray `export` could set.
func (c *Config) applyEnvOverrides() {
	// Credentials.
	if v := os.Getenv("KITE_API_KEY"); v != "" {
		c.Kite.APIKey = v
	}
	if v := os.Getenv("KITE_API_SECRET"); v != "" {
		c.Kite.APISecret = v
	}
	if v := os.Getenv("KITE_ACCESS_TOKEN"); v != "" {
		c.Kite.AccessToken = v
	}

	// Non-secret settings.
	if v := os.Getenv("TRADING_MODE"); v != "" {
		c.Mode = Mode(strings.ToLower(strings.TrimSpace(v)))
	}
	if v := os.Getenv("TRADING_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("TRADING_SQLITE_PATH"); v != "" {
		c.Storage.SQLitePath = v
	}
	if v := os.Getenv("TRADING_SECRETS_PATH"); v != "" {
		c.SecretsPath = v
	}
	if v := os.Getenv("TRADING_WEB_ADDR"); v != "" {
		c.Web.Addr = v
	}
	if v := os.Getenv("TRADING_PUBLIC_URL"); v != "" {
		c.Web.PublicURL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("TRADING_RECORD_TICKS"); v != "" {
		c.Recording.Ticks = isTruthy(v)
	}
	if v := os.Getenv("TRADING_TRUST_PROXY"); v != "" {
		c.Web.TrustProxy = isTruthy(v)
	}

	// Risk limits, so a fileless deployment is not forced to run uncapped.
	if v, ok := envFloat("TRADING_MAX_DAILY_LOSS"); ok {
		c.Risk.MaxDailyLoss = v
	}
	if v, ok := envFloat("TRADING_MAX_ORDER_VALUE"); ok {
		c.Risk.MaxOrderValue = v
	}
	if v, ok := envInt("TRADING_MAX_LOTS_PER_TRADE"); ok {
		c.Risk.MaxLotsPerTrade = v
	}
	if v, ok := envInt("TRADING_MAX_OPEN_POSITIONS"); ok {
		c.Risk.MaxOpenPositions = v
	}
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envFloat(name string) (float64, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func envInt(name string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
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
