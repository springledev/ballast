# Running Ballast with systemd

This page shows two supported ways to run Ballast unattended on a
Linux server, with complete, copy-pasteable unit files. It also
explains the handful of systemd settings that matter specifically
because Ballast shells out to `docker` and `restic` to do its work.

If you haven't set up your first backup yet, start with the
[quickstart guide](quickstart.md) — you can move it under systemd
afterwards using the steps here. For what every setting and
credential file actually means, see the [configuration
reference](configuration.md).

## The two ways to run it

- **`ballast serve`** — a long-running service: the web UI, the REST
  API, and a built-in scheduler that wakes up every minute and runs
  whatever backups are due. This is the simplest option: one service,
  and you get the dashboard too.
- **`ballast run-due` on a timer** — a systemd *timer* fires
  `ballast run-due` every minute (or so); it checks what's due, runs
  it, and exits. No web UI, no long-running process. Good for a
  headless box, or if you'd rather use the CLI (`ballast status`) than
  a browser.

You can also mix them: run `ballast serve` for the dashboard but rely
on it alone for scheduling, or run `run-due` from a timer and only
start `ballast serve` occasionally when you want to look at the UI.
Either way, install the [trigger unit](#triggering-a-backup-from-a-udev-rule-or-script)
if you want an external event (like plugging in a phone, in a future
Ballast release) to start one specific backup on demand.

A quick vocabulary note for anyone new to systemd: a **unit file** is
a small text file describing a service or timer; `systemctl enable`
makes it start at boot, `systemctl start` starts it right now. A
**timer unit** is a systemd-native alternative to a cron line — it
triggers its matching `.service` unit on a schedule.

## Before you start: pick your directories

Decide once where the config and data live, e.g. `/etc/ballast` and
`/var/lib/ballast`, and use the *same* two locations everywhere:
inside every unit file below, and on the command line whenever you run
`ballast` interactively as root. Ballast resolves its config directory
from the `BALLAST_CONFIG_DIR` environment variable (data directory:
`BALLAST_DATA_DIR`); if the unit and your interactive shell disagree,
they'll end up looking at two different databases and two different
sets of credentials, and things will look like they've vanished.

```bash
sudo mkdir -p /etc/ballast /etc/ballast/credentials /var/lib/ballast
sudo chmod 700 /etc/ballast/credentials

# Any interactive command — the wizard, checking status, adding a
# service — should set the same two variables:
sudo BALLAST_CONFIG_DIR=/etc/ballast BALLAST_DATA_DIR=/var/lib/ballast ballast wizard
```

## Provisioning credentials

Every secret Ballast uses is a plain-text file, permission `0600`,
owned by root, named after what it holds (the full list and lookup
rules are in the [configuration reference](configuration.md#credentials-and-secrets)).
Under systemd, you hand these to Ballast with `LoadCredential=NAME:PATH`
in the unit file — systemd copies the file into a private, read-only
directory just for that service ($CREDENTIALS_DIRECTORY) before it
starts, and Ballast checks there first.

### The API token (`ballast serve` only)

`ballast serve` auto-generates a 256-bit access key on first start if one
doesn't exist, prints it to the terminal once, and saves it root-only
(0600). You can then enter that key on the web UI's sign-in screen.

To use your own access key instead (for example, if you're migrating from an
existing setup), create it before starting:

```bash
sudo sh -c 'umask 077; openssl rand -hex 32 > /etc/ballast/credentials/ballast-api-token'
sudo chmod 600 /etc/ballast/credentials/ballast-api-token
```

### Restic repository passwords

If you set up your destination through the wizard or `ballast
destination add` without providing a password credential yourself,
Ballast generates one and saves it under `<config
dir>/credentials/ballast-restic-password.<destination>` — **back that
file up somewhere off this machine.** It is the only thing that can
decrypt your backups; if it and the machine are lost together, so are
the backups. To supply your own instead (or reuse one from an existing
restic setup), create the file yourself before adding the destination:

```bash
sudo sh -c 'umask 077; echo "your-repository-password" > /etc/ballast/credentials/ballast-restic-password.mybackup'
```

### ntfy / webhook alert destinations

```bash
# ntfy: the file holds the FULL url to POST to, not just a topic name
sudo sh -c 'umask 077; echo "https://ntfy.sh/your-hard-to-guess-topic" > /etc/ballast/credentials/ballast-ntfy-topic'

# webhook
sudo sh -c 'umask 077; echo "https://example.com/hooks/ballast" > /etc/ballast/credentials/ballast-webhook-url'
```

Add `.{service-name}` to either file name (e.g.
`ballast-ntfy-topic.immich`) to route just that one service's alerts
somewhere different.

### What Ballast logs

Ballast logs *which credential directory it used* at startup (so you
can debug "why isn't my token being picked up" issues) — it never logs
a credential's contents, in this or any other message. If something
looks wrong, `systemctl status` or the journal will tell you the
directory, not the secret.

## `ballast.service` — running `serve`

```ini
[Unit]
Description=Ballast backup service (web UI + API + scheduler)
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
Environment=BALLAST_CONFIG_DIR=/etc/ballast
Environment=BALLAST_DATA_DIR=/var/lib/ballast
ExecStart=/usr/local/bin/ballast serve
LoadCredential=ballast-api-token:/etc/ballast/credentials/ballast-api-token
LoadCredential=ballast-restic-password.mybackup:/etc/ballast/credentials/ballast-restic-password.mybackup
LoadCredential=ballast-ntfy-topic:/etc/ballast/credentials/ballast-ntfy-topic
TimeoutStopSec=5min
KillMode=mixed
Restart=on-failure
RestartSec=5s
User=root
Group=root

[Install]
WantedBy=multi-user.target
```

List one `LoadCredential=` line per credential file you've
provisioned (add more for other destinations, per-service ntfy/webhook
overrides, and so on).

```bash
sudo cp ballast.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ballast.service
sudo systemctl status ballast.service
```

Then open `http://<host>:8080` and enter the access key that Ballast printed
to the terminal on first start. (Or if you created your own token file, use
that instead.)

## `ballast-run-due.service` + `.timer` — headless, no web UI

**`ballast-run-due.service`:**

```ini
[Unit]
Description=Ballast: run any backup jobs that are due
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=oneshot
Environment=BALLAST_CONFIG_DIR=/etc/ballast
Environment=BALLAST_DATA_DIR=/var/lib/ballast
ExecStart=/usr/local/bin/ballast run-due
LoadCredential=ballast-restic-password.mybackup:/etc/ballast/credentials/ballast-restic-password.mybackup
LoadCredential=ballast-ntfy-topic:/etc/ballast/credentials/ballast-ntfy-topic
TimeoutStopSec=5min
KillMode=mixed
User=root
Group=root
```

(No `ballast-api-token` line here — `run-due` never starts the API, so
it doesn't need one.)

**`ballast-run-due.timer`:**

```ini
[Unit]
Description=Run ballast run-due every minute

[Timer]
OnCalendar=*-*-* *:*:00
AccuracySec=1s
Persistent=true

[Install]
WantedBy=timers.target
```

`Persistent=true` means if the machine was off when a run was due, it
fires once as soon as the machine is back up, instead of waiting for
the next minute boundary.

```bash
sudo cp ballast-run-due.service ballast-run-due.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ballast-run-due.timer
```

`ballast run-due` exits non-zero if any job failed. That means a
failed run shows up as `systemctl status ballast-run-due.service`
reporting `Main PID exited, code=exited, status=1/FAILURE` (rather
than silently looking fine), and you can attach an `OnFailure=`
directive to the service to get a systemd-level alert on top of
whatever Ballast's own ntfy/webhook notifications already send:

```ini
[Unit]
OnFailure=status-email@%n.service   # or your own alerting unit
```

## Triggering a backup from a udev rule or script

`ballast trigger <service> --token ...` (or a token file) runs one
service's backup on demand, authenticated by a per-service token
instead of the API bearer token — meant for external triggers (a udev
rule, a script) that should only be able to start *one* specific
backup, nothing else. Generate a token and provision it as a
per-instance credential with a **templated unit**,
`ballast-trigger@.service` (the `@` makes it a template; `%i` becomes
whatever you put after the `@` when you start it, e.g.
`ballast-trigger@immich.service` runs the `immich` job):

```bash
ballast token generate immich   # prints the token once — save it now
sudo sh -c 'umask 077; echo "the-printed-token" > /etc/ballast/immich.token'
```

**`ballast-trigger@.service`:**

```ini
[Unit]
Description=Ballast: run one backup for %i via a trigger token
After=network-online.target docker.service

[Service]
Type=oneshot
Environment=BALLAST_CONFIG_DIR=/etc/ballast
Environment=BALLAST_DATA_DIR=/var/lib/ballast
ExecStart=/usr/local/bin/ballast trigger %i
LoadCredential=ballast-trigger-token.%i:/etc/ballast/%i.token
TimeoutStopSec=5min
KillMode=mixed
User=root
Group=root
```

```bash
sudo cp ballast-trigger@.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl start ballast-trigger@immich.service
```

A udev rule (or any other systemd-aware trigger) can start
`ballast-trigger@immich.service` the same way `systemctl start` does,
without ever seeing the token itself — it's on disk, root-only, and
loaded by systemd, not passed on any command line.

## Two settings every backup-running unit needs, explained

Every unit above that actually runs a backup (`ballast.service`,
`ballast-run-due.service`, `ballast-trigger@.service`) sets these two.
Skipping either can leave a container stopped after a crash or a
restart, which is exactly the failure mode Ballast exists to prevent.

- **`TimeoutStopSec=5min`** — When systemd stops one of these units
  (during a restart, a reboot, or `systemctl stop`), it sends SIGTERM
  and then waits this long before giving up and sending SIGKILL.
  Ballast's stop‑start backup strategy (used for services like
  Vaultwarden that must be stopped, backed up, then restarted) bounds
  its own container restart at 2 minutes; systemd's *default* stop
  timeout is only 90 seconds, so without raising it, a stop landing at
  the wrong moment could SIGKILL Ballast mid-restart and leave the
  container stopped. Five minutes gives comfortable headroom above
  that 2-minute bound.
- **`KillMode=mixed`** — This controls *who* gets the stop signal.
  With systemd's default (`control-group`), stopping the unit sends
  SIGTERM to Ballast *and* every child process it has spawned (its
  `docker`/`restic` subprocesses) all at once, which can cut a
  `docker stop`/`docker start` cycle off halfway. `KillMode=mixed`
  sends SIGTERM to the main Ballast process only, so Ballast can wind
  down its own children in the right order; only if the process is
  still around after `TimeoutStopSec` does systemd SIGKILL the whole
  group.

## Verifying unit files before installing them

If you've edited these units, sanity-check the syntax before copying
them into `/etc/systemd/system/`:

```bash
systemd-analyze verify --man=false ./ballast.service
systemd-analyze verify --man=false ./ballast-run-due.service ./ballast-run-due.timer
systemd-analyze verify --man=false './ballast-trigger@.service'
```

Run this from a scratch directory with the unit files only (not
installed anywhere) — `systemd-analyze verify` reads them directly
from disk. Expect a complaint that `/usr/local/bin/ballast` doesn't
exist if you haven't installed the binary at that path yet; that's
about your filesystem, not the unit syntax. Point `ExecStart=` at
wherever you actually built or installed the `ballast` binary to check
against a real file.

## See also

- [Configuration reference](configuration.md) — every `ballast.yaml`
  key, environment variable, and credential file, plus how the
  credential-directory lookup order and schedules work in detail.
- [Quickstart](quickstart.md) — the fastest path to a first verified
  backup, before wiring any of this up.
