# Configuration reference

This page explains where Ballast keeps its files, every setting in
`ballast.yaml`, every environment variable it reads, how it finds
credentials, and how schedules and freshness alerts work. If you just
want to get a first backup running, see the [quickstart
guide](quickstart.md) instead — come back here when you want to
understand or change the defaults.

## Where Ballast keeps things

Ballast uses two directories:

- **Config directory** — `ballast.yaml`, your catalog additions, and
  (by default) the credentials Ballast saves for itself. Default:
  `~/.config/ballast`, or `$XDG_CONFIG_HOME/ballast` if that variable
  is set.
- **Data directory** — the SQLite database, per-run scratch space, and
  run locks. Default: `~/.local/state/ballast`, or
  `$XDG_STATE_HOME/ballast` if that variable is set.

Both defaults assume a normal user account. When Ballast runs as a
systemd service (see [systemd.md](systemd.md)) you point both
somewhere stable, such as `/etc/ballast` and `/var/lib/ballast`, with
the environment variables below.

Inside the data directory, Ballast creates two subfolders itself and
manages them automatically — there is no separate setting for either:

- `work/` — scratch space for an in-progress backup or restore (dump
  output, extracted archives). Cleaned up as each job finishes.
- `locks/` — one lock file per service (and one per destination),
  used to stop two runs of the same job overlapping.

### Environment variables

| Variable | Effect |
|---|---|
| `BALLAST_CONFIG_DIR` | Overrides the config directory. Takes priority over everything else, including `ballast.yaml`'s own `data_dir` key (see below). |
| `BALLAST_DATA_DIR` | Overrides the data directory. Same top priority. |
| `XDG_CONFIG_HOME` | Used to compute the default config directory when `BALLAST_CONFIG_DIR` is not set. |
| `XDG_STATE_HOME` | Used to compute the default data directory when `BALLAST_DATA_DIR` is not set. |
| `CREDENTIALS_DIRECTORY` | Set automatically by systemd when a unit uses `LoadCredential=`. Ballast reads credential files from here first — see [Credentials](#credentials-and-secrets) below. You should not set this by hand. |

Set `BALLAST_CONFIG_DIR` (and, if you want a non-default data
location, `BALLAST_DATA_DIR`) the same way for every way you run
Ballast on a machine — the long-running service, `run-due` from a
timer, and any interactive command you type yourself (`sudo
BALLAST_CONFIG_DIR=/etc/ballast ballast wizard`). If they disagree,
each sees a different database and a different set of credentials,
which looks like data going missing.

### `ballast.yaml`

An optional YAML file at `<config dir>/ballast.yaml`. Every key is
optional; anything you leave out uses its default.

| Key | Type | Default | Notes |
|---|---|---|---|
| `data_dir` | string (path) | XDG default, see above | Ignored if `BALLAST_DATA_DIR` is set — the environment variable always wins. |
| `catalog_dirs` | list of strings (paths) | `["<config dir>/catalog.d"]` | Extra directories of your own YAML catalog entries, loaded on top of the entries built into the binary. See [catalog-contributing.md](catalog-contributing.md). |
| `max_parallel_jobs` | integer | `2` | How many *different* services a scheduler sweep (`run-due`, or the built-in scheduler inside `ballast serve`) may back up at the same time. Must be between `1` and `16`; a value outside that range fails to load with a clear error rather than being silently clamped. One service is still never run twice at once, regardless of this setting — a slow or overlapping backup of the same service simply waits or is skipped, not run in parallel with itself. |

Example:

```yaml
data_dir: /var/lib/ballast
max_parallel_jobs: 3
catalog_dirs:
  - /etc/ballast/catalog.d
```

## Credentials and secrets

**Secrets never live in the database, in log lines, or in API
responses.** A password, token, ntfy link, or webhook URL only ever
exists as a file on disk (root-only, permission `0600`) and, briefly,
in memory while it's being used — and it's added to Ballast's
redaction list the moment it's read, so it can't leak into an error
message either. If you ever see a credential file with looser
permissions, `chmod 600` it; Ballast refuses to use a file that group
or other accounts can read.

### Where credential files are looked up

Every credential Ballast needs is a plain file, named after what it
holds (see the table below), inside a **credential directory**. There
are two:

1. **`$CREDENTIALS_DIRECTORY`** — set by systemd when a unit uses
   `LoadCredential=`. This is where files *you* provide belong. It's
   rebuilt fresh on every service start, so Ballast only ever reads
   from it, never writes to it.
2. **`<config dir>/credentials`** — Ballast's own directory, where it
   saves secrets it generates or that you type into the wizard (a
   fresh repository password, an ntfy link). This directory persists
   across restarts, which `$CREDENTIALS_DIRECTORY` does not.

Ballast looks in directory 1 first, then directory 2, and uses the
first file it finds with the right name. In other words: **a file you
provide via `LoadCredential=` always wins over one Ballast saved
itself under the same name.** Outside systemd (a plain interactive
run with no `$CREDENTIALS_DIRECTORY`), both of those resolve to the
same folder, `<config dir>/credentials`, so there's nothing to
reconcile.

Ballast logs which directory it actually used for credentials at
startup — never what's inside any file.

### Credential file names

Each file holds one secret as plain text (a trailing newline
is fine). `<service>` and `<destination>` below are the names you gave
those things (`ballast service add <service>`, `ballast destination
add <destination>`).

| File name | Holds | Who creates it |
|---|---|---|
| `ballast-api-token` | The bearer token `ballast serve`'s REST API (and the web UI) requires on every request. | You. Generate one yourself — see [systemd.md](systemd.md). |
| `ballast-heartbeat-url` | Optional. A "dead man's switch" address (Healthchecks, Uptime Kuma and similar) that `ballast serve` requests every 5 minutes while it is running, so an outside service can alert you if Ballast or the machine stops. Treat it like a password. `ballast doctor` shows when it last worked. | You. Copy the ping address from your monitor into the file (mode 0600). |
| `ballast-restic-password.<destination>` | The restic repository password for that destination. | Either. The wizard and `ballast destination add` generate and save one automatically if none exists; you can provide your own instead by placing a file of this name first. |
| `ballast-dest-keys.<destination>` | The access key pair for a Backblaze B2, S3 or Cloudflare R2 destination, as an AWS-style credentials file (`[default]`, `aws_access_key_id = …`, `aws_secret_access_key = …`). For B2 that's the application key ID and application key. | Either. The web UI's destination form and `ballast destination add --access-key-id` write it for you; you can provide your own instead. |
| `ballast-dest-ssh-key.<destination>` | The SSH private key (no passphrase) Ballast logs in to an SFTP destination with. Add its public key to the server's `~/.ssh/authorized_keys`. The server's host key is remembered in `<data dir>/ssh/known_hosts` the first time Ballast connects. | Either. The web UI's form and `ballast destination add --ssh-key-file` save it for you. |
| `ballast-ntfy-topic.<service>` | The full ntfy URL to POST alerts to for that service (e.g. `https://ntfy.sh/my-secret-topic`, not just the topic name). | Either. The wizard writes one when you paste a link into it. |
| `ballast-ntfy-topic` | Same, but used for any service that has no per-service file. | You. |
| `ballast-webhook-url.<service>` | The webhook URL to POST alert JSON to for that service. | You. |
| `ballast-webhook-url` | Same, but the fallback used when a service has no per-service file. | You. |
| `ballast-trigger-token.<service>` | The token `ballast trigger <service>` (or a `POST` to the trigger API) must present to start that service's backup. | You — see `ballast token generate <service>`. |

For the ntfy and webhook files, Ballast checks the per-service file
first and only falls back to the global one if that specific service
has none — so most homelabs only need the two global files, and only
services that should alert somewhere different need their own.

## Schedules

A schedule is a standard 5-field cron expression (`minute hour
day-of-month month day-of-week`), set with `ballast schedule set
<service> "<cron>"`. Ballast's own scheduler (inside `ballast serve`)
and `ballast run-due` (for a systemd timer or plain cron) both read
the same schedule table, so which one you use doesn't change when
jobs run.

- **A service that has never been backed up is backed up at the very
  next `run-due` sweep** (or the next tick of the built-in scheduler)
  once it has a schedule, regardless of what the cron expression says,
  and only then settles into following that expression. This means a
  new service is never left waiting days for its first backup just
  because its schedule happens to be a weekly one. A service that was
  backed up recently is not due straight away, and changing a
  schedule's cron expression never starts a backup: it only changes
  when future backups happen. `ballast schedule set` and `ballast
  schedule list` show when each schedule runs next (`Next run: ...`,
  or `Runs at the next check, then ...` if one is due now).
- **`ballast schedule disable <service>` pauses a schedule.** It shows
  as `paused` in `schedule list` and never runs until you run
  `ballast schedule enable <service>`. The cron expression is kept.
- Day-of-month and day-of-week follow classic (Vixie) cron behaviour:
  when *either* field is restricted to specific values, a date matches
  if **either** field matches (not both) — except that a field
  starting with `*` (including a step like `*/2`) is never considered
  "restricted" for this rule, in which case **both** fields must
  match. For example, `0 0 */2 * 1` means "midnight, every other day
  of the month, that also happens to be a Monday" (both must match,
  because `*/2` starts with `*`), while `0 0 15 * 1` means "midnight on
  the 15th, OR any Monday" (either is enough).

### Freshness alerts

Separately from "did the last run fail," Ballast can warn you when a
service simply hasn't produced a good backup recently enough — for
example because the machine was off, or someone disabled its
schedule. This is the freshness check (see `ballast notify set
--help`).

Unless you set `--max-age-hours` yourself, the threshold is derived
from the service's own schedule:

- A schedule that fires at least daily uses a **48-hour** default.
- A schedule that fires at least weekly uses a **9-day** default.
- A sparser schedule (monthly or longer) uses that schedule's own
  typical gap between runs, plus two days of slack.

These numbers intentionally lean generous — a weekday-only schedule,
say — so a normal weekend or a slightly late run never fires a false
alarm.

## Backup retention and setup

Each destination has its own "keep" policy — how many of the newest
daily, weekly and monthly backups to hang on to (`--keep-daily`,
`--keep-weekly`, `--keep-monthly` on `ballast destination add`, or the
"Old backups to keep" fields in the web UI). Older backups beyond that
are removed, and the space they used is reclaimed, **after every
*verified* backup** — a run that fails verification is never allowed
to age out anything, so a bad backup can't quietly push a good one out
of the retention window.

A service can set its own retention policy instead of following its
destination's, with `ballast service add --keep-daily/--keep-weekly/
--keep-monthly` (given at add time) or `ballast service retention
<name> --keep-daily ... --keep-weekly ... --keep-monthly ...`
afterwards; give only the flags you want to change and the rest keep
following the destination. `ballast service retention <name>
--inherit` removes the override again. In the web UI, each service's
"Backups to keep" panel (under its Details) does the same thing. Only
give one or two of the three flags/fields if that's all you want to
change — e.g. `--keep-daily 3` alone leaves weekly and monthly exactly
as the destination has them.

Whichever policy applies, cleanup only ever touches that one service's
own backups, identified by its tag — even when several services share
one destination, cleaning up after one service's backup never removes
another service's snapshots.

### Keep the newest N backups

`--keep-last N` (on `ballast destination add`, `ballast service add` and
`ballast service retention`, or the "Newest (any age)" field in the web UI)
keeps the newest N backups whatever their age. To keep *only* the newest 2,
set the daily, weekly and monthly counts to 0 and `--keep-last 2`. It adds
to the other rules; it never replaces them.

### Setting up a destination's repository

A destination needs its restic repository created once before backups
can be sent to it — normally done automatically the first time you add
it (tick "set up the storage now" in the web UI, or pass `--init` to
`ballast destination add`). If you skipped that, or the storage a
destination points at was later emptied out (a fresh bucket, a wiped
disk), run:

```
ballast destination init <name>
```

or click "Set up repository" next to that destination in the web UI.
It's safe to run at any time, including against a destination that's
already set up — nothing happens if the repository already exists.

## Skipping files and folders

Any service can skip things it does not need, such as logs, caches or
`node_modules`. Give patterns with `ballast service add --skip "*.log"`
(repeat for more), change them later with `ballast service excludes <name>
--skip "*.log" --skip node_modules` (or `--clear`), or use the "Skip these
files and folders" panel on the service in the web UI. This is how you skip
things in the generic folder service.

A pattern is a name that may use `*` and `?`. A pattern without a slash
matches that name anywhere inside the folder (`*.log`, `node_modules`);
`cache/thumbs` matches a `thumbs` folder inside any `cache` folder; a
leading slash (`/tmp`) matches from the top of the folder only. Catalog
entries can also carry their own `exclude:` lists on `files` and
`hostPaths` entries; yours are added on top.

"Show what would be skipped" (`ballast service excludes <name> --preview`)
lists what the patterns would leave out and how big it is, without backing
anything up. Folders on another Docker host skip the same things when the
backup runs, but cannot be previewed yet.

## Sending one backup to several places

A service can back up to more than one destination, for example a disk in
your server and an offsite bucket. One run makes the backup once (your app
is stopped at most once) and sends it to every place. Each place is checked
on its own: if the offsite copy fails, the local copy still counts and the
alert names the place that failed.

    ballast service add photos --catalog immich --destination ssd --also-to b2=library
    ballast service targets photos --add offsite
    ballast service targets photos --remove offsite

`--also-to b2=library` sends only the `library` part to `b2`. Parts are the
named pieces of a service (Immich has `database` and `library`; a plain
folder is `files`). Leave the parts off to send everything. In the web UI,
tick "Also send to…" on the service form. The first destination is the main
one, and the service shows red if any copy is red.

Each destination keeps its own number of old backups. To give one a different
setting from the destination default, use the service's "Backups to keep"
panel for the main destination. A first upload of many gigabytes can take
hours; Ballast writes its progress to the log every half minute. Cloud
storage (Backblaze B2, S3) is billed by the amount stored, so check your
provider's prices before sending a large library.

## A second copy in Proton Drive

Besides your main backups, Ballast can keep an extra copy of the small, important files (configs and
database backups) in Proton Drive. It is a safety net, not a replacement: the copy is reported on its own,
so a Proton problem shows as an amber light and never turns a good backup red.

1. Install Proton's own command-line tool (`proton-drive`) from <https://proton.me/drive/download> and put it
   on the PATH. `ballast doctor` checks that it is there.
2. Add the destination: Destinations page, "Proton Drive", or
   `ballast destination add proton --type proton-drive --path /my-files/ballast`.
3. Sign in: "Sign in to Proton Drive" on the Destinations page, or `ballast destination sign-in proton`.
   Ballast shows a link to open on any device. Your Proton password and 2FA code are typed on Proton's page
   and never reach Ballast. The login is kept in a private folder (only Ballast's user can open it) next to
   Ballast's other credentials.
4. Add it to a service as an extra place ("Also send to…", or `ballast service targets --add proton`). Choose
   only the small parts (for example the database dump) with the parts selector.

What happens on each backup: once the main backup is verified, Ballast uploads that run's files to
`<folder>/<service>/<date and time>/`, checks that every file arrived at the right size, downloads one of them
(a different one each time) and compares it with the original. A copy that cannot be read back is amber.
Files over the size limit (1024 MB unless you change it) are skipped and listed; folders of live files such as
a photo library are never sent. Only the newest N dated folders are kept (7 unless you change it); Ballast only
ever removes folders it created itself. If your Proton login ends, the light turns amber and says "sign in
again". If the main backup fails, no copy is attempted.

## See also

- [Quickstart](quickstart.md) — quick first-run setup.
- [Systemd setup](systemd.md) — running `ballast serve` or `run-due`
  unattended, and how the config/credential directories interact with
  systemd's own `LoadCredential=`.
- [Catalog contribution guide](catalog-contributing.md) — adding new
  services to the catalog.
