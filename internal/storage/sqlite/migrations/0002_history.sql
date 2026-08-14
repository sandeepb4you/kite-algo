-- Historical market data: fetch coverage and point-in-time instrument masters.

-- candle_coverage records which (symbol, interval, window) ranges have already
-- been fetched.
--
-- Without it there is no way to tell "no candles because we never asked" from
-- "no candles because the market was shut" — so every backtest over a holiday,
-- a weekend, or an untraded minute would re-hit the API forever. Kite allows
-- 3 requests/second, which makes that difference expensive.
CREATE TABLE IF NOT EXISTS candle_coverage (
    trading_symbol TEXT NOT NULL,
    interval       TEXT NOT NULL,
    from_time      TEXT NOT NULL,
    to_time        TEXT NOT NULL,
    source         TEXT NOT NULL DEFAULT 'kite',  -- kite | ticks | csv
    fetched_at     TEXT NOT NULL,
    PRIMARY KEY (trading_symbol, interval, from_time)
);

CREATE INDEX IF NOT EXISTS idx_coverage_lookup
    ON candle_coverage(trading_symbol, interval, from_time, to_time);

-- instrument_snapshots stores the instrument master as it stood on a given day.
--
-- This is the most time-sensitive table in the schema. Kite's /instruments feed
-- lists only LIVE contracts, and historical candles are keyed by
-- instrument_token — so once a weekly option expires, its token is gone from the
-- API and no backtest can ever resolve that contract again. Any day the server
-- runs without writing a snapshot is a day that can never be backtested.
CREATE TABLE IF NOT EXISTS instrument_snapshots (
    as_of            TEXT NOT NULL,          -- YYYY-MM-DD in IST
    instrument_token INTEGER NOT NULL,
    trading_symbol   TEXT NOT NULL,
    name             TEXT NOT NULL DEFAULT '',
    expiry           TEXT NOT NULL DEFAULT '',
    strike           REAL NOT NULL DEFAULT 0,
    lot_size         INTEGER NOT NULL DEFAULT 0,
    instrument_type  TEXT NOT NULL DEFAULT '',
    segment          TEXT NOT NULL DEFAULT '',
    exchange         TEXT NOT NULL DEFAULT '',
    tick_size        REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (as_of, instrument_token)
);

CREATE INDEX IF NOT EXISTS idx_instr_snapshot_symbol
    ON instrument_snapshots(as_of, trading_symbol);
CREATE INDEX IF NOT EXISTS idx_instr_snapshot_chain
    ON instrument_snapshots(as_of, name, expiry, strike);

-- Open interest is meaningful for options and is returned by Kite's historical
-- endpoint, but the original candles table predates that.
ALTER TABLE candles ADD COLUMN open_interest INTEGER NOT NULL DEFAULT 0;
