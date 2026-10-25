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

const dayNames = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

/**
 * Plain English for the cron shapes people actually use (a fixed time
 * daily, on weekdays or on one day of the week, or every N hours), or
 * undefined for anything else so the caller can show the raw expression.
 */
export function describeCron(cron: string): string | undefined {
  const f = cron.trim().split(/\s+/);
  if (f.length !== 5) return undefined;
  const [m, h, dom, mon, dow] = f;
  if (dom !== "*" || mon !== "*") return undefined;
  const digits = /^\d{1,2}$/;
  if (digits.test(m) && digits.test(h) && +m < 60 && +h < 24) {
    const at = `${+h}:${m.padStart(2, "0")}`;
    if (dow === "*") return `Every day at ${at}`;
    if (dow === "1-5") return `Weekdays at ${at}`;
    if (/^[0-7]$/.test(dow)) return `Every ${dayNames[+dow % 7]} at ${at}`;
    return undefined;
  }
  if (m === "0" && dow === "*") {
    if (h === "*") return "Every hour";
    const step = /^\*\/(\d{1,2})$/.exec(h);
    if (step && +step[1] > 1 && +step[1] < 24) return `Every ${step[1]} hours`;
  }
  return undefined;
}

/** How a schedule reads to a person. */
export function describeSchedule(s?: { cron: string; enabled: boolean }): string {
  if (!s) return "Only when you start it";
  const base = presetFor(s.cron)?.label ?? describeCron(s.cron) ?? `Custom (${s.cron})`;
  return s.enabled ? base : `${base} (paused)`;
}
