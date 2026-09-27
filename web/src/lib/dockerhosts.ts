// Docker hosts let Ballast back up containers running on a machine
// other than the one Ballast itself runs on (spec §17.2). Most
// homelab users only ever have "local"; this file keeps the
// plain-English labels, per-kind address hints and credential
// shaping in one place, shared by the Docker hosts page and any
// service form's host picker, as pure functions so they're easy to
// test.
import type { DockerHostCredentials, DockerHostKind } from "@/api/types";

export interface DockerHostKindInfo {
  id: DockerHostKind;
  label: string;
  hint?: string;
}

/** Every kind of Docker host, in the order the picker offers them. "local" is not offered when adding a new host — it always exists already. */
export const dockerHostKinds: DockerHostKindInfo[] = [
  { id: "remote-ssh", label: "Another machine over SSH", hint: "Recommended for homelabs: no certificates to manage." },
  { id: "remote-tls", label: "Another machine over TLS", hint: "Needs a CA certificate and a client certificate/key." },
];

/** The friendly label for a host kind, e.g. for a list row. */
export function kindLabel(kind: DockerHostKind): string {
  switch (kind) {
    case "local":
      return "This machine";
    case "remote-ssh":
      return "Remote (SSH)";
    case "remote-tls":
      return "Remote (TLS)";
    default:
      return kind;
  }
}

/** Placeholder text for the address field, specific to the chosen kind. */
export function addressPlaceholder(kind: DockerHostKind): string {
  switch (kind) {
    case "remote-ssh":
      return "user@192.168.1.20";
    case "remote-tls":
      return "192.168.1.20:2376";
    default:
      return "(default socket)";
  }
}

/** Help text shown under the address field, specific to the chosen kind. */
export function addressHelp(kind: DockerHostKind): string {
  switch (kind) {
    case "remote-ssh":
      return "The user and host/IP to connect to, e.g. user@192.168.1.20 or ssh://user@192.168.1.20:22. That user's account needs SSH access and permission to run docker.";
    case "remote-tls":
      return "The host name or IP of the Docker machine, e.g. 192.168.1.20 or 192.168.1.20:2376 (2376 is the default Docker TLS port).";
    default:
      return "Leave empty to use the default Docker socket on this machine. Only set this if Ballast's own Docker is on a custom socket path.";
  }
}

/** Which credential fields a kind needs: an SSH key, three TLS PEMs, or none. */
export function credentialKind(kind: DockerHostKind): "ssh" | "tls" | "none" {
  switch (kind) {
    case "remote-ssh":
      return "ssh";
    case "remote-tls":
      return "tls";
    default:
      return "none";
  }
}

/** Raw form values a Docker host form can collect. */
export interface DockerHostFormFields {
  ssh_private_key?: string;
  ca_cert?: string;
  client_cert?: string;
  client_key?: string;
}

/**
 * Builds the credentials object to send for `kind`, keeping only the
 * fields that kind uses and dropping empty ones. Returns undefined
 * when nothing was entered, so an edit that leaves credentials blank
 * keeps whatever the server already has instead of clearing it.
 */
export function buildCredentials(kind: DockerHostKind, raw: DockerHostFormFields): DockerHostCredentials | undefined {
  const creds: DockerHostCredentials = {};
  if (kind === "remote-ssh") {
    if (raw.ssh_private_key && raw.ssh_private_key.trim() !== "") creds.ssh_private_key = raw.ssh_private_key.trim();
  } else if (kind === "remote-tls") {
    if (raw.ca_cert && raw.ca_cert.trim() !== "") creds.ca_cert = raw.ca_cert.trim();
    if (raw.client_cert && raw.client_cert.trim() !== "") creds.client_cert = raw.client_cert.trim();
    if (raw.client_key && raw.client_key.trim() !== "") creds.client_key = raw.client_key.trim();
  }
  return Object.keys(creds).length ? creds : undefined;
}
