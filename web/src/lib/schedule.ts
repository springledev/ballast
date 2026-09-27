// Schedules are stored as cron expressions (what the server and CLI
// use), but most people only ever need a handful of choices. These
// presets are what the simple view offers; anything else is "custom"
// and shown as its raw expression in the advanced view.

export interface SchedulePreset {
  id: string;
  label: string;
  cron: string;
}

export const schedulePresets: SchedulePreset[] = [
  { id: "nightly", label: "Every night at 3:00", cron: "0 3 * * *" },
  { id: "weekly", label: "Every Sunday at 3:00", cron: "0 3 * * 0" },
  { id: "6h", label: "Every 6 hours", cron: "0 */6 * * *" },
  { id: "hourly", label: "Every hour", cron: "0 * * * *" },
];

/** The preset matching cron, or undefined for a custom expression. */
export function presetFor(cron: string | undefined): SchedulePreset | undefined {
  const norm = (cron ?? "").trim().replace(/\s+/g, " ");
  return schedulePresets.find((p) => p.cron === norm);
}

/** How a schedule reads to a person. */
export function describeSchedule(s?: { cron: string; enabled: boolean }): string {
  if (!s) return "Only when you start it";
  const base = presetFor(s.cron)?.label ?? `Custom (${s.cron})`;
  return s.enabled ? base : `${base} (paused)`;
}
