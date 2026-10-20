-- Migration 018: Proxmox guest jobs (spec §14 step 46, §19.6).
-- Rolling back: cannot be undone in place; the services table was rebuilt. Restore the pre-upgrade ballast.db copy instead.
--
-- A Proxmox guest backup is a service of kind 'proxmox-guest': it runs
-- through the same engine as every other service. Such a service has
-- no restic destination unless the operator also copies the dump to
-- one, so services.destination must be allowed to be empty. SQLite
-- cannot relax NOT NULL/REFERENCES in place, so the table is rebuilt.
-- service_targets (which cascades from services) is parked in a temp
-- table first so the rebuild cannot delete it.
CREATE TEMP TABLE parked_targets AS SELECT * FROM service_targets;

CREATE TABLE services_new (
    name            TEXT PRIMARY KEY,
    catalog_entry   TEXT NOT NULL,
    destination     TEXT NOT NULL DEFAULT '',
    params_json     TEXT NOT NULL DEFAULT '{}',
    containers_json TEXT NOT NULL DEFAULT '{}',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    secrets_json    TEXT NOT NULL DEFAULT '{}',
    retention_json  TEXT NOT NULL DEFAULT '',
    docker_host     TEXT NOT NULL DEFAULT 'local',
    project         TEXT NOT NULL DEFAULT '',
    proxmox_node    TEXT NOT NULL DEFAULT '',
    proxmox_vmid    INTEGER NOT NULL DEFAULT 0,
    excludes_json   TEXT NOT NULL DEFAULT '',
    kind            TEXT NOT NULL DEFAULT '',
    guest_json      TEXT NOT NULL DEFAULT ''
);

INSERT INTO services_new (name, catalog_entry, destination, params_json, containers_json, created_at, updated_at,
                          secrets_json, retention_json, docker_host, project, proxmox_node, proxmox_vmid, excludes_json)
    SELECT name, catalog_entry, destination, params_json, containers_json, created_at, updated_at,
           secrets_json, retention_json, docker_host, project, proxmox_node, proxmox_vmid, excludes_json
    FROM services;

DROP TABLE services;
ALTER TABLE services_new RENAME TO services;

INSERT INTO service_targets SELECT * FROM parked_targets;
DROP TABLE parked_targets;

-- Every dump volume a Proxmox guest job created. Keep-last pruning only
-- ever deletes volumes listed here (decision D7: Ballast never deletes
-- what it did not create).
CREATE TABLE proxmox_dumps (
    volume     TEXT NOT NULL,
    service    TEXT NOT NULL,
    node       TEXT NOT NULL,
    storage    TEXT NOT NULL,
    run_id     INTEGER NOT NULL DEFAULT 0,
    size       INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    pruned_at  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (node, volume)
);
