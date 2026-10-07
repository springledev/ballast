// The "replace a script" checklist shown on the dashboard once services
// exist. Ballast cannot see the old script or a manual test restore, so
// the steps are ticked by hand; the only thing worked out here is when
// it is safe to retire the script. Pure functions so the rules are easy
// to test.
import type { Light } from "@/lib/status";

/** What the person has ticked for one service. */
export interface ScriptProgress {
  parallel: boolean;
  sevenDays: boolean;
  restored: boolean;
}

/** One step for one service. */
export interface ScriptStep {
  key: keyof ScriptProgress | "retire";
  label: string;
  done: boolean;
  /** False when the step cannot be ticked yet. */
  enabled: boolean;
  hint?: string;
}

/** An empty progress record. */
export function emptyProgress(): ScriptProgress {
  return { parallel: false, sevenDays: false, restored: false };
}

/**
 * Returns the steps for one service. The last step (retire the script)
 * unlocks only when the service is green right now and the other three
 * are done, so a red or amber service can never be told it is safe.
 */
export function scriptSteps(progress: ScriptProgress, light: Light): ScriptStep[] {
  const green = light === "green";
  const ready = progress.parallel && progress.sevenDays && progress.restored && green;
  return [
    { key: "parallel", label: "Run Ballast next to your script", done: progress.parallel, enabled: true },
    {
      key: "sevenDays",
      label: "Seven green days in a row",
      done: progress.sevenDays,
      enabled: progress.parallel,
      hint: green ? undefined : "This service is not green right now.",
    },
    {
      key: "restored",
      label: "Do one test restore from Ballast",
      done: progress.restored,
      enabled: progress.parallel,
    },
    {
      key: "retire",
      label: "Switch your old script off",
      done: false,
      enabled: ready,
      hint: ready ? "Everything checks out. You can turn the script off." : "Finish the steps above first, and keep the light green.",
    },
  ];
}

/** True when the service is ready for its script to be retired. */
export function readyToRetire(progress: ScriptProgress, light: Light): boolean {
  return scriptSteps(progress, light).find((s) => s.key === "retire")!.enabled;
}

/** Parses stored progress, tolerating anything unexpected. */
export function parseProgress(raw: string | null): Record<string, ScriptProgress> {
  if (!raw) return {};
  try {
    const v = JSON.parse(raw) as Record<string, Partial<ScriptProgress>>;
    const out: Record<string, ScriptProgress> = {};
    for (const [k, p] of Object.entries(v ?? {})) {
      out[k] = { parallel: p?.parallel === true, sevenDays: p?.sevenDays === true, restored: p?.restored === true };
    }
    return out;
  } catch {
    return {};
  }
}
