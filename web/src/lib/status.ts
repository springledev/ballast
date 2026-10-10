// Plain-English wording for backup status, shared by every view so the
// same state always reads the same way. The traffic light itself comes
// from the server; this only decides how to say it.
import type { ServiceStatus, TierStatus } from "@/api/types";
import { formatRelative } from "./format";

export type Light = TierStatus["light"];

/** Short label for a light, e.g. on a badge. */
export const lightLabel: Record<Light, string> = {
  green: "Safe",
  amber: "Not checked",
  red: "Failed",
  running: "Backing up",
  none: "No backup yet",
};

/** One sentence describing a tier's state for a person, not a log. */
export function describeTier(t: TierStatus): string {
  const when = formatRelative(t.last_finished_at);
  if (t.stale && t.light === "amber" && t.last_state === "SUCCESS") {
    const verified = t.last_verified_at ? formatRelative(t.last_verified_at) : "a long time ago";
    return `No backup has been checked recently. The last checked backup was ${verified}, which is older than it should be.`;
  }
  switch (t.light) {
    case "green":
      return `Backed up and checked ${when}. It can be restored.`;
    case "amber":
      return `Backed up ${when}, but the check didn't pass. Don't rely on this backup until it's checked again.`;
    case "red":
      return t.last_finished_at ? `The last backup failed (${when}).` : "The last backup failed.";
    case "running":
      return t.waiting ? `Backing up now, ${t.waiting}…` : "Backing up now…";
    default:
      return "Hasn't been backed up yet.";
  }
}

/**
 * The worst light across every tier of a service: a failing copy must
 * never hide behind a healthy one.
 */
export function worstLight(s: ServiceStatus): Light {
  const order: Light[] = ["red", "amber", "none", "running", "green"];
  let worst: Light = "green";
  for (const t of s.tiers) {
    if (order.indexOf(t.light) < order.indexOf(worst)) worst = t.light;
  }
  return s.tiers.length ? worst : "none";
}

/** Headline for the overview: how many services need attention. */
export function summarize(services: ServiceStatus[]): { light: Light; text: string } {
  if (services.length === 0) return { light: "none", text: "Nothing is being backed up yet." };
  const lights = services.map(worstLight);
  const failed = lights.filter((l) => l === "red").length;
  const unchecked = lights.filter((l) => l === "amber").length;
  const never = lights.filter((l) => l === "none").length;
  const problems = failed + unchecked + never;
  if (problems === 0) {
    const running = lights.filter((l) => l === "running").length;
    if (running > 0) return { light: "running", text: `Backing up ${running} of ${services.length} now.` };
    return {
      light: "green",
      text: services.length === 1 ? "Your backup is safe." : `All ${services.length} backups are safe.`,
    };
  }
  const parts: string[] = [];
  if (failed) parts.push(`${failed} failed`);
  if (unchecked) parts.push(`${unchecked} not checked`);
  if (never) parts.push(`${never} not backed up yet`);
  const light: Light = failed ? "red" : unchecked ? "amber" : "none";
  return { light, text: `${problems} of ${services.length} need${problems === 1 ? "s" : ""} attention: ${parts.join(", ")}.` };
}

/** What each verification check actually did, in plain words. */
export const checkLabels: Record<string, string> = {
  "restic-read-back": "Read files back out of the stored backup",
  "archive-integrity": "Checked the backup archive is complete and readable",
  "container-restore": "Restored the database into a temporary copy and checked it",
  "db-restore-drill": "Restored the database into a temporary copy and checked it",
  "restart-smoke": "Checked the service came back up after the backup",
};

export function describeCheck(method: string): string {
  return checkLabels[method] ?? method;
}

/**
 * A few words for one line of a compact list, e.g. "Safe, 2 hours ago"
 * or "Uploading, 44%". The full sentence is on the card view.
 */
export function shortStatus(s: ServiceStatus): string {
  const light = worstLight(s);
  if (light === "running") {
    const up = (s.targets ?? []).find((t) => t.upload && t.upload.total_bytes > 0)?.upload;
    return up ? `Uploading, ${Math.floor(up.percent)}%` : "Backing up now";
  }
  if (light === "none") return lightLabel.none;
  const finished = s.tiers
    .map((t) => t.last_finished_at)
    .filter((t): t is string => !!t)
    .sort()
    .pop();
  return finished ? `${lightLabel[light]}, ${formatRelative(finished)}` : lightLabel[light];
}
