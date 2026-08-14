-- Operator settings changed at runtime and expected to survive a restart.
--
-- Deliberately a generic key/value table rather than a column per setting: the
-- things that belong here (risk limits now, halt state and strategy instances
-- later) are all small, read once at start-up, and written rarely. A new setting
-- should not require a schema migration.
--
-- Values are JSON so a setting can grow fields without changing this table.
--
-- Precedence is config.yaml -> this table: the config supplies defaults, a row
-- here overrides them, and deleting the row restores the default. That way an
-- operator can always get back to a known state by clearing the override rather
-- than remembering what the numbers used to be.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
