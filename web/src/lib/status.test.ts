import { describe, expect, it } from "vitest";

import type { ServiceStatus, TierStatus } from "@/api/types";
import { ageIsLate, cancelQuestion, describeCheck, describeTier, expectedIntervalSeconds, isCancelling, shortStatus, shortStatusParts, summarize, worstLight } from "./status";

function svc(...lights: TierStatus["light"][]): ServiceStatus {
  return {
    service: "s",
    catalog_entry: "folder",
    destination: "local",
    tiers: lights.map((light, i) => ({ tier: `t${i}`, light })),
  };
}

describe("describeTier", () => {
  it("never words amber as safe", () => {
    const text = describeTier({ tier: "restic", light: "amber" });
    expect(text).toContain("Don't rely on");
    expect(text).not.toContain("can be restored");
  });

  it("says green can be restored", () => {
    expect(describeTier({ tier: "restic", light: "green", last_finished_at: new Date().toISOString() })).toContain(
      "can be restored",
    );
  });

  it("says what a running backup is waiting for", () => {
    expect(describeTier({ tier: "restic", light: "running" })).toBe("Backing up now…");
    expect(describeTier({ tier: "restic", light: "running", waiting: "waiting for repository lock on b2 (held by immich)" })).toBe(
      "Backing up now, waiting for repository lock on b2 (held by immich)…",
    );
  });

  it("says which step a running backup is on", () => {
    expect(describeTier({ tier: "restic", light: "running", phase: "checking the backup can be restored" })).toBe(
      "Backing up now: checking the backup can be restored…",
    );
  });

  it("covers failure and no backup yet", () => {
    expect(describeTier({ tier: "restic", light: "red" })).toBe("The last backup failed.");
    expect(describeTier({ tier: "restic", light: "none" })).toBe("Hasn't been backed up yet.");
  });
});

describe("worstLight", () => {
  it("lets a failing second copy show through a green first one", () => {
    expect(worstLight(svc("green", "red"))).toBe("red");
    expect(worstLight(svc("green", "amber"))).toBe("amber");
    expect(worstLight(svc("green", "green"))).toBe("green");
  });

  it("treats a service with no tiers as not backed up", () => {
    expect(worstLight(svc())).toBe("none");
  });
});

describe("summarize", () => {
  it("handles nothing configured", () => {
    expect(summarize([])).toEqual({ light: "none", text: "Nothing is being backed up yet." });
  });

  it("reports all safe", () => {
    expect(summarize([svc("green"), svc("green")])).toEqual({ light: "green", text: "All 2 backups are safe." });
    expect(summarize([svc("green")]).text).toBe("Your backup is safe.");
  });

  it("counts every kind of problem and takes the worst light", () => {
    const got = summarize([svc("green"), svc("red"), svc("amber"), svc("none")]);
    expect(got.light).toBe("red");
    expect(got.text).toBe("3 of 4 need attention: 1 failed, 1 not checked, 1 not backed up yet.");
  });

  it("uses amber when nothing failed but something is unchecked", () => {
    expect(summarize([svc("amber")])).toEqual({ light: "amber", text: "1 of 1 needs attention: 1 not checked." });
  });

  it("reports running backups when nothing is wrong", () => {
    expect(summarize([svc("running"), svc("green")])).toEqual({ light: "running", text: "Backing up 1 of 2 now." });
  });
});

describe("describeCheck", () => {
  it("describes known checks and falls back to the raw name", () => {
    expect(describeCheck("restic-read-back")).toBe("Read files back out of the stored backup");
    expect(describeCheck("exec-script")).toBe("exec-script");
  });
});

describe("shortStatus", () => {
  it.each<[string, ServiceStatus, string]>([
    ["never backed up", svc("none"), "No backup yet"],
    ["running without progress", svc("running"), "Backing up now"],
    [
      "running with a step",
      { ...svc("running"), tiers: [{ tier: "restic", light: "running", phase: "making the backup copy" }] },
      "Backing up now, making the backup copy",
    ],
    [
      "running with an upload",
      {
        ...svc("running"),
        targets: [
          { destination: "b2", light: "running", upload: { destination: "b2", percent: 43.9, bytes_done: 1, total_bytes: 2 } },
        ],
      },
      "Uploading, 43%",
    ],
    ["failed without a time", svc("red"), "Failed"],
  ])("%s", (_name, s, want) => {
    expect(shortStatus(s)).toBe(want);
  });

  it("adds when the last backup finished", () => {
    const s = svc("green");
    s.tiers[0].last_finished_at = new Date(Date.now() - 2 * 3600e3).toISOString();
    expect(shortStatus(s)).toMatch(/^Safe, /);
  });
});

describe("cancelling a backup", () => {
  const running = (cancelling: boolean): ServiceStatus => ({
    service: "s",
    catalog_entry: "folder",
    destination: "local",
    tiers: [{ tier: "restic", light: "running", phase: cancelling ? "cancelling" : "uploading to the destination", cancelling }],
  });

  it("says a backup asked to stop is cancelling", () => {
    expect(isCancelling(running(true))).toBe(true);
    expect(isCancelling(running(false))).toBe(false);
    expect(shortStatus(running(true))).toBe("Cancelling…");
    expect(describeTier(running(true).tiers[0])).toContain("Stopping the backup");
    expect(describeTier(running(true).tiers[0])).toContain("last good backup is kept");
  });

  it("words a stopped backup as stopped, keeping the red light", () => {
    const t: TierStatus = { tier: "restic", light: "red", last_state: "CANCELLED" };
    expect(describeTier(t)).toBe("The last backup was stopped before it finished. The backup before it is kept.");
    expect(describeTier({ tier: "restic", light: "red", last_state: "FAILED" })).toBe("The last backup failed.");
  });

  it("asks in plain English and offers a safe way out", () => {
    expect(cancelQuestion.message).toBe("Stop this backup? The last good backup is kept.");
    expect(cancelQuestion.cancelLabel).not.toBe(cancelQuestion.confirmLabel);
  });
});

const HOUR = 3600;
const NOW = Date.parse("2026-11-05T12:00:00Z");

function aged(cron: string | undefined, ageSeconds: number, light: TierStatus["light"] = "green"): ServiceStatus {
  return {
    ...svc(light),
    schedule: cron === undefined ? undefined : { cron, enabled: true },
    tiers: [{ tier: "restic", light, last_finished_at: new Date(NOW - ageSeconds * 1000).toISOString() }],
  };
}

describe("expectedIntervalSeconds", () => {
  it.each<[string, string | undefined, number | undefined]>([
    ["daily", "0 2 * * *", 24 * HOUR],
    ["twice a day", "0 2,14 * * *", 12 * HOUR],
    ["every 6 hours", "0 */6 * * *", 6 * HOUR],
    ["hourly", "0 * * * *", HOUR],
    ["weekly", "0 3 * * 0", 7 * 24 * HOUR],
    ["monthly", "0 3 1 * *", 30 * 24 * HOUR],
    ["unscheduled", undefined, undefined],
    ["unreadable", "soon", undefined],
  ])("%s", (_n, cron, want) => {
    expect(expectedIntervalSeconds(cron)).toBe(want);
  });
});

describe("ageIsLate", () => {
  it.each<[string, string | undefined, number, boolean]>([
    ["daily at 13 hours", "0 2 * * *", 13 * HOUR, false],
    ["daily at 25 hours", "0 2 * * *", 25 * HOUR, true],
    ["weekly at 2 days", "0 3 * * 0", 48 * HOUR, false],
    ["weekly at 4 days", "0 3 * * 0", 96 * HOUR, true],
    ["weekly at 6 days", "0 3 * * 0", 144 * HOUR, true],
    ["unscheduled at 30 days", undefined, 30 * 24 * HOUR, false],
  ])("%s", (_n, cron, age, want) => {
    expect(ageIsLate(aged(cron, age), NOW)).toBe(want);
  });

  it("only cues a green light", () => {
    expect(ageIsLate(aged("0 3 * * 0", 144 * HOUR, "red"), NOW)).toBe(false);
  });
});

describe("shortStatusParts", () => {
  it("keeps the light and the sentence, and marks the age", () => {
    const s = aged("0 3 * * 0", 144 * HOUR);
    const parts = shortStatusParts(s, NOW);
    expect(parts.lead).toBe("Safe");
    expect(parts.late).toBe(true);
    expect(parts.age).toBe("6 days ago");
    expect(worstLight(s)).toBe("green");
  });
});
