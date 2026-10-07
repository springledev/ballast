# Replacing a backup script with Ballast

Most people arrive at Ballast with a script that has been quietly doing the job for years. This guide moves you over without a gap: Ballast runs beside the script, proves itself, and only then do you switch the script off.

## The plan

1. **Run both side by side.** Set up each service in Ballast with its own destinations. Do not point Ballast at the folders, buckets or dump files your script uses, so the two never touch each other's files.
2. **Look before you run.** For any service, `ballast backup <service> --preview` (or "What would a backup do?" on the Services page) lists what would be backed up, where each part goes, which containers are stopped and for about how long, what is skipped and what reaches a second copy. Nothing is run.
3. **Seven green days.** Wait until the service has been green for seven days in a row. Green means the backup was made and then restored in a test, not just that a file exists.
4. **One test restore each.** Restore every service once from the Ballast screen into a new folder, and open the result. A backup you have never restored is a guess.
5. **Switch the script off.** Comment out its schedule first (do not delete it yet). Keep it for a month, then remove it.

The dashboard shows this as a checklist for each service. You tick the steps; the last step only unlocks when the service is green right now.

## What your script does, in Ballast terms

| In your script | In Ballast |
| --- | --- |
| `pg_dump \| gzip > file` | The Postgres entry (or Immich, Gitea, MariaDB): Ballast runs the dump, checks it, then restores it into a throwaway container. Use `db_user` for a database user that is not `postgres`. |
| `mysqldump` / `mariadb-dump -u app db` | The `mariadb-db` entry with the app user, and the password kept as a secret. |
| `docker stop x && tar czf ... && docker start x` | A stop-and-copy service (Vaultwarden, CouchDB, or the folder entry with containers listed). Ballast always starts the containers again, even if the backup fails. |
| `tar` of a folder on another machine | A folder on the Docker host (`hostPaths`): Ballast reads it through a read-only helper container on that machine. |
| `tar --exclude=...` | "Skip these files and folders" on the service; preview shows what is left out. |
| `find -mtime +N -delete` | Retention on the destination or the service, for example "keep the last 2". Ballast only ever deletes backups it made. |
| `restic forget --keep-daily 7 ... --prune` | Retention on the destination (daily, weekly, monthly, last). It runs after each backup. |
| `restic check` | Repository checks, scheduled per destination with their own light. |
| `restic unlock --remove-all` | Ballast clears a lock only when it is this machine's, old, and its process is gone. Anything else turns amber with an explanation. |
| `vzdump` for a guest | A Proxmox guest job: schedule, light and alerts like any service; Ballast prunes only dumps it made. |
| restic copy of `/etc/pve` | The Proxmox settings snapshot. |
| Copy to B2 and to a local disk | Two destinations on one service ("Also send to..."). One run, one stop and start, each destination checked on its own. |
| Upload to Proton Drive | A Proton Drive destination as a second copy for small, verified files. A failure there never changes the service's light. |
| `curl ntfy.sh/...` | A notification channel (ntfy or webhook) with failure alerts and a weekly digest. |
| `--dry-run` | `ballast backup <service> --preview`. |

## Things to watch for

- **Big first uploads.** The first backup to B2 or S3 can take hours. Progress is logged every 30 seconds; do not stop it because it looks quiet.
- **Shared Proxmox storage.** If the guest dumps land on storage your script also writes to, Ballast leaves the script's dumps alone, but the storage can fill up twice as fast. Give Ballast its own storage if you can.
- **Same schedule.** Two jobs stopping the same container at the same minute is confusing. Give Ballast a different hour while you compare.
