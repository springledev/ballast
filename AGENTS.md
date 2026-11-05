# AGENTS.md — Ballast



 Ballast is a self-hosted, service-aware backup tool for homelab Docker

 containers. **`spec.md` in this repo root is the single source of truth.**

 Read it before doing anything. If code and spec disagree, the spec wins —

 flag the conflict, don't silently pick one.



 ## Product in one paragraph



 Ballast backs up Docker-hosted services (Immich, Vaultwarden, generic

 Postgres/MySQL, any folder) using per-service strategies from a YAML

 catalog, ships them via **restic** to local/B2/S3/SFTP destinations,

 **verifies every backup is restorable** (throwaway-container DB restores,

 archive integrity, restic read-backs), and reports per-service

 traffic-light status via ntfy/webhooks. Engine is Go; a Vue web UI is a

 thin layer over the same API the CLI uses. Phones are Phase 2 — do not

 build mobile features, but do not break the two reserved interfaces

 (`exec` strategy, `ballast trigger --token`).



 ## Stack (fixed — do not substitute)



 - Go 1.25, single static binary, no cgo (`CGO_ENABLED=0`)

 - SQLite via `modernc.org/sqlite` (pure Go)

 - Cobra for CLI, chi or stdlib mux for HTTP, in-Go scheduler plus

 `ballast run-due` for systemd/cron

 - Vue 3  TypeScript frontend embedded with `go:embed`

 - Tests: standard `testing`  testcontainers-go where Docker is needed



 ## Non-negotiable rules



 1. **A backup is not done until verified.** No code path may mark a job

 SUCCESS without passing the VERIFYING phase. Unverifiable = amber.

 2. **Secrets never appear in the DB, logs, or API responses.** They live

 in root-only files / systemd credentials (`LoadCredential=`).

 3. **Isolation of failure.** A secondary-tier (rclone) failure must never

 mask a primary-tier (restic) failure, and vice versa. Report all tiers.

 v0.1 ships the restic tier only; rclone is Phase 2 (spec §12), but keep

 per-tier reporting shapes so it can slot in without masking anything.

 4. **Wrap, don't rebuild.** Shell out to `restic`, `rclone`, `docker`,

 `pg_dump`. Never reimplement their logic.

 5. **Catalog is data.** New services are YAML entries, not Go code.

 6. **No mobile code in v0.1.** Test the reserved interfaces with stubs only.

 Phone *backup* is Phase 2; a mobile-friendly web UI is in scope (spec §20.5).



 ## Working conventions



 - Table-driven tests, one test file per source file (`foo_test.go`).

 - Every exported thing has doc comments; user-facing strings are plain

 English (audience includes homelab beginners).

 - Errors: wrap with `%w`, always include the service/job context.

 - Subprocess calls: use `pipefail`-equivalent semantics and handle SIGPIPE

 explicitly (read spec §8 lessons before writing any exec code).

 - Commits: conventional style (`feat:`, `fix:`, `test:`, `docs:`).

 - `go build ./...`, `go vet ./...`, and `go test ./... -race` must be

 green before considering any work finished.



 ## Repo layout (target)



 ```

 cmd/ballast/ main

 internal/engine/ job state machine, scheduler

 internal/runner/ subprocess exec (pipefail, SIGPIPE, credential env)

 internal/catalog/ YAML loading  JSON schema validation

 internal/strategy/ dump, stopstart, exec, folder plugins

 internal/verify/ verification runners

 internal/notify/ ntfy, webhook

 internal/store/ SQLite  migrations

 internal/api/ REST API (CLI and web UI share it)

 web/ Vue 3 frontend

 catalog/ built-in service YAML definitions

 docs/ user docs (beginner-first tone)

 ```



 ## Workflow



 Work strictly in the order of spec §14, one step per session. Start each

 session with the `/implement` command. Track progress in the spec.md §14 checkboxes (each step gets `[x]` and a one-line summary when done) and in PR descriptions. When you believe a

 feature is done, run `/verify-feature` before moving on. Never batch

 multiple §14 steps into one change.

 ## Shared code

 `secret`, `runner`, `dockerhost` and `sshkey` live in the `keel` repo
 (`github.com/springledev/keel`), pinned to a tagged version in go.mod.
 Fix those packages in keel, never copy them back into `internal/`.
 To edit keel and Ballast together, use a local, gitignored workspace
 (`go work init . ../keel`). Before pushing Ballast code that needs a
 keel change: commit and tag keel, push the tag, then
 `go get github.com/springledev/keel@<tag>`. Never add a `replace`
 directive — a fresh clone must build without keel checked out.
