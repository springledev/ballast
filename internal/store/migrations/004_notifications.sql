-- Migration 004: notification settings (spec §11, §14 step 10).
--
-- Per-service notification configuration: freshness max-age for alerting.
-- Channel-specific secrets (ntfy_topic, webhook_url) are read from
-- credential files at runtime, never stored in DB (spec rule 2).
--
-- max_age_hours is nullable: NULL means "no explicit override", and
-- the freshness sweep derives the default from the service's schedule
-- instead (notify.DefaultMaxAge — nightly 48h, weekly 9d, spec §11).
--
-- heartbeat_enabled is the per-service opt-in for a success
-- notification (spec §11 "success heartbeat"); it defaults to off so
-- a fresh install is not noisy.

CREATE TABLE IF NOT EXISTS notification_settings (
    service           TEXT PRIMARY KEY,
    enabled           INTEGER NOT NULL DEFAULT 1,
    max_age_hours     INTEGER,
    heartbeat_enabled INTEGER NOT NULL DEFAULT 0,
    updated_at        TEXT NOT NULL
);

-- Tracks the result of the last freshness check sweep, to avoid
-- repeating notifications for the same breach; cleared when fresh
-- again. last_checked_at is updated on every check for the service,
-- whether or not it is stale, so it always reflects when the sweep
-- last looked at this service.
CREATE TABLE IF NOT EXISTS freshness_alerts (
    service         TEXT PRIMARY KEY,
    last_checked_at TEXT NOT NULL,
    is_stale        INTEGER NOT NULL DEFAULT 0,
    reported_at     TEXT
);
