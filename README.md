# Algo Options Trading Platform (Go + Zerodha Kite)

A modular, **paper-first** algorithmic options-trading platform built in Go on
top of the Zerodha Kite Connect API. It streams live market data, computes
options Greeks, runs pluggable strategies, and executes through either a
**simulated (paper)** broker or **live** Kite orders — switching is a config
change.

> ⚠️ **Options can wipe out your capital.** This platform trades paper by
> default. Live mode requires a config flag AND typing `I UNDERSTAND` at
> startup. Even then, start with 1 lot until you've validated end-to-end.

---

## Features

- **Three modes** — `dryrun` (no keys, smoke test), `paper` (live data, fake
  orders), `live` (real orders, double-gated).
- **Robust Kite Ticker** — verified binary protocol, auto-reconnect with
  exponential backoff, automatic resubscribe.
- **Options math** — Black-Scholes price + Greeks (Delta/Gamma/Theta/Vega/Rho),
  implied-volatility solver, ATM-strike and symbol parsing helpers. Verified
  against textbook values (`go test ./internal/options`).
- **Pluggable strategies** — implement the `Strategy` interface, register in
  config. Ships a delta-managed **short-straddle** example.
- **Risk manager** — pre-trade checks: max daily loss, max open positions, max
  order value, max lots per trade.
- **Persistence** — SQLite (pure-Go, no CGO) for orders, fills, positions,
  candles, and optional tick recording for backtesting.
- **Rate limiting** — token-bucket limiter on every Kite call (3/sec) to avoid
  429s and API blocks.

---

## Architecture

```
                         ┌────────────────────────────────────────────┐
                         │                   Engine                    │
                         │  (implements strategy.Trader; owns wiring)  │
                         └────────────────────────────────────────────┘
          ticks │                                          │ orders (risk-checked)
                ▼                                          ▼
        ┌──────────────┐   OnPrice    ┌──────────────┐   ┌───────────────────┐
        │ Kite Ticker  │────────────▶ │ PaperBroker  │   │   Risk Manager    │
        │  (WebSocket) │              │  (simulate)  │   │ (max loss/pos/…)  │
        └──────────────┘              └──────────────┘   └───────────────────┘
                │  ticks & order updates                        │ allowed
                ▼                                               ▼
        ┌──────────────┐            ┌──────────────────────────────────┐
        │  Strategies  │◀─── fills──│     Broker (paper | live)        │
        │ (short strad.)│           │   LiveBroker → Kite REST client  │
        └──────────────┘            └──────────────────────────────────┘
                │                                              │
                └──────────── both persist ───────────────────▶│
                                          ┌─────────────────────┴────┐
                                          │   SQLite (orders/fills/  │
                                          │   positions/ticks/candles)│
                                          └──────────────────────────┘
```

The `Engine` is the hub: it routes ticks from the ticker to strategies, feeds
prices to the paper broker (so it can simulate fills), runs every order through
the risk manager before the broker, persists everything, and fans fills back to
strategies. Strategies never touch the broker directly — they call `Trader`.

---

## Project structure

```
cmd/trading/             main() — config, wiring, live double-gate
internal/
  config/                YAML config + env-var overrides + validation
  logger/                slog structured logger
  kite/                  Kite REST client + WebSocket ticker + binary parser
  marketdata/            broker-agnostic Tick/Quote/Candle types
  broker/                Broker interface, types, PaperBroker, LiveBroker
  options/               Black-Scholes Greeks, implied vol, symbol parsing
  risk/                  pre-trade limit checks
  strategy/              Strategy + Trader interfaces
    examples/shortstraddle/   delta-managed short straddle (reference)
  storage/               Store interface
    sqlite/              SQLite implementation + embedded schema
  engine/                the orchestrator (wires everything)
pkg/ratelimiter/         token-bucket limiter
```

---

## Prerequisites

- **Go 1.22+** (built and tested on Go 1.26; uses only stdlib + 4 deps)
- A **Zerodha Kite Connect** app → `api_key` + `api_secret`
  (create one at <https://developers.kiteconnect.com/apps>)

No C compiler needed — the SQLite driver (`modernc.org/sqlite`) is pure Go, so
this builds cleanly on Windows out of the box.

---

## Setup

```bash
# 1. Get dependencies
go mod download

# 2. Optional: copy the example config and edit it
cp config.example.yaml config.yaml

# 4. Put Kite credentials in a SECRETS FILE OUTSIDE the repo (recommended),
#    so credentials never travel with the code:
go build -o tradebot ./cmd/trading
./tradebot -init-secrets          # writes a template to ~/.trading/secrets.yaml
$EDITOR ~/.trading/secrets.yaml   # fill in api_key + api_secret (see below)
```

### The config file is optional

`config.yaml` is gitignored and **not required**. With no config file the
platform runs on defaults plus `TRADING_*` environment variables, which is the
usual shape for a container or a systemd unit:

```bash
TRADING_MODE=paper \
TRADING_MAX_DAILY_LOSS=5000 \
TRADING_MAX_ORDER_VALUE=100000 \
./tradebot
```

| Variable | Overrides |
|---|---|
| `TRADING_MODE` | `dryrun` \| `paper` \| `live` |
| `TRADING_LOG_LEVEL` | `debug` \| `info` \| `warn` \| `error` |
| `TRADING_WEB_ADDR` · `TRADING_PUBLIC_URL` | listen address, external URL |
| `TRADING_SQLITE_PATH` · `TRADING_SECRETS_PATH` | file locations |
| `TRADING_MAX_DAILY_LOSS` · `TRADING_MAX_ORDER_VALUE` | risk caps (rupees) |
| `TRADING_MAX_LOTS_PER_TRADE` · `TRADING_MAX_OPEN_POSITIONS` | risk caps (counts) |
| `TRADING_RECORD_TICKS` · `TRADING_TRUST_PROXY` | booleans |
| `KITE_API_KEY` · `KITE_API_SECRET` · `KITE_ACCESS_TOKEN` | credentials |

A file that is *missing* is fine; a file that is *malformed* is a hard error,
because that means settings you wrote are being ignored.

> **`live_confirm` has no environment override, deliberately.** Arming live
> trading must be an edit to a file on the machine — not something a shell
> profile, a CI job, or a stray `export` can flip. With no config file, live mode
> cannot be armed at all.

> ⚠️ **Watch the risk limits when running without a file.** `max_daily_loss` and
> `max_order_value` have no defaults, and zero means *no limit*. Startup logs a
> `RISK LIMITS DISABLED` warning listing exactly which checks are off. Set them
> via the table above, or on the `/risk` page at runtime.

### Keeping credentials out of the repo

Credentials can come from three places. **Precedence (highest wins):**

1. **Environment variables** — `KITE_API_KEY`, `KITE_API_SECRET`, `KITE_ACCESS_TOKEN`
   (best for a VPS / `systemd` / Docker deployment).
2. **Secrets file** (default `~/.trading/secrets.yaml`, i.e.
   `C:\Users\<you>\.trading\secrets.yaml` on Windows) — **recommended for local
   dev**, lives outside the repo. Create it with `./tradebot -init-secrets`.
3. `config.yaml` — the fallback; leave it empty so the repo stays clean.

The startup log tells you which source was used (`source=secrets-file`,
`source=env`, or `source=config.yaml`) and prints the resolved `secrets_path`.
Disable the secrets file by setting `secrets_path: ""` in config.

### Setting the web password

The UI is password-protected. Set it once, **in your own terminal** — the prompt
needs a real TTY to hide what you type:

```bash
./tradebot -set-password        # writes a PBKDF2 hash into your secrets file
```

Only the hash is stored; the plaintext never touches disk. Minimum 12 characters.

The app refuses to bind anything but loopback until a password is set — an
unauthenticated UI that can place orders is the worst failure mode here.

> The secrets file is written with mode `0600`, which protects it on Linux and
> macOS. **On Windows those bits are ignored** — NTFS inherits the parent
> directory's ACL instead. For local development on Windows that is usually fine
> (the file sits under your user profile), but it is not the same guarantee.

### Getting the access token

Kite uses short-lived session tokens that expire every morning around 06:00 IST.
**You no longer manage this by hand.**

1. Register your Kite Connect app's **Redirect URL** as exactly
   `<web.public_url>/kite/callback` — e.g. `https://trade.example.com/kite/callback`.
   Zerodha matches it character for character and allows no wildcards. The
   `/connect` page in the UI shows you the exact string to paste.
2. Start the server and open it in a browser. It starts fine with no token.
3. Sign in, click **Log in with Zerodha**, and you are returned connected.

The token is stored in the database, so restarting the process mid-session does
not send you back through Zerodha. When it expires the next morning, the UI shows
a banner and one click renews it.

> The redirect is a *browser* navigation, not a server-to-server callback —
> Zerodha never contacts your machine. So the server only has to be reachable
> from your own browser, which means a Tailscale/WireGuard address works and the
> VM need not be exposed to the internet at all.

---

## Running

```bash
# Dry-run: no keys, just boots and idles (smoke test)
go run ./cmd/trading -config config.yaml      # mode: dryrun

# Paper: live market data, simulated fills. Use this for strategy validation.
#   (set mode: paper + KITE_ACCESS_TOKEN first)
go run ./cmd/trading -config config.yaml

# Live: REAL orders. Requires mode: live, live_confirm: true, and typing
# "I UNDERSTAND" at startup. Start with 1 lot.
go run ./cmd/trading -config config.yaml
```

Build a standalone binary:

```bash
go build -o tradebot ./cmd/trading
./tradebot -config config.yaml
```

Stop with **Ctrl+C**. (On Windows the process responds to Ctrl+C; if you kill it
another way, use `taskkill /F /IM tradebot.exe`.)

---

## Testing

```bash
go test ./...            # all tests
go test ./internal/options   # Black-Scholes + IV + symbol parsing
go test ./internal/kite      # binary tick parser (verified vs documented layout)
go test ./internal/broker    # paper fills + position PnL
go vet ./...             # static checks
```

---

## Writing a strategy

Implement `strategy.Strategy`:

```go
type MyStrategy struct{}
func (s *MyStrategy) Name() string { return "my-strategy" }
func (s *MyStrategy) Init(ctx context.Context, t strategy.Trader, cfg config.StrategyCfg) error {
    // read params from cfg.ParamString/ParamFloat/ParamInt
    return t.Subscribe([]string{"NIFTY 50"})   // start streaming
}
func (s *MyStrategy) OnTick(ctx context.Context, tick marketdata.Tick) {
    // analyze, then:
    // t.PlaceOrder(ctx, broker.OrderRequest{ ... })
}
func (s *MyStrategy) OnFill(ctx context.Context, fill broker.Fill) {}
func (s *MyStrategy) Stop(ctx context.Context) error { return nil }
```

Then declare its parameters as data and register it:

```go
func init() { strategy.Register(Descriptor()) }

func Descriptor() strategy.Descriptor {
    return strategy.Descriptor{
        Type:    "my-strategy",
        Title:   "My strategy",
        Factory: func(id string, log *slog.Logger) (strategy.Strategy, error) { ... },
        Params: []strategy.ParamSpec{
            {Key: "lots", Label: "Lots", Kind: strategy.KindInt,
             Default: 1, Min: strategy.Ptr(1), Max: strategy.Ptr(10)},
        },
    }
}
```

Add a blank import to `internal/strategy/catalog` and it appears in the web UI
with a generated configuration form — **no template or UI changes needed**. The
registry coerces form strings to the declared types, applies defaults, enforces
ranges, and rejects unknown keys, so `Init` can trust what it receives.

The `Trader` you receive exposes `PlaceOrder` (risk-checked), `LTP`, `LotSize`,
`Lookup`, `Options`, `Subscribe`, `Now`, and `Signal`.

> **Use `trader.Now()`, never `time.Now()`.** In a backtest the trader supplies
> simulated time; a strategy that reads the wall clock while replaying past data
> evaluates its exit windows and greeks against today, which makes the backtest
> quietly wrong rather than obviously broken.

> **`Stop` must not place orders.** Whether an outgoing strategy's positions are
> squared off is the operator's decision, taken per-stop in the UI. Implement
> `strategy.Flattener` if your strategy needs to unwind its legs in a particular
> order; the engine calls it when square-off is requested.

---

## Configuration reference

See `config.example.yaml` for all options with comments. Key sections:

| Section   | Purpose                                                     |
|-----------|-------------------------------------------------------------|
| `mode`    | `dryrun` / `paper` / `live`                                 |
| `kite`    | credentials + endpoints (env vars override)                 |
| `risk`    | `max_daily_loss`, `max_open_positions`, `max_order_value`, `max_lots_per_trade` |
| `recording.ticks` | record every tick (huge) for backtesting          |
| `storage.sqlite_path` | DB file location                                  |
| `strategies` | list of `{name, enabled, params}` for each strategy     |

---

## Safety model

1. **Default is safe** — `mode: dryrun` or `paper`; no real orders possible.
2. **Live requires three gates**:
   - `live_confirm: true` in config, **and**
   - the process still boots with a *simulated* broker installed, so nothing can
     reach the exchange even in live mode, **and**
   - an explicit confirmation in the web UI: typing `I UNDERSTAND` *and*
     re-entering your password. Only then is the live broker swapped in.

   The confirmation is dropped on restart, so a crash-restart never resumes real
   trading unattended.
3. **Risk manager runs before every order** — daily-loss halt, position cap,
   order-value cap, lot-size validation. Manual orders from the web UI go
   through the same checks; a hand-typed order is not a trusted order.
4. **No limit ever blocks an exit.** Every risk rule caps the risk you are
   *taking on*; applied to an exit they trap you in the position they exist to
   protect you from. An order marked `OrderIntent: IntentClose` passes the risk
   manager unconditionally, and the kill switch does not block square-offs.

   This was got wrong four separate times, each plausible in isolation — the
   daily-loss cap firing on the day you most need to flatten; the order-value cap
   refusing to close a position larger than your entry size; the lots-per-trade
   cap refusing to close a 3-lot position built from three 1-lot entries; and the
   kill switch blocking its own square-off. Quantity validity is left to the
   exchange, which is the real authority on what it will accept.
5. **Order quantity is entered in lots**, not shares. The server multiplies by
   the instrument's lot size, so a stray keystroke cannot produce an order 75×
   larger than intended.
6. **Kill switch.** Halting blocks every new order at `Trader.PlaceOrder` — the
   only route a strategy has to the market — and stops all strategies. Square-off
   is a separate button, because halting and flattening are different decisions.
   Closing orders bypass the halt for the same reason they bypass the loss limit.
7. **Stopping a strategy asks what to do with its positions.** Two buttons, no
   default: leaving short options unmanaged and closing them unrequested are both
   bad outcomes, and only the operator knows which they meant.
8. **A strategy that panics is quarantined, not fatal.** The tick fan-out runs on
   the market-data goroutine, so an unrecovered panic would take down streaming,
   the UI, and every other strategy while positions stayed open.
9. **Every order/fill is persisted** with its mode (`paper`/`live`) for audit,
   and every order row in the UI shows which broker handled it.

---

## Known limitations

- Realized P&L on **fully-closed** positions in *live* mode isn't reflected in
  the in-memory day-P&L (Kite's `net` positions drop flat rows). Paper mode
  tracks it correctly. Workaround: persist fills and compute from the `fills`
  table.
- No backtesting engine yet (tick/candle *recording* is in place; replay isn't).
- **Backtests can only cover periods with an instrument snapshot.** Kite drops
  expired contracts from its feed, so a backtest over dates before this server
  first ran cannot resolve its option symbols. It fails loudly rather than
  reporting an empty result.
- Backtest runs are not persisted — results live only in the page that produced
  them. Strategy parameters other than lots use the descriptor's defaults; a full
  per-strategy parameter form on the backtest page is the obvious next step.
- Backtests run synchronously in the request. A few days of 5-minute bars is
  sub-second; a months-long minute-resolution run would want a job queue.
- Exchange holidays are not preloaded. The calendar handles weekends
  structurally, but until holidays are configured a few empty windows get
  fetched on holiday dates — wasteful, never wrong.
- Risk limits edited in the UI are held in memory and revert to `config.yaml` on
  restart. A halt does not survive a restart either — a crash-restart resumes
  with trading enabled.
- Single broker (Kite), single account.
- Holiday calendar not tracked (expiry/square-off assumes last Thursday).
- The `-race` detector needs a C toolchain, which this project deliberately
  avoids (pure-Go SQLite). Run `go test -race` on a machine with gcc, or in CI.

---

## Backtesting

`/backtest` replays stored candles through the **real** strategy, broker, and
risk-manager code. A strategy runs unmodified in backtest, paper, and live,
because all three talk only to `strategy.Trader` and share the same
`broker.PaperBroker` execution path.

`TestPaperAndBacktestAgree` asserts that the same strategy over the same prices
produces identical fills through the live engine and through the backtester. If
that ever fails, the backtester has stopped predicting what paper trading does —
which makes every number it reports untrustworthy, however plausible it looks.

**Determinism is an invariant.** The runner is single-goroutine, the event feed
imposes a total order on `(time, symbol)`, and nothing reads the wall clock. A
backtest is a measurement; one that changes between runs is worthless.

Things the backtester does deliberately, because the alternative flatters the
strategy:

| Choice | Why |
|---|---|
| **Costs and slippage on by default** | A straddle round trip costs ~₹121 in charges. At zero cost a losing strategy looks profitable. |
| **Pessimistic intrabar path** | A candle records four prices in no defined order. When the data cannot say whether the high or low came first, assume the adverse one. |
| **Open positions force-closed** | Otherwise a strategy that never exits contributes no trade, and its unrealized loss vanishes from the report. |
| **Mid-run subscriptions carry no lookahead** | A symbol attached at 11:00 never receives the morning's bars. |
| **The real risk manager runs** | A backtest that ignores position limits measures a strategy nobody could have run. |
| **Sharpe is zero below two trading days** | A ratio from one observation is noise dressed as a statistic. |

> Equity is capital plus cumulative net P&L. **Margin is not modelled**, so for an
> option-selling strategy the return percentage is a comparison between runs, not
> a true account return.

⚠️ Statutory rates in the cost model (STT, GST, exchange, stamp duty) change with
every Indian budget. Verify against Zerodha's brokerage calculator before
trusting a result. Defaults reflect the post-October-2024 regime.

---

## Historical data

`/research` loads candles for any instrument and interval. Data is cached in
SQLite, and a **coverage table** records which windows have been fetched — so a
repeat request costs nothing, only genuine gaps are downloaded, and a weekend
that legitimately has no candles is never requested twice. Non-trading days are
skipped before the request is made.

Historical requests use a **separate rate-limit budget** from trading. A backfill
is thousands of requests; sharing one bucket would let it queue ahead of order
placement and delay an entry — or a square-off — by seconds.

Without Zerodha's Historical Data subscription, the platform falls back to
candles aggregated from ticks it recorded itself (`recording.ticks: true`). Those
are labelled as tick-derived; they are not exchange data and should not be
treated as equivalent.

### Instrument snapshots — start these early

> Kite's `/instruments` feed lists only **live** contracts, and historical
> candles are keyed by `instrument_token`. When a weekly option expires its token
> disappears from the API, and no backtest can ever resolve that contract again.
> **The data is not recoverable at any price.**

The instrument master is snapshotted automatically on every Zerodha login, so
simply running the server captures it. `/research` warns when today's snapshot is
missing. Every day the server runs without one is a day that can never be
backtested.

---

## Database migrations

The schema is versioned with SQLite's `PRAGMA user_version`. `schema.sql` is the
version 1 baseline; everything after it is a numbered file in
`internal/storage/sqlite/migrations/`, applied in its own transaction at startup.

Databases created before versioning existed upgrade in place — the baseline is
all `CREATE TABLE IF NOT EXISTS`, so applying it to an existing file is a no-op
that stamps the version and moves on.

Never edit a migration that has shipped; add a new one.

---

## Web UI

Server-rendered Go templates plus a small amount of vanilla JavaScript. **No
frontend build step and no third-party JS** — the ~30 lines of fragment polling
that would otherwise justify htmx live in `static/app.js`, and the page's CSP
forbids loading anything from another origin.

Go dependencies are kept to three: `gorilla/websocket` for the market-data
socket, `modernc.org/sqlite` for CGO-free storage, and `golang.org/x/term` to
hide the password prompt. Everything else is standard library.

Market data reaches the browser over a WebSocket (`static/ws.js`), coalesced
server-side to roughly 5 updates/sec per client — an option chain ticks far
faster than a screen can be read. Each browser only receives the symbols it
declared, via `data-ltp` attributes in the rendered HTML.

The socket is a **latency optimisation, not a correctness dependency**: every
live region also carries a `data-poll` fallback, so a broken socket makes the UI
a few seconds stale rather than wrong.

---

## Roadmap

- Persist backtest runs so results can be compared over time
- Per-strategy parameter form on the backtest page
- Parameter sweeps (the `ParamSpec` min/max metadata is already the right shape)
- Fill-based realized P&L reconciliation for live mode
- Multi-leg order types (spread entry as one unit)
- Exchange holiday calendar

---

## Disclaimer

This is educational software. Trading options involves substantial risk of
loss. The authors are not responsible for any financial losses. **Do not trade
with money you cannot afford to lose.** Validate thoroughly in paper mode first.
