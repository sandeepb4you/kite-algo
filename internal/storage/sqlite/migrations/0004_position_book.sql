-- Positions gain a `book`: real money or simulated.
--
-- Manual orders can now route to the exchange while strategies stay on the
-- paper broker, so both kinds of position exist at the same time. The book must
-- be part of the primary key, not merely a column: a manual position held in
-- simulation and later the same symbol held for real share
-- (strategy_id='manual', trading_symbol, product) and would otherwise collide,
-- silently overwriting a real position with a simulated one.
--
-- SQLite cannot alter a primary key in place, so the table is rebuilt. Existing
-- rows predate live routing and are therefore simulated.

CREATE TABLE IF NOT EXISTS positions_new (
    strategy_id     TEXT NOT NULL,
    exchange        TEXT NOT NULL,
    trading_symbol  TEXT NOT NULL,
    product         TEXT NOT NULL,
    book            TEXT NOT NULL DEFAULT 'paper',
    net_quantity    INTEGER NOT NULL,
    average_price   REAL NOT NULL DEFAULT 0,
    last_price      REAL NOT NULL DEFAULT 0,
    pnl             REAL NOT NULL DEFAULT 0,
    updated         TEXT NOT NULL,
    PRIMARY KEY (strategy_id, trading_symbol, product, book)
);

INSERT OR IGNORE INTO positions_new (
    strategy_id, exchange, trading_symbol, product, book,
    net_quantity, average_price, last_price, pnl, updated
)
SELECT strategy_id, exchange, trading_symbol, product, 'paper',
       net_quantity, average_price, last_price, pnl, updated
FROM positions;

DROP TABLE positions;
ALTER TABLE positions_new RENAME TO positions;

CREATE INDEX IF NOT EXISTS idx_positions_book ON positions(book);
