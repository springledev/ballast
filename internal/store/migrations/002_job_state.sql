-- Migration 002: job state machine tables (spec §4.1, §14 step 2).
-- Rolling back: not applicable; run history lives here, so dropping it loses every past run. Restore the pre-upgrade ballast.db copy instead.
--
-- Every backup run is persisted as a `run` belonging to a `job` (one
-- job per service). Each legal transition is recorded in
-- run_transitions with a timestamp and a structured detail line, so
-- the full history of a run can be replayed from the DB.

CREATE TABLE IF NOT EXISTS jobs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    service    TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id      INTEGER NOT NULL REFERENCES jobs(id),
    state       TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    error       TEXT
);

CREATE TABLE IF NOT EXISTS run_transitions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      INTEGER NOT NULL REFERENCES runs(id),
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    at          TEXT NOT NULL,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    detail      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_runs_job ON runs(job_id);
CREATE INDEX IF NOT EXISTS idx_transitions_run ON run_transitions(run_id);
