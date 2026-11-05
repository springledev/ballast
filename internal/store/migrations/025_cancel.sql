-- Migration 025: remember when someone asked a running backup to stop (spec §22.6).
-- Rolling back: safe. The column only holds a timestamp; older versions ignore it, so a stop asked for just before a downgrade is simply not acted on.
ALTER TABLE runs ADD COLUMN cancel_requested_at TEXT;
