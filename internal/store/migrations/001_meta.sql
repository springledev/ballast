-- Migration 001: bootstrap schema.
-- Rolling back: not applicable; this is the base every later migration builds on.
--
-- Kept deliberately minimal: core job/run tables land with the
-- job state machine step (spec §14 step 2, tracked as PLAN step 4)
-- in later migrations. This migration creates the key/value meta
-- table used for housekeeping state and provides a stable base for
-- the migration machinery itself.
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
