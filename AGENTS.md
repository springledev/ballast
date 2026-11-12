# AGENTS.md — Ballast

Ballast is a self-hosted, service-aware backup tool for bare-metal, Docker and
Proxmox servers. It backs up services using per-service strategies from a YAML
catalog, ships them with **restic** (whole Proxmox guests use `vzdump`),
**verifies every backup is restorable**, and reports per-service traffic-light
status. The engine is Go; the Vue web UI is a thin layer over the same API the
CLI uses.

## Spec

`spec.md` is the single source of truth. If code and spec disagree, the spec
wins: flag the conflict, never silently pick one.

It is about 23k tokens, so **do not read it whole**. Run
`grep -n '^ ##' spec.md` for the outline and read only the sections your task
cites, by line range. Always relevant: §3 (principles), §4.1 (job state
machine), §8 (subprocess lessons, required before writing any exec code).

## Stack (fixed, do not substitute)

- Go 1.27, single static binary, `CGO_ENABLED=0`
- SQLite via `modernc.org/sqlite`
- Cobra CLI, chi or stdlib mux for HTTP, in-Go scheduler plus `ballast run-due`
- Vue 3 + TypeScript in `web/`, embedded with `go:embed` from the committed `web/dist`
- Tests: standard `testing`, testcontainers-go where Docker is needed

## Rules

1. **A backup is not done until verified.** No code path marks a job SUCCESS
   without passing VERIFYING. Unverifiable = amber.
2. **Secrets never appear in the DB, logs, or API responses.** They live in
   root-only files or systemd credentials (`LoadCredential=`).
3. **Failures stay isolated.** One tier's failure never masks another's; report
   every tier. Only the restic tier ships today (rclone is Phase 2, spec §12),
   so keep the per-tier reporting shapes.
4. **Wrap, don't rebuild.** Shell out to `restic`, `rclone`, `docker`,
   `pg_dump`, `vzdump`.
5. **Catalog is data.** New services are YAML entries in `catalog/`, not Go code.
6. **No phone backup code** (Phase 2). Do not break the two reserved interfaces
   (`exec` strategy, `ballast trigger --token`); test them with stubs only. A
   mobile-friendly web UI is in scope (spec §20.5).

## Conventions

- Table-driven tests, one test file per source file (`foo_test.go`).
- Findings tagged Inferred or Expected get a failing test before the fix.
- Doc comments on everything exported.
- User-facing text is plain English for beginners, with no spec or plan references.
- Errors: wrap with `%w` and include the service/job context.
- Subprocesses: pipefail-equivalent semantics, explicit SIGPIPE handling (spec §8).
- Conventional commits (`feat:`, `fix:`, `test:`, `docs:`, `chore:`) on
  `feature/<topic>` or `fix/<topic>` branches, never on `main`.
- Changing `web/src` means running `make web` and committing `web/dist` in the
  same pull request. CI fails otherwise.

## Tests: run the cheapest thing that proves the change

- While iterating: `go test ./internal/<pkg>/ -run <TestName>` on the packages you touched.
- Step finished: `go build ./... && go vet ./... && go test -short ./...`
  (`-short` skips the Docker-backed tests; unchanged packages come from the test cache).
- Before a pull request, once: `go test ./... -race`, plus `npm test` in `web/`
  if `web/src` changed. All must be green.
- Pipe long output through `tail`; report failures only, not passing output.

## Workflow

- Work follows the numbered steps in spec §14. One step is one commit; never
  mix steps in a commit and never skip ahead.
- When a step lands, mark it in §14 with `[x]` and a one-line summary.
- `/implement` does the next single step. `/run-plan` runs several steps with
  one subagent per step. `/verify-feature` checks a finished feature against
  its acceptance checklist.
- `PLAN-*.md` files are the "why". Their steps are not adopted until they are
  copied into spec §14.

## Shared code (keel)

`secret`, `runner`, `dockerhost` and `sshkey` live in
`github.com/springledev/keel`, pinned to a tag in `go.mod`. Fix them in keel,
never copy them into `internal/`. To edit both together use a local, gitignored
workspace (`go work init . ../keel`). Before pushing Ballast code that needs a
keel change: commit and tag keel, push the tag, then
`go get github.com/springledev/keel@<tag>`. Never add a `replace` directive to
`go.mod`; a fresh clone must build without keel checked out.

## Layout

`cmd/ballast/` main · `internal/` Go packages (`ls internal/`) · `web/` Vue UI ·
`catalog/` built-in service YAML · `docs/` user docs · `deploy/` install units
