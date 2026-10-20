-- Migration 011: Proxmox connection + per-service guest attachment
-- Rolling back: safe to drop the new table(s); nothing else depends on them. The added service columns can stay.
-- (spec §14 step 24, §17.3).
--
-- Unlike Docker hosts, Ballast talks to at most one Proxmox host
-- (spec §17: "one Proxmox host plus its Docker hosts") — so this is a
-- singleton table (id is always 1), holding only NON-SECRET
-- configuration (spec rule 2): `credential` is the name of the
-- root-only credential file holding the API token's secret half,
-- never its value.
CREATE TABLE IF NOT EXISTS proxmox_connection (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    host       TEXT NOT NULL,
    token_id   TEXT NOT NULL,
    credential TEXT NOT NULL DEFAULT '',
    insecure   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- A service can be attached to the Proxmox guest it runs in (spec
-- §17.4): proxmox_node + proxmox_vmid name the guest, proxmox_vmid 0
-- meaning "not attached" (the common case — a service that runs
-- directly in Docker or bare metal, with no Proxmox tier involved).
-- This is inventory only for now: orchestrating vzdump around it is
-- spec §14 step 25.
ALTER TABLE services ADD COLUMN proxmox_node TEXT NOT NULL DEFAULT '';
ALTER TABLE services ADD COLUMN proxmox_vmid INTEGER NOT NULL DEFAULT 0;
