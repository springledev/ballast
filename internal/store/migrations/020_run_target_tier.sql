-- Migration 020: which tier a run's destination outcome belongs to
-- Rolling back: safe to leave in place; an older Ballast ignores the added column(s), which have a default.
-- (spec §14 step 49, §19.8). 'primary' is a restic backup destination;
-- 'secondary' is a report-only second copy (Proton Drive), whose outcome
-- is shown on its own and never changes the run's own state.
ALTER TABLE run_targets ADD COLUMN tier TEXT NOT NULL DEFAULT 'primary';
