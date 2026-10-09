-- Migration 021: what a running job is waiting for (spec §14 step 52,
-- §20.1), e.g. "waiting for repository lock on b2 (held by immich)", so
-- a stalled run says why in `ballast status` and the web UI. NULL when
-- the run is not waiting.
ALTER TABLE runs ADD COLUMN waiting TEXT;
