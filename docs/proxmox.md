# Proxmox guide

If you run Proxmox, Ballast can back up whole guests (VMs and LXC containers) with Proxmox's own `vzdump`, and it can back up the services *inside* those guests too. Ballast checks each layer separately, so a good guest backup never hides a broken database backup, or the other way round.

Ballast talks to Proxmox through its web API, never over SSH.

## 1. Create an API token

In the Proxmox web UI:

1. Go to **Datacenter → Permissions → Users** and add a user, for example `ballast@pve`.
2. Go to **Datacenter → Permissions → API Tokens** and add a token named `backup` for that user.
3. Copy the secret Proxmox shows you. It's displayed once.
4. Go to **Datacenter → Permissions**, add an API Token Permission on path `/vms`, and give it the `VM.Backup` role (plus `Datastore.AllocateSpace` on the storage you'll back up to, if Proxmox asks for it).

Keep the permissions small. Ballast never needs to create or delete guests.

### Which permissions Ballast needs

Backing up needs `VM.Backup` on `/vms`. To list your guests and to save a copy of each guest's settings, Ballast also needs to read them, so give the token these as well:

- `VM.Audit` on `/vms` (see the guest list and settings)
- `Sys.Audit` on `/` (see the node)
- `Datastore.Audit` on the backup storage, and `Datastore.AllocateSpace` there so dumps can be written

The built-in `PVEAuditor` role covers the three read-only ones.

**A privilege-separated token needs its own permissions.** When you create a token with "Privilege Separation" ticked (the default), it does not inherit anything from its user. Granting roles to `ballast@pve` is not enough: add each permission under **Datacenter → Permissions → Add → API Token Permission** for the token itself. A token with no permissions of its own can still log in, so Ballast may report that the token is accepted while `ballast proxmox guests` says "No guests found". If that happens, this is the first thing to check.

## 2. Connect Ballast

Save the token secret to a file (no trailing whitespace), then:

```bash
ballast proxmox connect \
  --host 192.168.1.10 \
  --token-id ballast@pve!backup \
  --token-secret-file ./token.txt
```

Ballast tests the connection before saving it, so a typo never leaves you with a broken setup. Proxmox uses a self-signed certificate by default. If you haven't replaced it, add `--insecure`.

Check it any time:

```bash
ballast proxmox test
ballast proxmox guests
```

The web UI has the same thing under **Proxmox**.

## 3. Back up a guest

```bash
ballast proxmox backup --node pve1 --vmid 101 --storage local --mode snapshot --keep-last 3
```

| Option | What it does |
| --- | --- |
| `--mode snapshot` | Guest keeps running (default). |
| `--mode suspend` | Pauses the guest briefly. |
| `--mode stop` | Shuts the guest down while it's copied. |
| `--keep-last N` | Keeps the newest N dumps of that guest and deletes older ones. `0` keeps everything. |

Ballast starts the job, waits for it to finish, and reports the result.

### Copies and "keep last"

A scheduled job's `--keep-last N` applies to its copies too: with `--copy-to b2 --keep-last 2`, B2 also keeps only the newest 2 copies. After `ballast proxmox job add` or `edit`, Ballast prints how many backups are kept on the Proxmox storage and in each copy destination. To give a copy its own count, use `ballast service retention <job> --keep-last N ...`; that setting wins over the job's.

### Compression and copies

Ballast asks Proxmox for zstd-compressed dumps. Compressed files barely dedupe, so every weekly dump uploads in full (about the size of the guest each time). Uncompressed dumps dedupe far better, so a copy of one only uploads what changed. Ballast does not yet let you choose compression per job; until it does, keep `--keep-last` small for big guests.

## 4. Back up the services inside a guest

Attach a catalog service (Vaultwarden, Postgres, a plain folder…) to a guest, in the web UI under **Proxmox → guests**.

- **A Docker guest**: the service uses that guest's Docker host. Set it up as described in [Docker hosts](docker-hosts.md).
- **A plain LXC container**: Ballast runs the dump with `pct exec` through the Proxmox API.

The order is: dump inside the guest, send the dump to your restic destination, then take the `vzdump` of the whole guest.

## 5. What "verified" means here

Two independent checks must both pass, and each shows its own light:

- **Guest layer**: Ballast confirms the guest's configuration can be restored from its `vzdump`.
- **Service layer**: the service's own backup goes through its usual restore test (for example, a throwaway database restore).

If one fails, the other is still reported. Nothing is marked green until both have been proven restorable.

## What Ballast does inside your guests

Ballast only *reads or dumps* inside a guest: `docker exec` to run a dump, or `pct exec` to run a dump command. It doesn't install anything, edit files or change settings there. This is the one place Ballast reaches into your other machines, so it's worth knowing.

## Running Ballast itself on Proxmox

**In an LXC container (recommended for homelabs)**

- Use an unprivileged container.
- Turn on `nesting=1,keyctl=1` if the restore tests will run Docker inside it.
- On ZFS storage, give Docker its own directory, or use fuse-overlayfs, so Docker's layers don't clash with ZFS.
- If Ballast backs up Docker on the *same* machine, mount `/var/run/docker.sock` into it. For Docker on other machines, nothing needs mounting; use a [remote host](docker-hosts.md).

**In a VM**

- Simpler and better isolated, needs about 2 GB of RAM, and has none of the nesting caveats. A good default if you'd rather not think about Docker inside LXC.

## Checking your setup

```bash
ballast doctor
```

This checks restic, each Docker host, the Proxmox connection, your destinations, and free disk space for restore tests, and says in plain English what to fix.

## Backing up the Proxmox host's own files

The settings snapshot covers `/etc/pve`, but the host also has files such as `/etc/fstab`, `/etc/exports` and `/etc/nfs.conf`. Ballast can copy these over SSH without installing anything on the Proxmox host (it only needs `sshd` and `tar`):

1. Add the Proxmox host under Docker hosts as an SSH host (the host does not need Docker).
2. Add a service from the "Files from another machine" entry on that host and list the files, separated by commas.

It only reads, only from `/etc`, `/usr/local`, `/opt`, `/srv` and `/var/spool/cron`, and leaves secrets out unless you opt in under "secrets": `tailscale-node-key`, `ssh-host-keys` or `password-hashes`. The run log says what was left out.
