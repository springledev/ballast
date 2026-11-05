-- Migration 026: remember signed-in browsers across a restart (spec §22.9). Only a SHA-256 hash of each session token is stored, never the token itself.
-- Rolling back: safe. Older versions ignore the table, so browsers are simply signed out again on their next restart; the table can be dropped by hand.
CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX sessions_expires_at ON sessions (expires_at);
