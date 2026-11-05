// Plain-English wording for backup status, shared by every view so the
// same state always reads the same way. The traffic light itself comes
// from the server; this only decides how to say it.
import type { ServiceStatus, TierStatus } from "@/api/types";
import { formatAgeSeconds, formatRelative } from "./format";

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
      if (t.last_state === "CANCELLED") {
        return t.last_finished_at
          ? `The last backup was stopped before it finished (${when}). The backup before it is kept.`
          : "The last backup was stopped before it finished. The backup before it is kept.";
      }
      return t.last_finished_at ? `The last backup failed (${when}).` : "The last backup failed.";
    case "running":
      if (t.cancelling) return "Stopping the backup and tidying up… The last good backup is kept.";
      if (t.waiting) return `Backing up now, ${t.waiting}…`;
      return t.phase ? `Backing up now: ${t.phase}…` : "Backing up now…";
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

/** True while a service's running backup has been asked to stop and is tidying up. */
export function isCancelling(s: ServiceStatus): boolean {
  return s.tiers.some((t) => t.light === "running" && !!t.cancelling);
}

/** The question asked before stopping a backup; the safe answer is focused first. */
export const cancelQuestion = {
  message: "Stop this backup? The last good backup is kept.",
  confirmLabel: "Stop backup",
  cancelLabel: "Keep running",
};

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

/** The pieces of a compact status line: the light, its age, and whether the age deserves a cue. */
export interface StatusParts {
  /** The light in words, or the whole line when there is no age, e.g. "Safe" or "Uploading, 44%". */
  lead: string;
  /** How long ago the last backup finished, e.g. "6 days ago". */
  age?: string;
  /** True when the age is long for how often the backup is meant to run. */
  late: boolean;
}

/**
 * How often a backup is meant to run, in seconds, read from its
 * schedule. Undefined when there is no schedule or it can't be read
 * (an unscheduled backup has no expected interval).
 */
export function expectedIntervalSeconds(cron?: string): number | undefined {
  const f = (cron ?? "").trim().split(/\s+/);
  if (f.length !== 5) return undefined;
  const [, hour, dom, , dow] = f;
  const count = (field: string) => field.split(",").length;
  const day = 24 * 3600;
  if (dow !== "*") return (7 * day) / count(dow);
  if (dom !== "*") return (30 * day) / count(dom);
  if (hour === "*") return 3600;
  const step = /^\*\/(\d+)$/.exec(hour);
  if (step) return Number(step[1]) * 3600;
  if (/^[\d,]+$/.test(hour)) return day / count(hour);
  return undefined;
}

/**
 * True when a safe (green) backup is old for how often it should run:
 * a daily one once a run has been missed, a longer one once it passes
 * half its interval. Only a cue for the reader; it never changes the
 * light.
 */
export function ageIsLate(s: ServiceStatus, now: number = Date.now()): boolean {
  return shortStatusParts(s, now).late;
}

/**
 * A few words for one line of a compact list, split so the age can be
 * styled on its own, e.g. "Safe" and "2 hours ago".
 */
export function shortStatusParts(s: ServiceStatus, now: number = Date.now()): StatusParts {
  const light = worstLight(s);
  if (light === "running") {
    if (isCancelling(s)) return { lead: "Cancelling…", late: false };
    const up = (s.targets ?? []).find((t) => t.upload && t.upload.total_bytes > 0)?.upload;
    if (up) return { lead: `Uploading, ${Math.floor(up.percent)}%`, late: false };
    const phase = s.tiers.find((t) => t.light === "running")?.phase;
    return { lead: phase ? `Backing up now, ${phase}` : "Backing up now", late: false };
  }
  if (light === "none") return { lead: lightLabel.none, late: false };
  const finished = s.tiers
    .map((t) => t.last_finished_at)
    .filter((t): t is string => !!t)
    .sort()
    .pop();
  if (!finished) return { lead: lightLabel[light], late: false };
  const seconds = (now - new Date(finished).getTime()) / 1000;
  const interval = expectedIntervalSeconds(s.schedule?.cron);
  const day = 24 * 3600;
  const limit = interval === undefined ? undefined : interval <= day ? interval : interval / 2;
  const late = light === "green" && limit !== undefined && seconds > limit;
  return { lead: lightLabel[light], age: formatAgeSeconds(seconds), late };
}

/**
 * A few words for one line of a compact list, e.g. "Safe, 2 hours ago"
 * or "Uploading, 44%". The full sentence is on the card view.
 */
export function shortStatus(s: ServiceStatus): string {
  const p = shortStatusParts(s);
  return p.age ? `${p.lead}, ${p.age}` : p.lead;
}
