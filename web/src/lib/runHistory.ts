// Plain-English wording for a service's past runs. The traffic light
// comes from the server; this only decides how to say it.
import type { RunSummary, RunVerificationSummary, VerificationRecord } from "@/api/types";

/** A short phrase for how a run ended. */
export function runStatusText(r: RunSummary): string {
  switch (r.light) {
    case "green":
      return "Backed up and checked";
    case "amber":
      return "Backed up, but not checked";
    case "running":
      return "Still running";
    default:
      return r.state === "CANCELLED" ? "Cancelled" : "Failed";
  }
}

/** How long a run took, e.g. "3 min 5 s". */
export function formatRunDuration(seconds?: number): string {
  if (seconds === undefined || seconds === null) return "—";
  if (seconds < 1) return "under a second";
  if (seconds < 60) return `${seconds} second${seconds === 1 ? "" : "s"}`;
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  if (h > 0) return m ? `${h} h ${m} min` : `${h} h`;
  return s ? `${m} min ${s} s` : `${m} min`;
}

/** One line saying how the run's checks went. */
export function verificationLine(v: RunVerificationSummary): string {
  if (v.checks === 0) return "No checks recorded";
  const warn = v.warnings ? `, ${v.warnings} with a warning` : "";
  if (v.failed === 0 && v.passed === v.checks) {
    if (v.checks === 1 && !v.warnings) return "1 check passed";
    return `All ${v.checks} checks passed${warn}`;
  }
  const failed = v.failed ? `, ${v.failed} failed` : "";
  return `${v.passed} of ${v.checks} checks passed${failed}${warn}`;
}

/** The result of one check in plain words. A failure outranks a warning. */
export function checkOutcome(r: VerificationRecord): { kind: "pass" | "warning" | "fail"; text: string } {
  if (!r.passed) return { kind: "fail", text: "Failed" };
  if (r.warning) return { kind: "warning", text: "Passed with a warning" };
  return { kind: "pass", text: "Passed" };
}

/** A set of checks that belong together, e.g. one destination's. */
export interface CheckGroup {
  title: string;
  checks: VerificationRecord[];
}

/** Groups checks by destination, whole-backup checks first. */
export function groupChecks(records: VerificationRecord[]): CheckGroup[] {
  const whole: VerificationRecord[] = [];
  const byTarget = new Map<string, VerificationRecord[]>();
  for (const r of records) {
    if (!r.target) {
      whole.push(r);
      continue;
    }
    const list = byTarget.get(r.target) ?? [];
    list.push(r);
    byTarget.set(r.target, list);
  }
  const groups: CheckGroup[] = [];
  if (whole.length) groups.push({ title: "Whole backup", checks: whole });
  for (const [title, checks] of byTarget) groups.push({ title, checks });
  return groups;
}

/** A sentence leading with why a run did not fully succeed; empty if it did. */
export function failureSentence(r: RunSummary): string {
  if (r.light === "amber") {
    const base = "The files were backed up, but Ballast could not check they can be restored";
    return r.error ? `${base}: ${r.error}` : `${base}.`;
  }
  if (r.light !== "red") return "";
  if (r.state === "CANCELLED") return "This backup was cancelled before it finished.";
  if (r.error) return `This backup failed: ${r.error}`;
  const bad = r.targets.find((t) => t.light === "red");
  if (bad) return `This backup failed. The copy to ${bad.destination} did not work${bad.detail ? `: ${bad.detail}` : "."}`;
  if (r.verification.failed > 0) {
    const n = r.verification.failed;
    return `This backup failed because ${n} check${n === 1 ? " did" : "s did"} not pass.`;
  }
  return "This backup failed. No reason was recorded; the log below may help.";
}

/** Adds an older page of runs to the list, skipping any already shown. */
export function mergeRuns(existing: RunSummary[], more: RunSummary[]): RunSummary[] {
  const seen = new Set(existing.map((r) => r.id));
  return [...existing, ...more.filter((r) => !seen.has(r.id))];
}
