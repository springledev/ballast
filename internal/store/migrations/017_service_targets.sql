-- Migration 017: destination targets (spec §14 step 44, §19.4).
-- Rolling back: safe to leave in place; services.destination still holds each service's first destination, so an older Ballast keeps working with its first target only. Extra targets are ignored.
--
-- A service sends its backups to one or more destinations, each a
-- "target" with its own choice of parts and optional retention override.
-- Every existing service becomes one target with all parts (parts_json
-- '' = all parts) carrying the service's own retention override, so
-- nothing about an existing setup changes. services.destination stays as
-- the first target's destination so older code paths keep working.
CREATE TABLE IF NOT EXISTS service_targets (
    service        TEXT NOT NULL REFERENCES services(name) ON DELETE CASCADE,
    position       INTEGER NOT NULL,
    destination    TEXT NOT NULL REFERENCES destinations(name),
    parts_json     TEXT NOT NULL DEFAULT '',
    retention_json TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (service, position)
);

INSERT INTO service_targets (service, position, destination, parts_json, retention_json)
    SELECT name, 0, destination, '', retention_json FROM services;

-- One row per target of a run: its own outcome, snapshot and detail, so a
-- failure of one target never hides another's result.
CREATE TABLE IF NOT EXISTS run_targets (
    run_id      INTEGER NOT NULL REFERENCES runs(id),
    destination TEXT NOT NULL,
    state       TEXT NOT NULL,
    snapshot_id TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, destination)
);

-- Which target a verification result belongs to ('' = the run as a
-- whole, e.g. a restore drill that runs once per artifact).
ALTER TABLE verification_results ADD COLUMN target TEXT NOT NULL DEFAULT '';
