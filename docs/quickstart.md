# Your first verified backup in 15 minutes

Ballast backs up the apps you run in Docker, sends the backup somewhere safe
with a tool called **restic**, and then actually checks that the backup can
be restored — so "backed up" means "backed up," not "we hope so."

You don't need to know anything about restic to use Ballast. In short: restic
is an open-source program that copies your files into an encrypted,
space-efficient "repository" (just a folder, on this disk or somewhere
remote) and can hand them back to you later. Ballast drives restic for you
and adds the checking on top.

## Prerequisites

- **Linux.** Ballast is a single binary; no other runtime is required.
- **Docker**, if you want to back up anything that runs in a Docker
  container (Immich, Vaultwarden, Postgres, MySQL, CouchDB, ...). Backing up
  a plain folder works without Docker.
- **restic 0.17.0 or newer** installed and on your `PATH` (`sudo apt install
  restic`, or see <https://restic.readthedocs.io/en/stable/020_installation.html>).
  Ballast is tested against restic 0.17.3 specifically; a newer version
  usually still works, but Ballast will print a warning if it hasn't been
  tested against it, and it refuses to start at all on anything older than
  0.17.0.

Check your restic version with:

```bash
restic version
```

## 1. Install the binary

Build it from source (there isn't a packaged release yet):

```bash
make build
```

This produces a single static binary at `bin/ballast`. Copy it wherever you
like, e.g. `/usr/local/bin/ballast`.

## 2. Run the wizard

The wizard is the fastest way to a first backup. Run it as root (or with
`sudo`) so it can talk to Docker and read container details:

```bash
sudo ballast wizard
```

It checks that restic is usable, then asks a short series of questions.
Every question shows a default in `[brackets]` — press Enter to accept it.

### Step 1 of 4: What do you want to back up?

Ballast lists everything in its built-in catalog (Immich, Vaultwarden,
Postgres, MySQL/MariaDB, CouchDB, or "any folder"), and — if Docker is
available — tries to spot a matching container already running on this
machine and suggests it.

Type the name of the entry you want (e.g. `immich`, or `folder` for a plain
directory), then give the service a short name of your own (lowercase
letters, numbers and dashes — this is just a label you'll use later in
commands like `ballast backup <name>`).

You'll then be asked for that catalog entry's parameters — see the [worked
Immich example](#worked-example-backing-up-immich) below for what to type
for Immich specifically.

### Step 2 of 4: Where should the backups be kept?

If you haven't configured a destination yet, the wizard offers to create a
local folder for you (a good starting point — you can also use cloud
destinations like Backblaze B2, S3, Cloudflare R2, or SFTP). It suggests a folder under Ballast's own
data directory, but you can point it anywhere, including a second disk or a
mounted NAS share, which is worth doing so a single disk failure can't take
out both your data and your backups.

This is where the wizard creates the restic repository and its **encryption
password**. You are never asked to invent or type a password yourself —
Ballast generates one and saves it to a file such as:

```
~/.config/ballast/credentials/ballast-restic-password.<destination-name>
```

**This file is important. Copy it somewhere off this machine** (a password
manager, a USB stick, printed and locked in a drawer — anywhere that isn't
this computer). Restic encrypts everything with this password, and without
it nobody — including you — can ever restore these backups, even if the
backup files themselves are intact.

### Step 3 of 4: Notifications (optional)

Ballast can message you through [ntfy](https://ntfy.sh) — a free
notification service with no account needed. Install the ntfy app,
subscribe to a topic with a hard-to-guess name (so strangers can't spam or
read it), and paste the topic's full link (e.g.
`https://ntfy.sh/my-secret-topic-x7d2`) when asked. Leave it blank to skip;
you can set it up later from the web UI's Notifications page or with
`ballast notify set`.

### Step 4 of 4: How often should it back up?

Pick `nightly` (3am every night — the recommended default), `weekly` (3am
every Sunday), `none` (only when you run `ballast backup` yourself), or type
your own cron expression.

### The first backup runs

The wizard then runs the backup immediately and verifies it, printing one of
three results per backup destination:

- **green — SUCCESS.** The backup was made and Ballast proved it can be
  restored.
- **amber — backed up but unverified.** The backup was made, but Ballast
  could not prove it restores. Treat this the same as "no backup" until it
  turns green — don't rely on it.
- **red — FAILED.** The backup itself did not succeed.

"Verified" is not a guess — for every backup, Ballast actually did one of:

- **read the backup back out of the destination** (restic `dump`/`ls` on a
  sample of what was stored, which also proves the backup is genuinely
  there and not just recorded as done);
- for a database, **restored the dump into a fresh throwaway container**
  and checked it came up cleanly with no real errors, then deleted the
  container; or
- checked the backup **archive is intact and readable** end to end (like
  `tar -tf`, but for whichever format was used).

Only after one of those checks passes does a backup go green.

## 3. What's next

### Look at the status any time

```bash
ballast status
ballast status --service immich --verify   # per-check detail
ballast status --json                       # for scripts
ballast status --check                      # for scripts: the answer is the exit code
ballast status --running                    # is a backup running right now?
ballast wait-idle --timeout 30m             # wait until nothing is running
```

`ballast status --check` prints the status and exits with 0 when every service
is green, 1 for amber, stale, running or no backups yet, 2 for red, and 3 when
the status cannot be read. `ballast status --running` exits with 1 if a backup
is running and 0 if not. `ballast wait-idle` waits until nothing is running and
then exits with 0, or exits with 1 if the timeout passes first (the default is
10 minutes). Use them in scripts, or before you stop or upgrade Ballast.

### Open the web UI

```bash
ballast serve
```

By default this listens on `http://0.0.0.0:8080` and serves both the API
and the Vue-based web UI. On first start, Ballast auto-generates a 256-bit
access key, prints it to the terminal once, and saves it root-only. Open the
web UI in a browser, enter that access key on the sign-in screen, and you're
in. Your browser then stays signed in for up to 30 days, and restarting or
upgrading Ballast does not sign you out. (Changing the access key, or using
**Sign out everywhere** on the Security page, does sign every browser out.) The
access key is kept in a cookie that is sent only to your own Ballast server.
(If you're listening beyond localhost without HTTPS, a
banner will remind you to use a reverse proxy with HTTPS or a tailnet.)

To use your own access key instead, create it before starting:

```bash
openssl rand -hex 32 > ~/.config/ballast/credentials/ballast-api-token
chmod 600 ~/.config/ballast/credentials/ballast-api-token
```

The dashboard shows a short **Getting started** checklist (add a
destination, add a service, set up notifications, run your first verified
backup) until every step is done. Above it, a **Ballast health** banner
appears only when Ballast itself needs attention: its own settings have not
been backed up in the last week (`ballast self-backup <destination>`), the
disk is running low for verification drills, or a password, key or `.env`
file your setup points at has gone missing.

### Set up a schedule or change it later

```bash
ballast schedule set <service> "0 3 * * *"
ballast schedule list
ballast schedule enable <service>
ballast schedule disable <service>
```

`schedule set` and `schedule list` say when each schedule runs next, for
example `Next run: Tue 2026-11-03 03:00`. A schedule you have turned off with
`schedule disable` shows as `paused` and never runs until you run
`schedule enable`; it is kept, not deleted. Saving a schedule does not start a
backup by surprise. The one exception is a service that has never been backed
up, or whose slot was missed: the list then says `Runs at the next check`.
To back up right now, run `ballast backup <service>`.

### Stop a backup, and look at past backups

```bash
ballast cancel <service>        # stop a backup that is running
ballast runs <service>          # past backups, newest first (--limit N, --json)
```

`ballast cancel` stops the backup that is running for that service. Anything
Ballast stopped for the backup (such as a container) is started again, the run
is recorded as cancelled, and your last good backup is kept. The web UI has the
same thing: a **Cancel** button on the dashboard while a backup runs, and a
**History** button on each service that opens the **Backup history** page. It
lists every run, and clicking one shows each check that was done and why
anything is amber or red.

### Remove a service

`ballast service remove <service>` (or **Remove** on the Services page) tells
Ballast to stop backing that service up. It deletes the service's schedule,
trigger token and notification settings, and stops its history from counting
toward your status. It does **not** touch the backups already stored in your
destinations, your destinations themselves, or the encryption password files,
so you can still restore from them. If a backup of that service is running,
removal waits for it to finish.

### Notifications

```bash
ballast notify show <service>
ballast notify set <service> --heartbeat --max-age-hours 72
```

For a weekly "Ballast is alive" message, turn on the digest for a channel
(the checkbox in Settings, or `ballast notify channel edit <name> --digest`).
Once a week you get one message: how many services are green, amber and
red, when each one last passed a restore check, and free disk space.
`ballast notify digest` sends one right away.

### Restore something

```bash
ballast snapshots <service>
ballast restore <service> --to /path/to/empty/restore/dir
ballast restore <service> --snapshot <id> --file /some/specific/path --to /path/to/empty/restore/dir
```

`--to` must point at a new, empty location — Ballast never restores over
existing data by default.

### Run it permanently (daemon first)

The simplest setup is `ballast serve` kept running by systemd: it hosts the
web UI, the API and the scheduler in one process, so scheduled backups run
and you can manage everything from the browser, including notification
channels (Settings), `.env` sources and compose-stack discovery. Install the
shipped hardened unit from [Systemd setup](systemd.md).

**Don't port-forward Ballast to the internet.** Use a reverse proxy with
HTTPS or a tailnet/VPN to reach it remotely.

### Run it unattended with a timer instead

`ballast run-due` runs every job whose schedule is currently due and exits
non-zero if anything failed — built for a systemd timer or cron, so backups
keep happening even if you never run `ballast serve`. See
[Systemd setup](systemd.md) for unit files and credential wiring.

### Back up services on other machines

To back up containers running on a different machine in your homelab, see
[Docker hosts](docker-hosts.md) for setting up SSH or TLS connections.

## Worked example: backing up Immich

Immich keeps two things: a Postgres database (albums, people, metadata) and
a library folder of the actual photo/video files. When the wizard asks for
Immich's parameters, answer with:

| Prompt | What to type |
|---|---|
| Path to your Immich upload/library folder | The value of `UPLOAD_LOCATION` in Immich's `.env` file — the folder on this machine holding your photos. |
| Name of Immich's database container | The default, `immich_postgres`, is right for a standard Immich installation. |
| Immich's database user | The default, `postgres`, matches `DB_USERNAME` in a standard Immich `.env`; change it if you customized that. |
| Image of Immich's database container | Copy the exact image tag from the `database` service in Immich's `docker-compose.yml` (the default shown is only a fallback — Immich's Postgres image bundles vector-search extensions a plain `postgres` image doesn't have, and the test restore must use the same image your Immich stack actually runs, or the restore check would fail for the wrong reason). |

Everything else about the Immich entry — how the dump runs, how the restore
is verified — is handled for you; see
[Adding a service to the catalog](catalog-contributing.md) if you're curious
how it works under the hood, or want to add a similar entry for something
else.

### Check your setup

```bash
ballast doctor
```

This checks restic, your Docker hosts, destinations and free disk space, and tells you in plain English what to fix.

### Proxmox

If you run Proxmox, see the [Proxmox guide](proxmox.md). Replacing Ballast with a newer version? See [Upgrading](upgrading.md). Coming from v0.1? See [Migrating from v0.1](migrating.md).
