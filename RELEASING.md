# Releasing Ballast

The maintainer does every push and tag. Pushing a version tag to GitHub does
the rest: [the release workflow](.github/workflows/release.yml) runs the full
CI suite, then builds linux amd64 and arm64 archives (binary, LICENSE,
README.md, `deploy/ballast.service`), a `checksums.txt` and a changelog from the
conventional commit messages, and publishes them as a GitHub release. You do not
need goreleaser installed.

## Making a release

1. Make sure everything you want to ship is merged to `main` and that CI is
   green. The workflow refuses a tag that is not on `main`.
2. Update your local copy and tag it:

   ```bash
   git switch main
   git pull
   git tag -a vX.Y.Z -m "Ballast vX.Y.Z"
   git push origin vX.Y.Z
   ```

3. Watch it under the repository's **Actions** tab. When it finishes, the
   release is on the **Releases** page and is what the "latest" download links
   in the install instructions point at.

If the workflow fails, fix the problem on `main`, then move the tag to the fixed
commit. A failure in the CI or tag check steps publishes nothing. If the publish
step itself fails, look on the **Releases** page and delete any partial release
for that version first. Only move a tag for a version nobody could have
downloaded:

```bash
git tag -d vX.Y.Z
git push origin :refs/tags/vX.Y.Z
```

Then tag and push again.

## Before you tag

**If keel changed.** Ballast's `go.mod` must only point at a keel tag that
already exists on GitHub, because a fresh clone has to build without keel checked
out (never add a `replace` line).

1. In keel: commit, then `git tag vX.Y.Z` and push the commit and the tag.
2. In Ballast: `go get github.com/springledev/keel@vX.Y.Z`, `go mod tidy`, then
   run `go build ./... && go vet ./... && go test ./... -race` and commit
   (`chore: bump keel to vX.Y.Z`).

**If `web/src` changed.** `make web` rebuilds `web/dist`, which is committed and
embedded in the binary, so commit the result. CI fails if you forget.

## Running goreleaser yourself

You should not normally need this. A dry run builds everything into `dist/` and
publishes nothing. It needs [goreleaser](https://goreleaser.com) v2:

```bash
GOWORK=off goreleaser release --snapshot --clean
```

`GOWORK=off` matters if you have a local `go.work`: without it the build uses
your keel checkout instead of the version pinned in `go.mod`.

## Container image

The image is not published yet. To build it yourself: `docker build --build-arg VERSION=vX.Y.Z -t ballast:vX.Y.Z .`
The image runs as root and needs `/etc/ballast` and `/var/lib/ballast` as
volumes plus the Docker socket (`-v /var/run/docker.sock:/var/run/docker.sock`)
so it can manage containers. Use the same host folders you would use with the
systemd unit, never a throwaway copy of a real install.
