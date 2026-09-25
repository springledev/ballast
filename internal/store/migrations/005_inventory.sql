-- Migration 005: configured services and destinations (spec §14 step
-- 11), plus first-class verification records and the run's snapshot
-- ID.
--
-- `destinations` and `services` hold only NON-SECRET configuration
-- (spec rule 2: secrets never appear in the DB). A destination's
-- repository password lives in a root-only credential file; this
-- table stores only that file's *name* (password_credential), never
-- the password itself, exactly like the trigger-token and
-- notification credential files already do. Similarly, `location` is
-- the restic repository URL or path with no embedded credentials
-- (internal/inventory rejects a location containing "://user:pass@"
-- userinfo).
--
-- `services` ties a configured service name to its catalog entry, the
-- destination it backs up to, and its resolved template values
-- (params_json / containers_json — the {{ .Params.* }} / {{
-- .Services.* }} values the catalog templates substitute). These are
-- non-secret by construction: secret values belong in credential
-- files and are referenced by environment variable, never stored
-- here.
--
-- `verification_results` is the first-class per-check record of spec
-- §7: one row per Checker result for a run's VERIFYING phase, so
-- `ballast status --verify` and the API can show every check's
-- pass/fail detail, not just the run's overall outcome.
--
-- `runs.snapshot_id` records the restic snapshot a run produced, so
-- restore and read-back operations can find it directly instead of
-- re-deriving it from tags.
CREATE TABLE IF NOT EXISTS destinations (
    name                TEXT PRIMARY KEY,
    location            TEXT NOT NULL,
    password_credential TEXT NOT NULL,
    keep_daily   INTEGER NOT NULL DEFAULT 7,
    keep_weekly  INTEGER NOT NULL DEFAULT 4,
    keep_monthly INTEGER NOT NULL DEFAULT 6,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS services (
    name          TEXT PRIMARY KEY,
    catalog_entry TEXT NOT NULL,
    destination   TEXT NOT NULL REFERENCES destinations(name),
    params_json     TEXT NOT NULL DEFAULT '{}',
    containers_json TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS verification_results (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES runs(id),
    method TEXT NOT NULL,
    passed INTEGER NOT NULL,
    warning TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    output TEXT NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0,
    recorded_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_verification_run ON verification_results(run_id);

ALTER TABLE runs ADD COLUMN snapshot_id TEXT;
