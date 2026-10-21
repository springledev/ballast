-- Migration 024: remember which scheduled firing an alert was for (spec §21.1).
-- Rolling back: safe. The column only holds a timestamp; older versions ignore it and fall back to the plain 24-hour cool-down.
ALTER TABLE alert_state ADD COLUMN last_window TEXT NOT NULL DEFAULT '';
