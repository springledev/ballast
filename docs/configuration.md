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

- **A brand-new schedule backs the service up once at the very next
  `run-due` sweep** (or the next tick of the built-in scheduler),
  regardless of what the cron expression says, and only then settles
  into following that expression. This is deliberate: it's the "prove
  it works right now" moment, and it means a service is never left
  waiting days for its first backup just because its schedule happens
  to be a weekly one. Changing an *existing* schedule's cron
  expression does **not** trigger this immediate run — it only changes
  when future backups happen.
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

## See also

- [Quickstart](quickstart.md) — quick first-run setup.
- [Systemd setup](systemd.md) — running `ballast serve` or `run-due`
  unattended, and how the config/credential directories interact with
  systemd's own `LoadCredential=`.
- [Catalog contribution guide](catalog-contributing.md) — adding new
  services to the catalog.
