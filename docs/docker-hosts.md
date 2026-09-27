# Docker hosts

A Docker host is the Docker daemon (the server running your containers) that Ballast talks to when it needs to back up a service. Every service belongs to one host.

## Built-in local host

Ballast always has a host named `local` that talks to your own machine's Docker daemon via its socket. You can't remove it; all existing services use it.

If Ballast runs in a container itself or inside LXC, mount `/var/run/docker.sock` from the host into your Ballast container, and the `local` host continues to work.

For advanced setups (different socket location), you can configure the socket path in `ballast.yaml` — ask in the community forums if you need this.

## Remote hosts: SSH (recommended for homelabs)

A `remote-ssh` host connects via SSH to another machine running Docker. This is the simplest way to back up a Docker machine on your LAN.

### Setting up SSH

**On the Ballast machine:**

Create an SSH key that has no passphrase — Ballast runs unattended and cannot prompt for passphrases:

```bash
ssh-keygen -t ed25519 -N '' -f ballast_key
```

This creates `ballast_key` (private) and `ballast_key.pub` (public).

**On the Docker machine (where the service runs):**

Append the public key to the Docker user's `~/.ssh/authorized_keys`:

```bash
cat ballast_key.pub >> ~/.ssh/authorized_keys
```

Then make sure that user can use Docker:

```bash
sudo usermod -aG docker <username>
sudo usermod -aG docker $(whoami)  # if the user is you
```

Log out and back in so the Docker group takes effect.

**Add the host to Ballast:**

```bash
ballast host add nas --kind remote-ssh \
  --address ballast@192.168.1.20 \
  --ssh-key-file ballast_key
```

Or leave `--ssh-key-file` empty to use Ballast's own SSH keys or agent.

### Host key verification

The first time Ballast connects to a new SSH host, it remembers that machine's SSH host key in its own `known_hosts` file (stored under `<data dir>/ssh/known_hosts`, shared with SFTP destinations). If the machine is later reinstalled or its SSH key regenerates, Ballast will refuse to connect — delete that host's line from that file to try again.

Ballast connects in batch mode and never prompts interactively.

## Remote hosts: TLS (Docker daemon on TCP)

A `remote-tls` host connects to Docker's TCP port (default 2376) with TLS certificates, which requires three files: the CA certificate (`ca.pem`), a client certificate (`cert.pem`) and its key (`key.pem`).

### Generating TLS credentials

Follow Docker's official guide: [Protect the Docker daemon socket](https://docs.docker.com/engine/security/protect-daemon-socket/).

Once you have `ca.pem`, `cert.pem`, and `key.pem`, add the host:

```bash
ballast host add media --kind remote-tls \
  --address 192.168.1.30 \
  --tls-ca ca.pem \
  --tls-cert cert.pem \
  --tls-key key.pem
```

The certificate files are stored in root-only credentials files (permission `0600`) — they never appear in the database or in any API response.

## Testing and managing hosts

When you save a new host, Ballast tests it automatically: it asks the Docker daemon for its version, which proves the address is correct, your key works, and you have permission to use Docker.

```bash
ballast host test <name>          # Test an existing host
ballast host list                 # Show all hosts
ballast host remove <name>        # Delete a host (refused while a service uses it)
```

Use `--no-test` or "Save without testing" in the web UI if a machine is currently offline.

## Attaching a service to a host

When you add a service, specify which host it runs on:

```bash
ballast service add nextcloud-db \
  --catalog postgres \
  --destination b2 \
  --host nas
```

Add the same `--param`/`--container` options the catalog entry needs on any host; `--host` only changes where its Docker commands run. Or select the host in the web UI when adding a service. Container detection then lists that host's containers.

## What runs on the remote machine

Ballast runs these commands on the remote machine, but nothing is installed there:

- `docker ps` — to discover and list containers
- `docker exec` — to run database dumps (e.g., `pg_dump`)
- `docker stop` and `docker start` — for stop-start services and stopped-mode volume backups
- `docker volume ls` / `docker volume inspect` — to list volumes in the web UI and check a volume exists before backing it up
- A temporary `alpine` helper container (`docker run --rm`, no network, volume mounted read-only) — to copy a Docker volume into the backup. The image is pulled once on that machine the first time a volume is backed up.
- Temporary restore containers — created and destroyed during verification

## Troubleshooting

| Problem | Fix |
|---|---|
| "SSH key refused" | Check that the key's public part was added to the Docker user's `~/.ssh/authorized_keys` on the remote machine. |
| "Docker is not installed on that machine" | Install Docker on the remote machine. |
| "Permission denied" | The user can't use Docker there. Run `sudo usermod -aG docker <username>` on the remote machine and have the user log out and back in. |
| "SSH host key changed" | The machine was reinstalled. Delete the host's line from `<data dir>/ssh/known_hosts` and try again. |
| "Hostname could not be resolved" | The address you typed doesn't resolve. Check the IP or hostname. |
| "Connection refused" or "Network unreachable" | A firewall is blocking the connection. Check that SSH (port 22) or Docker TLS (port 2376 by default) is reachable from your Ballast machine to the remote machine. |
| "TLS: unknown authority" or "bad certificate" | Your TLS certificates are wrong. Regenerate them with Docker's official guide. |
| "certificate is valid for" | The certificate was issued for a different hostname than the one you're connecting to. Regenerate it with the correct hostname. |
