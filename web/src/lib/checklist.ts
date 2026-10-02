// The first-run checklist shown on the dashboard until a new install has
// a destination, a service, a notification channel and one verified
// backup. Pure functions so the rules are easy to test.
import type { ServiceStatus } from "@/api/types";

/** What the dashboard knows about the install so far. */
export interface ChecklistInput {
  destinations: number;
  services: number;
  channels: number;
  anyVerified: boolean;
}

/** One step of the first-run checklist. */
export interface ChecklistItem {
  key: "destination" | "service" | "notifications" | "verified-backup";
  label: string;
  done: boolean;
  /** The screen where this step can be done. */
  to: string;
}

/** Returns the four first-run steps in the order a beginner should do them. */
export function firstRunChecklist(input: ChecklistInput): ChecklistItem[] {
  return [
    { key: "destination", label: "Add a destination", done: input.destinations > 0, to: "/destinations" },
    { key: "service", label: "Add a service", done: input.services > 0, to: "/wizard" },
    { key: "notifications", label: "Set up notifications", done: input.channels > 0, to: "/settings" },
    { key: "verified-backup", label: "Run your first verified backup", done: input.anyVerified, to: "/services" },
  ];
}

/** True once every checklist step is done. */
export function checklistComplete(items: ChecklistItem[]): boolean {
  return items.every((i) => i.done);
}

/** True when any service has a copy whose restore check has passed at least once. */
export function anyServiceVerified(statuses: ServiceStatus[]): boolean {
  return statuses.some((s) => s.tiers.some((t) => !!t.last_verified_at));
}
