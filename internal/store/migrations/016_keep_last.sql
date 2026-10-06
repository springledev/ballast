-- Migration 016: "keep the newest N" retention (spec §19.3, §14 step 43).
--
-- keep_last = 0 means the rule is off, so existing destinations keep
-- their daily/weekly/monthly policy unchanged.
ALTER TABLE destinations ADD COLUMN keep_last INTEGER NOT NULL DEFAULT 0;

-- Per-service "skip these" patterns (spec §19.3), a JSON array of
-- strings; '' means none.
ALTER TABLE services ADD COLUMN excludes_json TEXT NOT NULL DEFAULT '';
