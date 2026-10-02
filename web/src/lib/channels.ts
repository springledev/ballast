// Notification channels (ntfy, webhooks, etc) let Ballast tell you about
// backups. This file keeps the plain-English labels, per-type hints, and
// credential shaping in one place as pure functions so they're easy to test.
import type { ChannelInput, ChannelType, EmailSettings } from "@/api/types";

/** Raw form values a notification channel form can collect. */
export interface ChannelFormFields {
  url?: string;
  username?: string;
  token?: string;
  clear_token?: boolean;
}

/**
 * Builds the channel input to send, keeping only the fields that apply
 * and handling write-only credential fields properly.
 */
export function buildChannelInput(
  name: string,
  url: string,
  username: string | undefined,
  token: string | undefined,
  clearToken: boolean,
  applyToAll: boolean,
  services: string[],
  extra: { type?: ChannelType; email?: EmailSettings; events?: string[]; tiers?: string[]; digest?: boolean } = {},
): ChannelInput {
  const input: ChannelInput = {
    name,
    type: extra.type ?? "ntfy",
    apply_to_all: applyToAll,
    services,
    events: extra.events ?? [],
    tiers: extra.tiers ?? [],
  };
  if (extra.digest !== undefined) {
    input.digest = extra.digest;
  }
  // Blank means "keep the stored address" when editing; email has none.
  if (input.type !== "email" && url.trim() !== "") {
    input.url = url.trim();
  }
  if (input.type === "email" && extra.email) {
    input.email = extra.email;
  }

  if (username && username.trim() !== "") {
    input.username = username.trim();
  }

  if (token && token.trim() !== "") {
    input.token = token.trim();
  }

  if (clearToken) {
    input.clear_token = true;
  }

  return input;
}

/**
 * Default ntfy URL for new channels.
 */
export const defaultNtfyUrl = "https://ntfy.sh/";
