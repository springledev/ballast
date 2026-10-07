-- Migration 019: repository health (spec §14 step 48, §19.7).
--
-- One row per destination that has a health setting or a result. A
-- destination with no row uses the defaults (weekly, 5% of data read
-- back). Holds no secrets: only schedule, last outcome and a plain
-- English explanation.
CREATE TABLE IF NOT EXISTS destination_health (
    destination     TEXT PRIMARY KEY REFERENCES destinations(name) ON DELETE CASCADE,
    enabled         INTEGER NOT NULL DEFAULT 1,
    interval_hours  INTEGER NOT NULL DEFAULT 168,
    subset          TEXT NOT NULL DEFAULT '5%',
    state           TEXT NOT NULL DEFAULT 'unchecked',
    detail          TEXT NOT NULL DEFAULT '',
    last_checked_at TEXT NOT NULL DEFAULT '',
    last_ok_at      TEXT NOT NULL DEFAULT ''
);
