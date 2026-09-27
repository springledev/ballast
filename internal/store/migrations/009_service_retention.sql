-- Migration 009: per-service retention (spec §14 step 19).
--
-- A service may override its destination's keep_daily/keep_weekly/
-- keep_monthly policy. retention_json is '' when the service follows
-- its destination (every existing service), otherwise a JSON object
-- {"keep_daily":n,"keep_weekly":n,"keep_monthly":n}.
ALTER TABLE services ADD COLUMN retention_json TEXT NOT NULL DEFAULT '';
