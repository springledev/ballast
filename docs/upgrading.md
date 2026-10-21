# Upgrading Ballast

Upgrading means replacing the `ballast` program with a newer one and starting it again. Ballast updates its own database the first time the new version starts, and your services, schedules, destinations and snapshots carry on as before.

It takes about five minutes. Do it by hand the first time; there is no automatic `ballast install --upgrade` yet, on purpose, until this guide has been used enough to trust.

The paths below are the ones `ballast install` uses (`/var/lib/ballast` for data, `/usr/local/bin/ballast` for the program, a systemd service called `ballast`). If you put things elsewhere, use your own paths.

## Before you start

1. See which version you run now, and that Ballast is healthy:

   ```bash
   ballast version
   ballast doctor
   ```

   The first lines of `doctor` show the Ballast version and the database version. Write both down in case you need to go back.

2. Wait until no backup is running. Restarting in the middle of a backup does not lose it (an interrupted backup is shown as amber and the next one runs normally), but it wastes the night's work, so avoid it:

   ```bash
   ballast wait-idle --timeout 30m
   ```

   This returns as soon as nothing is running. A backup counts as running until Ballast has finished with it completely, including the tidy-up of old snapshots that follows a finished backup (on a remote destination that can take a while). It exits with an error if something is still running after 30 minutes. To only look, without waiting, use `ballast status --running`: it prints what is running and exits with 1 if anything is, or prints "Nothing is running." and exits with 0.

## Upgrade

1. Stop Ballast, so nothing starts while you swap the program:

   ```bash
   sudo systemctl stop ballast
   ```

2. Copy the database. This is your way back, so do not skip it:

   ```bash
   sudo cp -a /var/lib/ballast/ballast.db /var/lib/ballast/ballast.db.before-upgrade
   ```

   If `ballast.db-wal` or `ballast.db-shm` files sit next to it, copy them too. (They are normally empty once Ballast has stopped cleanly.)

3. Keep the old program, then put the new one in place:

   ```bash
   sudo cp -a /usr/local/bin/ballast /usr/local/bin/ballast.old
   sudo install -m 0755 bin/ballast /usr/local/bin/ballast
   ```

   To build the new program from source, run `make` in the Ballast folder: it writes `bin/ballast`. `make` stamps the version from `git describe --tags --always --dirty`, so a build from a tagged release says `v0.5.0`, and a build from any other commit says the commit it came from (with `-dirty` added if you changed files).

4. Start it and check:

   ```bash
   sudo systemctl start ballast
   ballast version
   ballast doctor
   ```

   `doctor` should now show the new Ballast version, a database version the same as or higher than before, and no new red lines. Look at the dashboard (or `ballast status`) the next morning to see that the scheduled backups ran and are green.

## If something is wrong: going back

The new version moves the database forward and does not move it back, so going back means putting the copy back too:

```bash
sudo systemctl stop ballast
sudo cp -a /usr/local/bin/ballast.old /usr/local/bin/ballast
sudo cp -a /var/lib/ballast/ballast.db.before-upgrade /var/lib/ballast/ballast.db
sudo systemctl start ballast
ballast doctor
```

Any backups made between the upgrade and the rollback are still safely in your destinations. Only Ballast's own record of them is lost, and you can see them with `ballast snapshots`.

Each database change says in its comment (in `internal/store/migrations`) whether it can be rolled back by itself. Most only add things an older Ballast ignores, but a few rewrite existing data, so the copy from step 2 is the only method that is always safe.

## For maintainers: tagging a release

A release is a git tag, and tagging is something the maintainer does by hand; nothing in the code does it:

```bash
git tag v0.5.0
git push origin v0.5.0
```

Build after tagging, so `make` picks the tag up. Until a commit is tagged, builds report the commit instead, never just `dev`.
