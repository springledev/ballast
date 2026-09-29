# Upgrading from Ballast v0.1

Upgrading is meant to be uneventful: replace the binary and run any command. Ballast upgrades its own database the first time it starts, and your existing services, schedules, destinations and snapshots keep working.

## Before you upgrade

Take a copy of Ballast's data folder (see [Configuration](configuration.md#where-ballast-keeps-things)). Ballast can also do this for you:

```bash
ballast doctor
ballast self-backup <destination>
```

`self-backup` sends Ballast's own status database, config and your own catalog files to a destination and checks that they can be read back.

## What changes

| Then (v0.1) | Now |
| --- | --- |
| Destinations were local folders. | Destinations have a type: local, Backblaze B2, S3, R2 or SFTP. Existing local destinations are converted automatically. |
| Services always used the Docker on the same machine. | Every service belongs to a Docker host. Existing services use the built-in `local` host. Add others with `ballast host add` ([Docker hosts](docker-hosts.md)). |
| Secrets went in files by hand. | `ballast secret` manages them, and catalog entries can refer to them. |
| Image matching in the catalog was a plain word. | It is now a case-insensitive pattern, so `mysql\|mariadb` works. Single words behave exactly as before. |
| Status was one light per service. | Each destination shows its own light, plus how long ago the last restore test passed. A stale one shows amber. |

## New things you can use

- `ballast verify <service>` or `--all` re-runs the restore test on the latest backup without making a new one.
- `ballast catalog validate <file>` and `install <file>` check and add your own catalog entries.
- Retention: `forget --prune` runs per your policy after each backup.
- Docker volumes and Compose projects can be backed up and restored together (`ballast restore-project`).
- [Proxmox](proxmox.md) guest backups and restore drills.
- `ballast doctor` for a health check.

## If something looks wrong

Run `ballast doctor` first. Your old snapshots stay in their restic repositories, so you can always restore them with `ballast restore`, even if you roll back to the old binary. Copy back the data folder from before the upgrade if you do roll back.
