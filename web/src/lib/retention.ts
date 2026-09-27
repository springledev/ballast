// Retention (spec §14 step 19: keep_daily/keep_weekly/keep_monthly)
// in plain English, shared by the Services view (a service's own
// override, or its destination's policy) and anywhere else that needs
// to say the same thing. Kept dependency-free like the rest of lib/
// (see format.ts).
import type { Retention } from "@/api/types";

/**
 * Describes a retention policy in plain English, e.g. "7 daily, 4
 * weekly and 6 monthly backups". A count of 0 for a unit is left out
 * entirely rather than printed as "0 weekly", since that unit simply
 * isn't part of the policy. "daily"/"weekly"/"monthly" are adjectives
 * here (describing the backups, not the count), so they're never
 * pluralized.
 */
export function describeRetention(r: Retention): string {
  const parts: string[] = [];
  if (r.keep_daily > 0) parts.push(`${r.keep_daily} daily`);
  if (r.keep_weekly > 0) parts.push(`${r.keep_weekly} weekly`);
  if (r.keep_monthly > 0) parts.push(`${r.keep_monthly} monthly`);
  if (parts.length === 0) return "no backups kept";
  if (parts.length === 1) return `${parts[0]} backups`;
  if (parts.length === 2) return `${parts[0]} and ${parts[1]} backups`;
  return `${parts[0]}, ${parts[1]} and ${parts[2]} backups`;
}

/**
 * The full sentence shown next to a service: its effective policy,
 * plus whether that's its own override or inherited from its
 * destination.
 */
export function describeServiceRetention(effective: Retention, destinationName: string, isOverride: boolean): string {
  const policy = `Keeps ${describeRetention(effective)}`;
  return isOverride ? `${policy} (its own setting, not ${destinationName}'s)` : `${policy} (same as ${destinationName})`;
}
