-- Migration 013: email/webhook channels, event routing and alert
-- de-duplication (spec §18.1, §14 step 31).
--
-- settings_json holds a channel's non-secret settings (an email
-- channel's server, port, security, sender and recipients). Secrets
-- still live only in credential files. events and tiers are
-- comma-separated filters; empty means "everything".
ALTER TABLE notification_channels ADD COLUMN settings_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE notification_channels ADD COLUMN events TEXT NOT NULL DEFAULT '';
ALTER TABLE notification_channels ADD COLUMN tiers TEXT NOT NULL DEFAULT '';

-- alert_state remembers which services are currently being alerted
-- about, so a service that fails every night sends one alert plus a
-- reminder per cool-down, and a return to green sends one recovery.
CREATE TABLE IF NOT EXISTS alert_state (
    service       TEXT PRIMARY KEY,
    last_alert_at TEXT NOT NULL
);
