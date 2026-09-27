// Destination types are shown to people who may not know what a
// "restic repository" or an "S3 endpoint" is, so every type gets a
// plain-English label and only the fields it actually uses. This file
// keeps that mapping in one place, shared by the Destinations page and
// the setup wizard, and kept as pure functions so it's easy to test.
import type { DestinationSettings, DestinationType } from "@/api/types";

export interface DestinationTypeInfo {
  id: DestinationType;
  label: string;
  /** Shown once a type is chosen, above its fields. */
  hint?: string;
  /** Whether this type is only offered behind an "advanced" disclosure. */
  advanced?: boolean;
}

/** Every destination type, in the order the picker offers them. */
export const destinationTypes: DestinationTypeInfo[] = [
  { id: "local", label: "Folder on this machine or NAS mount" },
  { id: "b2", label: "Backblaze B2" },
  { id: "s3", label: "Amazon S3 / S3-compatible (MinIO, Wasabi…)" },
  { id: "r2", label: "Cloudflare R2" },
  { id: "sftp", label: "SFTP server" },
  {
    id: "custom",
    label: "Custom (advanced)",
    hint: "Paste a raw restic repository string, for setups like rest-server or rclone.",
    advanced: true,
  },
];

/** The friendly label for a destination type, e.g. for a list row. */
export function typeLabel(type: DestinationType): string {
  return destinationTypes.find((t) => t.id === type)?.label ?? type;
}

/** Whether a destination type needs backend credentials (a key pair or an SSH key). */
export function needsCredentials(type: DestinationType): boolean {
  return type === "b2" || type === "s3" || type === "r2" || type === "sftp";
}

/** Which credential fields a type needs: an access key pair, an SSH key, or none. */
export function credentialKind(type: DestinationType): "keypair" | "ssh" | "none" {
  switch (type) {
    case "b2":
    case "s3":
    case "r2":
      return "keypair";
    case "sftp":
      return "ssh";
    default:
      return "none";
  }
}

// The settings fields each type accepts. Anything else must be left
// out of the request entirely, or the server rejects it (spec §17.1
// backend.onlyFields) — buildSettings enforces that here so a form
// can bind every field without worrying about which type is active.
const fieldsByType: Record<DestinationType, (keyof DestinationSettings)[]> = {
  local: ["path"],
  s3: ["path", "endpoint", "bucket", "region"],
  b2: ["path", "endpoint", "bucket"],
  r2: ["path", "bucket", "account_id"],
  sftp: ["path", "host", "port", "user"],
  custom: ["url"],
};

/** Raw form values a destination form can collect, one field per key. */
export interface DestinationFormFields {
  path?: string;
  endpoint?: string;
  bucket?: string;
  region?: string;
  account_id?: string;
  host?: string;
  port?: number | string;
  user?: string;
  url?: string;
}

/**
 * Builds the settings object to send for `type`, keeping only the
 * fields that type uses and dropping empty ones, so a form can hold
 * every field for every type without leaking irrelevant or blank
 * values into the request.
 */
export function buildSettings(type: DestinationType, raw: DestinationFormFields): DestinationSettings {
  const settings: Record<string, string | number> = {};
  for (const field of fieldsByType[type]) {
    if (field === "port") {
      const port = typeof raw.port === "string" ? parseInt(raw.port, 10) : raw.port;
      if (port) settings.port = port;
      continue;
    }
    const value = raw[field as keyof Omit<DestinationFormFields, "port">];
    if (typeof value === "string" && value.trim() !== "") {
      settings[field] = value.trim();
    }
  }
  return settings as DestinationSettings;
}
