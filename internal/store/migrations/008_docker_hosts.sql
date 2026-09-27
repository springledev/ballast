-- Migration 008: Docker hosts (spec §14 step 18, §17.2).
--
-- A Docker host is a daemon Ballast talks to: the local socket, a
-- remote daemon over SSH, or a remote daemon over TCP+TLS. Every
-- Docker operation for a service runs against that service's host.
--
-- Like destinations, this table holds only NON-SECRET configuration
-- (spec rule 2): `credential` is the *name* of the root-only
-- credential file(s) holding an SSH key or TLS client certificates,
-- never their contents.
--
-- The built-in "local" host always exists, and every existing service
-- is attached to it — which is exactly how they ran before. services.
-- docker_host has no REFERENCES clause because SQLite cannot add one
-- with a non-NULL default; internal/inventory enforces it instead.
CREATE TABLE IF NOT EXISTS docker_hosts (
    name       TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    address    TEXT NOT NULL DEFAULT '',
    credential TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

INSERT OR IGNORE INTO docker_hosts (name, kind, address, credential, created_at, updated_at)
    VALUES ('local', 'local', '', '', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

ALTER TABLE services ADD COLUMN docker_host TEXT NOT NULL DEFAULT 'local';
