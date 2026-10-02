-- Migration 014: compose folders to look in per Docker host (spec §18.4,
-- §14 step 34).
--
-- Running stacks are found from container labels and need no entry here.
-- A row names a folder on the host: kind 'project' is one stack's own
-- folder (a stack that was never started), kind 'scan' is a parent folder
-- whose sub-folders are each checked for a stack that is defined but not
-- running. Paths only — nothing secret.
CREATE TABLE IF NOT EXISTS compose_dirs (
    docker_host TEXT NOT NULL,
    path        TEXT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('project', 'scan')),
    created_at  TEXT NOT NULL,
    PRIMARY KEY (docker_host, path)
);
