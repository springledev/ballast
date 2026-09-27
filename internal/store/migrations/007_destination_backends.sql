-- Migration 007: backend-typed destinations (spec §14 step 17, §17.1).
--
-- A destination now has a backend type (local, b2, s3, r2, sftp,
-- custom) and its non-secret settings (settings_json: bucket,
-- endpoint, host, ...). `location` stays, but is now the restic
-- repository string Ballast generates from type + settings.
--
-- backend_credential is the *name* of the root-only credential file
-- holding the backend's secrets (an access key pair or an SSH key) —
-- never the secrets themselves (spec rule 2), exactly like
-- password_credential. Empty for a type that needs none.
--
-- Existing destinations were local-only in practice (v0.1 never wired
-- backend credentials): an absolute path becomes a `local`
-- destination; anything else is kept working unchanged as a `custom`
-- raw restic repository string.
ALTER TABLE destinations ADD COLUMN type TEXT NOT NULL DEFAULT 'local';
ALTER TABLE destinations ADD COLUMN settings_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE destinations ADD COLUMN backend_credential TEXT NOT NULL DEFAULT '';

UPDATE destinations SET type = 'local', settings_json = json_object('path', location)
    WHERE location LIKE '/%';
UPDATE destinations SET type = 'custom', settings_json = json_object('url', location)
    WHERE location NOT LIKE '/%';
