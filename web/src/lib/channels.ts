// Notification channels (ntfy, webhooks, etc) let Ballast tell you about
// backups. This file keeps the plain-English labels, per-type hints, and
// credential shaping in one place as pure functions so they're easy to test.
import type { ChannelInput } from "@/api/types";

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
): ChannelInput {
  const input: ChannelInput = {
    name,
    type: "ntfy",
    url,
    apply_to_all: applyToAll,
    services,
  };

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
