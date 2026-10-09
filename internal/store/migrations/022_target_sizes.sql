-- Migration 022: how much data the last backup of a service sent to a
-- destination (spec §14 step 56, §20.3), so doctor and the preview can
-- project how full each destination disk will get.
CREATE TABLE IF NOT EXISTS target_sizes (
    service     TEXT NOT NULL,
    destination TEXT NOT NULL,
    bytes       INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (service, destination)
);
