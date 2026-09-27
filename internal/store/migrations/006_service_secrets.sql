-- Migration 006: catalog secret references (spec §14 step 15).
--
-- secrets_json maps a catalog {{ .Secrets.NAME }} name to the *name*
-- of the root-only credential file holding its plaintext value (spec
-- rule 2: secrets never appear in the DB) — exactly like
-- destinations.password_credential already does for a repository
-- password. The value itself is read from that file at run time via
-- internal/runner.LoadCredentialValue.
ALTER TABLE services ADD COLUMN secrets_json TEXT NOT NULL DEFAULT '{}';
