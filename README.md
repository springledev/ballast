# Ballast

Ballast is a self-hosted backup tool for Docker-based homelabs. It backs up your favorite services (Immich, Vaultwarden, Postgres, MySQL, CouchDB, and any folder), encrypts and stores them using restic—a well-known open-source backup program—on local disk or cloud destinations (Backblaze B2, S3, Cloudflare R2, and SFTP), and proves every single backup can actually be restored before marking it done. Think of it as automated backup sanity checking for the apps that matter to you. No telemetry, no tracking.

**Status meanings:** Green = verified and restores; Amber = backed up, but Ballast could not prove it restores; Red = something failed.

## Quickstart

**Requirements:** Linux, restic 0.17.0 or newer installed (`sudo apt install restic` or equivalent), and Docker if backing up Docker services.

1. Build Ballast:
   ```bash
   make build
   ```
   The binary appears at `bin/ballast`.

2. Run the interactive setup wizard:
   ```bash
   sudo bin/ballast wizard
   ```
   This will detect your Docker services, let you pick one to back up, and run your first verified backup — usually well within 15 minutes.

3. For the full walkthrough, see [docs/quickstart.md](docs/quickstart.md).

## Documentation

- [Quickstart](docs/quickstart.md) — Get your first backup running
- [Configuration](docs/configuration.md) — Configure services and destinations
- [Systemd setup](docs/systemd.md) — Run backups automatically
- [Contributing services](docs/catalog-contributing.md) — Add your own services to the catalog
- [Docker hosts](docs/docker-hosts.md) — Back up containers on other machines
- [Proxmox](docs/proxmox.md) — Back up guests and the services inside them
- [Replacing a backup script](docs/replace-a-script.md) — Move over from a shell script without a gap
- [Upgrading](docs/migrating.md) — Moving from v0.1
- [Upgrading](docs/upgrading.md) — Replace the program safely and go back if needed

## Supported Services

Ballast comes with built-in support for:

- Immich (photos & videos)
- Vaultwarden (password manager)
- CouchDB (database)
- MySQL / MariaDB (databases)
- PostgreSQL (databases)
- Any folder

## Commands

- `backup` — Run a verified backup for one configured service now
- `catalog` — Browse the service catalog
- `destination` — Manage backup destinations
- `doctor` — Check restic, Docker hosts, destinations, and disk space for common configuration problems
- `notify` — Choose when Ballast alerts you about each service
- `restore` — Restore a service's backup into a new directory
- `run-due` — Run every backup job whose schedule is due (for systemd timers)
- `schedule` — Manage backup schedules
- `self-backup` — Back up Ballast's own state database and configuration to a destination
- `serve` — Serve the Web UI and REST API
- `service` — Manage configured backup services
- `snapshots` — List restic snapshots for one service
- `status` — Show backup status for one or all services
- `token` — Manage external trigger tokens
- `trigger` — Run one backup using a trigger token (for automation scripts)
- `wizard` — Set up your first backup, step by step, and prove it can be restored
