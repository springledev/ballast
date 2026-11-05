# Releasing Ballast

The maintainer does every push and tag. Order matters: Ballast's `go.mod`
must only point at a keel tag that already exists on GitHub, because a fresh
clone has to build without keel checked out (never add a `replace` line).

1. **keel**: commit, then `git tag vX.Y.Z` and push the commit and the tag.
2. **Ballast, bump keel**: `go get github.com/springledev/keel@vX.Y.Z`,
   `go mod tidy`, then run `go build ./... && go vet ./... && go test ./... -race`
   and commit (`chore: bump keel to vX.Y.Z`).
3. **Web UI** (only if `web/src` changed): `make web` rebuilds `web/dist`.
   `web/dist` is committed and embedded in the binary, so commit the result.
4. **Tag Ballast**: `git tag -a vX.Y.Z -m "Ballast vX.Y.Z"`, then push the
   branch and the tag. Check the version first with
   `make build VERSION=vX.Y.Z && bin/ballast version`.
5. **Release**: with [goreleaser](https://goreleaser.com) v2 installed, run
   `goreleaser release --clean` from the tagged commit with `GITHUB_TOKEN`
   set. It builds linux amd64 and arm64 archives (binary, LICENSE, README.md,
   `deploy/ballast.service`), a `checksums.txt`, and a changelog from the
   conventional commit messages, and publishes them as a GitHub release.

Dry run without publishing anything:

```
goreleaser release --snapshot --clean
```

Container image (optional): `docker build --build-arg VERSION=vX.Y.Z -t ballast:vX.Y.Z .`
The image runs as root and needs `/etc/ballast` and `/var/lib/ballast` as
volumes plus the Docker socket (`-v /var/run/docker.sock:/var/run/docker.sock`)
so it can manage containers. Use the same host folders you would use with the
systemd unit, never a throwaway copy of a real install.
