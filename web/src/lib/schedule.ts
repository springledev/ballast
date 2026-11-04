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

const dayShort = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const monthShort = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/**
 * A next-run time in plain words, in the viewer's local time: "today
 * 18:30", "tomorrow 03:00", "Tue 03:00" (within the coming week) or
 * "Mon 16 Nov 03:00". Empty when there is no next run (paused).
 */
export function formatNextRun(iso: string | undefined, now: Date = new Date()): string {
  if (!iso) return "";
  const when = new Date(iso);
  if (Number.isNaN(when.getTime())) return "";
  const at = `${String(when.getHours()).padStart(2, "0")}:${String(when.getMinutes()).padStart(2, "0")}`;
  const startOf = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  const days = Math.round((startOf(when) - startOf(now)) / 86_400_000);
  if (days <= 0) return `today ${at}`;
  if (days === 1) return `tomorrow ${at}`;
  if (days < 7) return `${dayShort[when.getDay()]} ${at}`;
  return `${dayShort[when.getDay()]} ${when.getDate()} ${monthShort[when.getMonth()]} ${at}`;
}

/**
 * When an enabled schedule runs, in plain words: "Next run: Tue 03:00",
 * or "It will run at the next check, then Tue 03:00" when a missed slot
 * makes the next check start a backup at once. Empty when paused. The
 * Schedules page uses it for both the saved banner and each row.
 */
export function nextRunSentence(res: { enabled: boolean; next_run?: string; due_now?: boolean }, now: Date = new Date()): string {
  if (!res.enabled || !res.next_run) return "";
  const next = formatNextRun(res.next_run, now);
  return res.due_now ? `It will run at the next check, then ${next}` : `Next run: ${next}`;
}

/** The sentence confirming a saved schedule, in plain words. */
export function savedMessage(res: { enabled: boolean; next_run?: string; due_now?: boolean }, now: Date = new Date()): string {
  const when = nextRunSentence(res, now);
  return when ? `Saved. ${when}.` : "Saved. Paused, so it will not run.";
}

/**
 * What to send as `enabled` when saving a timing. A single save of a
 * paused schedule means "turn it back on" (true); a bulk save never
 * changes paused state, so it sends nothing (undefined).
 */
export function enabledToSend(wasPaused: boolean, bulk: boolean): boolean | undefined {
  return wasPaused && !bulk ? true : undefined;
}
