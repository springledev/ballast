-- Migration 023: the last few alert decisions per service (spec §21.1).
-- Holds only time, event type, outcome and a plain-English reason:
-- never a topic, URL or token (rule 2). Rolling back: safe to drop.
CREATE TABLE IF NOT EXISTS alert_decisions (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    service TEXT NOT NULL,
    at      TEXT NOT NULL,
    type    TEXT NOT NULL,
    outcome TEXT NOT NULL,
    reason  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS alert_decisions_service ON alert_decisions(service, id);
