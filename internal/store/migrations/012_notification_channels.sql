-- Migration 012: reusable notification channels (spec §18.1, §14 step 30).
-- Rolling back: safe to drop the new table(s); nothing else depends on them. (Channels would be lost; the notification credential files stay.)
--
-- A channel is one delivery target (an ntfy server + topic) that many
-- services can share. Like every other secret (spec rule 2), the
-- channel's URL (an ntfy topic is effectively a password) and its
-- optional access token are NOT stored here: url_credential and
-- token_credential only name the root-only credential files that hold
-- them. username is not secret and is stored as-is.
--
-- apply_to_all channels deliver for every service; service_channels
-- links the rest to the services that use them.
CREATE TABLE IF NOT EXISTS notification_channels (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT NOT NULL UNIQUE,
    type             TEXT NOT NULL,
    url_credential   TEXT NOT NULL,
    username         TEXT NOT NULL DEFAULT '',
    token_credential TEXT NOT NULL DEFAULT '',
    apply_to_all     INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS service_channels (
    service    TEXT NOT NULL,
    channel_id INTEGER NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    PRIMARY KEY (service, channel_id)
);
