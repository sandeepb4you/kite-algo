-- Schema for the trading platform SQLite database.
-- All timestamps are stored as RFC3339 strings (TEXT) for portability.

-- Orders: one row per order submitted (paper or live).
CREATE TABLE IF NOT EXISTS orders (
    id                TEXT PRIMARY KEY,          -- internal uuid
    exchange_order_id TEXT NOT NULL DEFAULT '',  -- broker/exchange id
    client_order_id   TEXT NOT NULL DEFAULT '',  -- idempotency key
    strategy_id       TEXT NOT NULL DEFAULT '',
    exchange          TEXT NOT NULL,
    trading_symbol    TEXT NOT NULL,
    product           TEXT NOT NULL,
    order_type        TEXT NOT NULL,
    side              TEXT NOT NULL,
    quantity          INTEGER NOT NULL,
    filled_quantity   INTEGER NOT NULL DEFAULT 0,
    pending_quantity  INTEGER NOT NULL DEFAULT 0,
    price             REAL NOT NULL DEFAULT 0,
    trigger_price     REAL NOT NULL DEFAULT 0,
    validity          TEXT NOT NULL DEFAULT 'DAY',
    status            TEXT NOT NULL,
    tag               TEXT NOT NULL DEFAULT '',
    mode              TEXT NOT NULL,             -- paper | live
    reject_reason     TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_orders_status      ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_strategy    ON orders(strategy_id);
CREATE INDEX IF NOT EXISTS idx_orders_client_id   ON orders(client_order_id);
CREATE INDEX IF NOT EXISTS idx_orders_created_at  ON orders(created_at);

-- Fills: one row per (possibly partial) execution.
CREATE TABLE IF NOT EXISTS fills (
    id                TEXT PRIMARY KEY,
    order_id          TEXT NOT NULL,
    exchange_order_id TEXT NOT NULL DEFAULT '',
    strategy_id       TEXT NOT NULL DEFAULT '',
    exchange          TEXT NOT NULL,
    trading_symbol    TEXT NOT NULL,
    side              TEXT NOT NULL,
    quantity          INTEGER NOT NULL,
    price             REAL NOT NULL,
    mode              TEXT NOT NULL,
    timestamp         TEXT NOT NULL,
    FOREIGN KEY (order_id) REFERENCES orders(id)
);

CREATE INDEX IF NOT EXISTS idx_fills_order_id   ON fills(order_id);
CREATE INDEX IF NOT EXISTS idx_fills_symbol     ON fills(trading_symbol);
CREATE INDEX IF NOT EXISTS idx_fills_timestamp  ON fills(timestamp);

-- Positions: upserted on every fill. Net per strategy+symbol+product.
CREATE TABLE IF NOT EXISTS positions (
    strategy_id     TEXT NOT NULL,
    exchange        TEXT NOT NULL,
    trading_symbol  TEXT NOT NULL,
    product         TEXT NOT NULL,
    net_quantity    INTEGER NOT NULL,
    average_price   REAL NOT NULL DEFAULT 0,
    last_price      REAL NOT NULL DEFAULT 0,
    pnl             REAL NOT NULL DEFAULT 0,
    updated         TEXT NOT NULL,
    PRIMARY KEY (strategy_id, trading_symbol, product)
);

-- Ticks: recorded only when recording.ticks is enabled. This table grows fast
-- (lakhs of rows per day); keep it off unless backtesting.
CREATE TABLE IF NOT EXISTS ticks (
    instrument_token INTEGER NOT NULL,
    trading_symbol   TEXT NOT NULL,
    exchange         TEXT NOT NULL,
    last_price       REAL NOT NULL,
    last_quantity    INTEGER NOT NULL DEFAULT 0,
    volume           INTEGER NOT NULL DEFAULT 0,
    ohlc_open        REAL NOT NULL DEFAULT 0,
    ohlc_high        REAL NOT NULL DEFAULT 0,
    ohlc_low         REAL NOT NULL DEFAULT 0,
    ohlc_close       REAL NOT NULL DEFAULT 0,
    timestamp        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ticks_symbol    ON ticks(trading_symbol);
CREATE INDEX IF NOT EXISTS idx_ticks_timestamp ON ticks(timestamp);

-- Candles: aggregated OHLC bars, for charts and backtesting.
CREATE TABLE IF NOT EXISTS candles (
    instrument_token INTEGER NOT NULL,
    trading_symbol   TEXT NOT NULL,
    interval         TEXT NOT NULL,
    open             REAL NOT NULL,
    high             REAL NOT NULL,
    low              REAL NOT NULL,
    close            REAL NOT NULL,
    volume           INTEGER NOT NULL,
    open_time        TEXT NOT NULL,
    close_time       TEXT NOT NULL,
    PRIMARY KEY (trading_symbol, interval, open_time)
);

CREATE INDEX IF NOT EXISTS idx_candles_symbol ON candles(trading_symbol);

-- Kite session: the current Zerodha access token. Single row (id is pinned to
-- 1). Tokens expire daily around 06:00 IST and are obtained via an interactive
-- browser login, so persisting the live one is what lets the process restart
-- mid-session without forcing the operator back through Zerodha's login page.
--
-- This is deliberately NOT stored in the YAML secrets file: that file is
-- hand-edited and comment-rich, and rewriting it at runtime would destroy the
-- operator's own content. api_key/api_secret stay there; only this rotating
-- token lives here.
CREATE TABLE IF NOT EXISTS kite_sessions (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    api_key      TEXT NOT NULL,
    access_token TEXT NOT NULL,
    user_id      TEXT NOT NULL DEFAULT '',
    issued_at    TEXT NOT NULL,
    expires_at   TEXT NOT NULL
);

-- Web sessions: logged-in browser sessions for the single operator. Persisted
-- so a service restart does not log you out.
CREATE TABLE IF NOT EXISTS web_sessions (
    id         TEXT PRIMARY KEY,          -- opaque random token from the cookie
    csrf_token TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    user_agent TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_web_sessions_expires ON web_sessions(expires_at);
