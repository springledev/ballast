# Security policy

## Reporting a vulnerability

Please report security problems privately, using GitHub's private vulnerability reporting: open the **Security** tab of this repository and choose **Report a vulnerability**. Please do not open a public issue or pull request for a security problem.

Include what you found, how to reproduce it, and which version you used (`ballast version`). Never include real passwords, keys or tokens in a report.

## What counts

Examples of things we want to hear about:

- A secret (password, key, token) showing up in the database, a log, an API response or a support bundle.
- Getting past web UI or API sign-in, or using a trigger token to do more than start its one backup.
- Reading or writing files outside the places Ballast is meant to use.
- Running commands on a Docker host or SSH host that Ballast should not allow.
- A backup marked green when it was not verified.

Ordinary bugs, and problems that need an attacker who already has root on your machine, are better reported as normal issues.

## Supported versions

Ballast is alpha software. Only the latest release is supported, and fixes go into the next release.

## How Ballast handles secrets

- Secrets live in root-only credential files (or systemd credentials) on the machine running Ballast.
- Secrets are never stored in Ballast's database, never written to logs, and never returned by the API.
- Support bundles leave secrets out.
