-- Migration 015: weekly digest opt-in (spec §18.5, §14 step 40).
-- Rolling back: safe to leave in place; an older Ballast ignores the added column(s), which have a default.
--
-- digest = 1 means the channel also receives the weekly one-message
-- summary. It is opt-in, so existing channels stay as they were.
ALTER TABLE notification_channels ADD COLUMN digest INTEGER NOT NULL DEFAULT 0;
