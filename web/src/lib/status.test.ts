import { describe, expect, it } from "vitest";

import type { ServiceStatus, TierStatus } from "@/api/types";
import { describeCheck, describeTier, summarize, worstLight } from "./status";

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
