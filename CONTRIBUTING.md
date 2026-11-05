# Contributing to Ballast

Thanks for helping. Ballast is alpha software and small, focused changes are the easiest to review.

## Branches and commits

- Name branches `feature/<topic>` or `fix/<topic>`.
- Use conventional commit messages: `feat:`, `fix:`, `docs:`, `test:`, `chore:`, `ci:`.
- One change per pull request. Do not mix unrelated work.

## Build and test

You need Go 1.25. Before opening a pull request, all of these must pass:

```bash
go build ./...
go vet ./...
go test ./... -race
```

If you touch the web UI (needs Node and npm):

```bash
cd web
npm ci
npm test
npm run build
```

Tests are required for new behaviour and bug fixes. The Go tests are table-driven, with one test file per source file.

## The web build is committed

The web UI is embedded into the binary from `web/dist`, which is committed. When you change anything under `web/src`, rebuild with `make web` and commit the updated `web/dist` in the same pull request. CI fails if `web/dist` is out of date.

## Adding a service

Services are YAML entries in `catalog/`, not Go code. See [docs/catalog-contributing.md](docs/catalog-contributing.md).

## Rules worth knowing

- A backup is never marked successful until it has been verified.
- Secrets must never appear in the database, logs or API responses.
- Wrap the real tools (`restic`, `docker`, `pg_dump`); do not reimplement them.
- Write user-facing text in plain English. Many users are new to servers.

## Shared packages

The `secret`, `runner`, `dockerhost` and `sshkey` packages live in a separate repository, [keel](https://github.com/springledev/keel), and are pinned by version in `go.mod`. Fix those packages in keel, not in this repository.

## Security

Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md).
