# Adding a service to the catalog

A **catalog entry** is what teaches Ballast how to correctly back up one
kind of app — for example "Immich" or "any PostgreSQL server." It's a plain
YAML file: no Go code, no compiling, no restarting anything except loading
the file. This is deliberate — new services should be able to arrive as
data contributions, reviewable by anyone who can read YAML.

Bundled entries (the ones Ballast ships with) live in [`catalog/`](../catalog/)
at the repository root and are baked into the binary. Your own entries — for
a service not yet bundled, or a variant you want to keep private — go in
`<config dir>/catalog.d/`, which by default is `~/.config/ballast/catalog.d/`
(override the config directory with `$BALLAST_CONFIG_DIR`, or set
`catalog_dirs` in `ballast.yaml`; see [Configuration](configuration.md)).
Ballast loads both bundled and user entries every time it starts.

Every entry is checked against a strict schema
(`internal/catalog/schema/catalog-v1.json`) before Ballast will use it:
**unknown keys are rejected outright**, so a typo fails loudly at load time
instead of silently doing nothing.

## Field reference

```yaml
apiVersion: ballast.dev/v1   # required, must be exactly this
service: my-service          # required; lowercase letters, digits, dashes
version: 1                   # required; integer, starts at 1

display:                     # optional
  name: My Service            # shown in the wizard/UI; falls back to `service`
  description: One line about what this backs up

detect:                      # optional: how the wizard suggests this entry
  docker:
    imagePattern: "myorg/myservice"   # regular expression (case-insensitive) to match a container image
    orEnv:
      MY_SERVICE_VERSION: "*"          # or: match if this env var is set on a container

parameters:                  # optional: values the wizard/CLI ask the user for
  - name: data_dir
    required: true            # missing + required = wizard/service add fails with a clear error
    prompt: "Path to My Service's data folder"
    default: "/srv/myservice/data"   # used when the value is omitted (not just left blank on purpose)
    # picker: docker-volume    # optional UI hint: offer the named volumes on the service's Docker host
    # picker: compose-project  # optional UI hint: offer the Compose projects on the service's Docker host

backup:                      # required
  strategy: dump              # required: one of dump | stop-start | files | exec | volume | compose
  dumps:                      # required when strategy is dump; one entry per thing to dump
    - name: database
      container: "{{ .Params.container }}"   # optional: docker container to `docker exec` into
      command: ["pg_dumpall", "-U", "postgres"]  # required, non-empty
      format: sql              # optional; free text (drives the output file's extension)
      user: git                # optional: run the command as this container user (docker exec -u)
  files:                      # optional (any strategy): plain paths backed up directly via restic
    - path: "{{ .Params.data_dir }}"
  hostPaths:                  # files / stop-start: folders on the service's Docker host (path, or container + mount)
  volumes:                    # required when strategy is volume: named Docker volumes to archive
    - name: "{{ .Params.volume }}"
  stopStart:                  # optional; used by the stop-start and volume strategies
    containers:
      - "{{ .Params.container }}"
  compose:                    # required when strategy is compose: the Compose project to snapshot
    project: "{{ .Params.project }}"
  exec:                       # required when strategy is exec (Phase-2 reserved interface — see below)
    script: "/path/to/script"
    args: ["--flag"]
    verifyScript: "/path/to/verify-script"

verify:                      # required
  method: db-restore-drill     # required (single method): see "Choosing a verification method" below
  methods:                     # or required (multiple methods, all must pass):
    - archive-integrity         #   - archive-integrity
    - restart-smoke             #   - restart-smoke
  image: "postgres:16"        # required when method is db-restore-drill
  filterStderr:                # optional: benign stderr substrings to ignore during db-restore-drill
    - "already exists"
  restartPolicy: never         # optional: never | always (documents whether this strategy restarts containers)
  smoke:                       # optional: required when methods includes restart-smoke
    container: "{{ .Params.container }}" # required, the container to check
    timeout: 2m                # optional, default 2m
    http:                      # optional: HTTP probe (or tcp, not both)
      port: 80                 # required: port inside the container
      path: /                  # optional, default /
      expectStatus: 200        # optional, default 200
      expectStatuses: [200, 401] # optional list; any of these is healthy (overrides expectStatus)
    # or: tcp:
    #   port: 5432

notifications:                # optional
  failureVerbosity: full       # optional: full | brief
  successHeartbeat: true       # optional
```

A few rules worth calling out:

- **Unknown keys are rejected** at every level of the document — the schema
  uses `additionalProperties: false` throughout, so a misspelled field name
  fails to load instead of being silently ignored.
- `backup.strategy: dump` requires at least one entry in `backup.dumps`.
- `backup.strategy: volume` requires at least one entry in `backup.volumes`.
- `backup.strategy: compose` requires `backup.compose.project`.
- `verify.method: db-restore-drill` requires `verify.image`.
- `verify.methods` (a list) declares multiple checks that all must pass; use
  `method` (singular) for a single check.
- Every entry **must** define a `verify` block — there is no way to skip
  verification. That's the whole point of Ballast (see below).

## Templating: `{{ .Params.* }}` and `{{ .Services.* }}`

Catalog files can reference values that aren't known until someone actually
configures the service:

- `{{ .Params.NAME }}` — a value from this entry's `parameters` list (either
  what the user typed, or that parameter's `default` if they left it out).
- `{{ .Services.NAME }}` — a value for a container Ballast detected on its
  own (not yet wired up to the wizard or web UI — see the note in
  `catalog/immich.yaml`; use a parameter instead for now).

This is **string substitution only** — there is no arbitrary expression
language, no conditionals, no function calls. `{{ .Params.data_dir }}` and
`{{ .Services.database }}` are the only forms accepted; anything else inside
`{{ }}` is a load-time error. This is intentional: a catalog entry cannot
execute arbitrary logic, only fill in blanks.

A parameter's `default` is used only when the value is *omitted entirely* —
if the user (or `ballast service add --param name=`) explicitly supplies an
empty string, that empty string wins, not the default.

## Choosing a strategy

| Strategy | What it does | Use it when |
|---|---|---|
| `dump` | Runs a command (optionally via `docker exec` into a named container) and captures its stdout as a backup artifact; the app keeps running. | The app has an export/dump tool that produces a consistent snapshot without stopping (e.g. `pg_dumpall`, `mysqldump --single-transaction`). |
| `stop-start` | Stops the listed containers, backs up `files` paths directly, then always restarts the containers — even if the backup fails or is interrupted. | The app's on-disk data (e.g. a SQLite file, an append-only data folder) isn't safe to copy while the app is writing to it. |
| `volume` | Copies each named Docker volume in `volumes` into a tar archive using a small temporary helper container on the service's Docker host (works the same on remote hosts; no host folders needed). With `stopStart.containers` set, those containers are stopped during the copy and always started again; without it the volume is copied live. | The data lives in a named Docker volume rather than a folder you can point at. Stop the containers for anything that writes constantly (databases); live is fine for media and rarely-changing files. See `catalog/docker-volume.yaml` and `catalog/docker-volume-live.yaml`. |
| `files` | Backs up `files` paths directly, no container involved. | A plain folder, or any data that's already safe to read live. |
| `compose` | Finds the named Docker Compose project by its containers' own Compose labels (never a host path you supply), and archives its config files (`docker-compose.yml` and any override files it was started from) plus `.env`, when present, through a small temporary helper container — works the same on remote hosts. Also captures each container's name, image and labels (never its environment variables — those commonly carry secrets) as a second artifact. | You want the stack's own configuration backed up alongside its data, so a full-stack restore has something to recreate the containers from. Config-only: pair it with `volume`/`dump` entries for the project's actual data. See `catalog/docker-compose.yaml`. |
| `exec` | Runs a user-provided script instead of a built-in strategy. | Your own backup logic. The script writes its files into `$BALLAST_ARTIFACT_DIR` and exits 0. Pair it with `exec.verifyScript` and the `exec-script` method so Ballast can prove the backup restores. The web UI's **Custom services** page has a script template. |

## Choosing a verification method

Four methods are available today: **`archive-integrity`**, **`db-restore-drill`**
(formerly called `container-restore`; the old name still works as an alias),
**`restart-smoke`** and **`exec-script`**. `read-back` passes schema validation
but is not implemented; don't declare it in a catalog entry.

**`exec-script`** runs the entry's `exec.verifyScript` with
`BALLAST_ARTIFACT_DIR` set to the folder holding the backup files; exit 0 means
verified, anything else fails the run. An `exec` entry with no `verifyScript`
cannot be proven, so its light stays amber, never green.

You do not need a terminal to add your own entry: the **Custom services** page
in the web UI offers templates (database dump, stop-and-copy folder, volume,
script), checks your YAML as you type with the same rules as
`ballast catalog validate`, and installs it into your `catalog.d` folder.
Built-in names cannot be replaced from the UI.

You can declare either a single `verify.method` or a list `verify.methods`
where all listed methods must pass. Every backup that reaches the verification
phase runs all of them before reporting a result.

- **`archive-integrity`** — checks that the backed-up archive/artifact is
  structurally intact and readable (the equivalent of `tar -tf` or
  `gzip -t`, depending on format). This is the right (and simplest) choice
  for `stop-start` and `files` entries.
- **`db-restore-drill`** — for `dump` entries with a SQL dump artifact:
  starts a fresh, disposable container of `verify.image`, restores the dump
  into it, and checks the restore produced no unexpected errors, then
  destroys the container. This is the only way to actually prove a database
  dump is restorable, not just present. (Older catalog files may use the
  alias `container-restore` instead; both names work.)
- **`restart-smoke`** — after a backup, checks that the service's container
  is running and healthy. Optionally probes HTTP (default status 200) or TCP
  from inside the container's own network. Configuration goes in `verify.smoke`,
  which specifies the container name and optional probes; see the
  `catalog/vaultwarden.yaml` and `catalog/couchdb.yaml` examples below.

Whichever method you pick, restic's own read-back (a sample read of what
was actually stored, proving the snapshot exists and is fresh) also runs
automatically on every backup — you never need to declare that yourself.
When a `files`-only entry (no dump artifacts) declares `archive-integrity`,
Ballast falls back to this restic read-back to satisfy verification, since
there's no archive artifact of Ballast's own to check.

**Every catalog entry must be verifiable — there is no "just trust it"
option.** A backup whose checks don't pass is reported amber (backed up,
unverified), never green, no matter what the entry declares.

### Database-restore-drill image matching

For `db-restore-drill`, `verify.image` **must be the same image** the
service's own database container runs — not just "a Postgres image." Some
apps ship a customized database image with extensions the stock image
doesn't have: Immich's Postgres needs vector-search extensions
(VectorChord/pgvecto.rs) to restore a real Immich dump at all; restoring
into plain `postgres:16` would fail for the wrong reason (a missing
extension, not a bad dump) and give a false negative. If you can, get the
image tag straight from the service's own `docker-compose.yml`. Ballast
also compares the live source container's image against `verify.image` at
verify time and adds a warning (not a failure) if they differ, so drift
between what you're running and what you're testing against doesn't go
unnoticed.

The restore container is started with **the image's own entrypoint and its
own default command** — Ballast does not override either. Overriding the
entrypoint looks tempting but
skips the image's first-run initialization entirely and the server then
refuses to start; running a bare server binary as the command discards
server arguments the image's entrypoint would have supplied (which is
exactly how Immich's Postgres loads its extensions). Every supported
database image already starts its server correctly via its own default
command, so the restore simply runs the image the way its own stack does.

Anything the restore command prints to stderr is checked **whitelist-only**:
only substrings listed in `verify.filterStderr` are treated as benign
(e.g. `"already exists"`, expected when restoring into a fresh cluster
without `--clean`). Anything else on stderr fails the check — a new,
unrecognized error is never assumed to be harmless.

## Secrets

**Never put a password, API key, or any other secret directly in a catalog
YAML file.** Catalog entries are meant to be shared and reviewed in the
open; a secret belongs in a root-only credential file, not in a file that
might end up in a public PR.

For dump commands that need a database password, the intended approach —
declared in `catalog/mysql.yaml`'s own comments — is that Ballast forwards
secret environment variables into the `docker exec` call by name only
(`docker exec -e MYSQL_PWD ...`), so the value is resolved by Docker from
its own environment and never appears on a command line or in a log.

At the time of writing, the plumbing for this (`strategy.Request.SecretEnv`
in the engine) exists, but nothing yet populates it from a catalog entry, a
service parameter, or a CLI flag — there is no supported way today to
actually get a secret into that environment variable. If your service's
dump command needs a password, either use trust-based local-socket
authentication (as the bundled `postgres` and `immich` entries do) or wait
for that wiring to land before contributing an entry that depends on it. If
in doubt, check the [catalog schema](../internal/catalog/schema/catalog-v1.json)
in this repo, or ask before submitting.

## Worked examples

### `catalog/immich.yaml` — dump + files, db-restore-drill

- **`detect.docker.imagePattern`** and **`orEnv.IMMICH_VERSION`** let the
  wizard spot a running Immich install automatically.
- Three required parameters: `library_dir` (no default — every install's
  path differs), `db_container` (defaults to `immich_postgres`, the
  standard compose name), `db_user` (defaults to `postgres`), and
  `db_image` (defaults to a specific VectorChord-enabled tag, but the
  prompt tells the user to copy their own compose file's value instead).
- `backup.dumps` runs `pg_dumpall -U {{ .Params.db_user }}` inside
  `{{ .Params.db_container }}`; deliberately **no `--clean`**, because a
  restore into a fresh cluster must not try to drop the role it's connected
  as.
- `backup.files` backs up `{{ .Params.library_dir }}` directly — that's the
  actual photo/video files, which restic can read live.
- `verify.method: db-restore-drill` with `verify.image` set to the
  parameterized `db_image`, and `filterStderr: ["already exists"]` to
  tolerate the one expected benign error from skipping `--clean`.
- The database container could in principle be templated as
  `{{ .Services.database }}` (an auto-detected container), but nothing in the wizard or web UI can fill that role
  yet, so this entry asks for `db_container` as an ordinary parameter
  instead, matching how the generic `postgres` entry does it.

### `catalog/vaultwarden.yaml` — stop-start + hostPaths, archive-integrity + restart-smoke

- Vaultwarden keeps its database (SQLite) and everything else in one data
  folder. Copying a live SQLite file risks catching it mid-write, so this
  entry uses `strategy: stop-start` instead of `dump`.
- `backup.stopStart.containers` names the one container to stop
  (`{{ .Params.container }}`, defaulting to `vaultwarden`); `backup.hostPaths`
  names the container's mount point (`mount: /data`); Ballast asks Docker
  which folder on the service's Docker host is mounted there and archives
  it through a read-only helper container while the container is down. A
  `hostPaths` entry is either `path:` (an absolute path on that host) or
  `container:` plus `mount:`. It works the same on remote Docker hosts;
  `backup.files` keeps meaning paths on the Ballast machine, and the two
  cannot be mixed in one entry.
- `verify.methods` declares two checks, both of which must pass:
  - `archive-integrity` — the resulting archive is complete and readable,
    plus the automatic restic read-back.
  - `restart-smoke` — after the backup, the container is running and responds
    on its health endpoint (`/alive` on port 80 inside the container).
- `restartPolicy: always` documents (for humans reading the file) that this
  strategy restarts the container on every run, success or failure — the
  engine itself guarantees the restart regardless of what this field says;
  it's not a switch you can use to skip restarting.

### `catalog/couchdb.yaml` — stop-start + hostPaths, archive-integrity + restart-smoke

- Structurally identical to Vaultwarden's entry: CouchDB's data files are
  append-only, but a copy taken mid-compaction can still be caught in an
  inconsistent state, so `stop-start` is used again.
- `container` defaults to `couchdb`; the data folder is found from the
  container's mount at `/opt/couchdb/data`, so the user types no path.
- Same `verify.methods: [archive-integrity, restart-smoke]` and
  `restartPolicy: always` as Vaultwarden, for the same reasons. The
  restart-smoke probe checks that CouchDB's health endpoint (`/_up` on
  port 5984) responds after the container restarts.

## Contribution checklist

Before opening a PR for a new catalog entry:

- [ ] The file name matches the service name (e.g. `service: gitea` lives
      in `catalog/gitea.yaml`).
- [ ] It passes the catalog test suite:
      ```bash
      go test ./catalog/ ./internal/catalog/
      ```
      This runs schema validation and a template round-trip against every
      bundled entry automatically — you don't need to write a new test file
      for your entry, just make sure it loads and resolves cleanly under
      this suite.
- [ ] `go build ./...`, `go vet ./...` and `go test ./... -race` are all
      green (see [AGENTS.md](../AGENTS.md)).
- [ ] You've tested it against a real running container, end to end:
      ```bash
      ballast service add my-service --catalog my-service --destination <dest> \
        --param key=value ...
      ballast backup my-service
      ballast status --service my-service --verify
      ```
      and confirmed the result comes back green, not amber.
- [ ] No secrets anywhere in the YAML file.
