import { describe, expect, it } from "vitest";

import type { RunSummary, VerificationRecord } from "@/api/types";
import {
  plainReason,
  checkOutcome,
  failureSentence,
  formatRunDuration,
  groupChecks,
  mergeRuns,
  runStatusText,
  verificationLine,
} from "./runHistory";

function run(over: Partial<RunSummary> = {}): RunSummary {
  return {
    id: 1,
    service: "immich",
    state: "SUCCESS",
    light: "green",
    started_at: "2026-01-01T00:00:00Z",
    verification: { checks: 0, passed: 0, failed: 0, warnings: 0 },
    targets: [],
    ...over,
  };
}

describe("runStatusText", () => {
  it.each([
    [{ light: "green" as const }, "Backed up and checked"],
    [{ light: "amber" as const, state: "SUCCESS_UNVERIFIED" }, "Backed up, but not checked"],
    [{ light: "red" as const, state: "FAILED" }, "Failed"],
    [{ light: "red" as const, state: "CANCELLED" }, "Cancelled"],
    [{ light: "running" as const, state: "UPLOADING" }, "Still running"],
    [{ light: "red" as const, state: "FAILED", error: "cancelled: stopped on request" }, "Cancelled"],
    [{ light: "red" as const, state: "cancelled" }, "Cancelled"],
  ])("%j reads %s", (over, want) => {
    expect(runStatusText(run(over))).toBe(want);
  });
});

describe("formatRunDuration", () => {
  it.each([
    [undefined, "—"],
    [0, "under a second"],
    [1, "1 second"],
    [45, "45 seconds"],
    [60, "1 min"],
    [185, "3 min 5 s"],
    [3600, "1 h"],
    [3725, "1 h 2 min"],
  ])("%s -> %s", (secs, want) => {
    expect(formatRunDuration(secs)).toBe(want);
  });
});

describe("verificationLine", () => {
  it.each([
    [{ checks: 0, passed: 0, failed: 0, warnings: 0 }, "No checks recorded"],
    [{ checks: 1, passed: 1, failed: 0, warnings: 0 }, "1 check passed"],
    [{ checks: 3, passed: 3, failed: 0, warnings: 0 }, "All 3 checks passed"],
    [{ checks: 3, passed: 3, failed: 0, warnings: 1 }, "All 3 checks passed, 1 with a warning"],
    [{ checks: 3, passed: 2, failed: 1, warnings: 0 }, "2 of 3 checks passed, 1 failed"],
    [{ checks: 4, passed: 1, failed: 3, warnings: 2 }, "1 of 4 checks passed, 3 failed, 2 with a warning"],
  ])("%j -> %s", (v, want) => {
    expect(verificationLine(v)).toBe(want);
  });
});

describe("checkOutcome", () => {
  const rec = (over: Partial<VerificationRecord>): VerificationRecord => ({
    method: "restic-read-back",
    passed: true,
    duration_ms: 1,
    recorded_at: "",
    ...over,
  });
  it.each([
    [{}, "pass", "Passed"],
    [{ warning: "drift" }, "warning", "Passed with a warning"],
    [{ passed: false }, "fail", "Failed"],
    [{ passed: false, warning: "x" }, "fail", "Failed"],
  ])("%j", (over, kind, text) => {
    expect(checkOutcome(rec(over))).toEqual({ kind, text });
  });
});

describe("groupChecks", () => {
  it("groups by destination and keeps whole-run checks first", () => {
    const mk = (target: string | undefined, method: string): VerificationRecord => ({
      target,
      method,
      passed: true,
      duration_ms: 1,
      recorded_at: "",
    });
    const groups = groupChecks([mk("b2", "a"), mk(undefined, "c"), mk("nas", "d"), mk("b2", "e")]);
    expect(groups.map((g) => [g.title, g.checks.map((c) => c.method)])).toEqual([
      ["Whole backup", ["c"]],
      ["b2", ["a", "e"]],
      ["nas", ["d"]],
    ]);
  });
  it("is empty for no checks", () => {
    expect(groupChecks([])).toEqual([]);
  });
});

describe("failureSentence", () => {
  it.each([
    [run(), ""],
    [run({ light: "red", state: "FAILED", error: "disk full" }), "This backup failed: disk full."],
    [run({ light: "red", state: "CANCELLED" }), "This backup was cancelled before it finished."],
    [
      run({ light: "red", state: "FAILED", targets: [{ destination: "b2", state: "FAILED", light: "red", detail: "no route" }] }),
      "This backup failed. The copy to b2 did not work: no route",
    ],
    [
      run({ light: "red", state: "FAILED", verification: { checks: 2, passed: 1, failed: 1, warnings: 0 } }),
      "This backup failed because 1 check did not pass.",
    ],
    [run({ light: "red", state: "FAILED" }), "This backup failed. No reason was recorded; the log below may help."],
    [
      run({ light: "amber", state: "SUCCESS_UNVERIFIED", error: "docker unreachable" }),
      "The files were backed up, but Ballast could not check they can be restored: docker unreachable.",
    ],
    [
      run({ light: "amber", state: "SUCCESS_UNVERIFIED" }),
      "The files were backed up, but Ballast could not check they can be restored.",
    ],
    [run({ light: "running", state: "UPLOADING" }), ""],
  ])("%#", (r, want) => {
    expect(failureSentence(r)).toBe(want);
  });
});

describe("mergeRuns", () => {
  it("appends older runs and drops repeats", () => {
    const merged = mergeRuns([run({ id: 5 }), run({ id: 4 })], [run({ id: 4 }), run({ id: 3 })]);
    expect(merged.map((r) => r.id)).toEqual([5, 4, 3]);
  });
});

describe("plainReason", () => {
  it.each([
    ["disk full", "disk full"],
    ["service svc14: service folder: backup path /srv/data/svc14 does not exist", "backup path …/svc14 does not exist"],
    ["service immich: dumping database: pg_dump exited with code 1. See log.", "pg_dump exited with code 1"],
    ["service a: destination b2: uploading: connection refused", "connection refused"],
    ["  ", ""],
  ])("%s", (raw, want) => {
    expect(plainReason(raw)).toBe(want);
  });

  it("keeps a long reason to one short sentence", () => {
    expect(plainReason("x".repeat(400)).length).toBeLessThanOrEqual(160);
  });

  it("leads the failure sentence with the plain reason, not the wrapped text", () => {
    const r = run({ light: "red", state: "FAILED", error: "service svc14: service folder: backup path /srv/data/svc14 does not exist" });
    expect(failureSentence(r)).toBe("This backup failed: backup path …/svc14 does not exist.");
  });
});
