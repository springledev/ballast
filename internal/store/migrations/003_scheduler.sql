-- Migration 003: scheduler and external-trigger tables (spec §9, §14
-- Rolling back: not applicable; dropping these loses every schedule and trigger token. Restore the pre-upgrade ballast.db copy instead.
-- step 9).
--
-- `schedules` holds the per-service cron expression the built-in
-- scheduler sweeps (`ballast run-due`). It is keyed on service name
-- directly, not a foreign key to `jobs`, because a schedule is
-- normally configured before a service has ever run (no jobs row
-- exists yet).
--
-- `last_fired_at` records the most recent firing the scheduler acted
-- on. It is tracked separately from `runs` so that manual and
-- external-trigger runs never shift the schedule, and a job whose run
-- fails before it is even recorded is not retried every minute.
--
-- `trigger_tokens` holds a SHA-256 hash of each service's external
-- trigger token (spec §12, `ballast trigger --token`) — never the
-- token itself (spec rule 2: secrets never appear in the DB).

CREATE TABLE IF NOT EXISTS schedules (
    service       TEXT PRIMARY KEY,
    cron_expr     TEXT NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    updated_at    TEXT NOT NULL,
    last_fired_at TEXT
);

CREATE TABLE IF NOT EXISTS trigger_tokens (
    service    TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);
