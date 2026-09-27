import { describe, expect, it } from "vitest";

import { describeSchedule, presetFor } from "./schedule";

describe("presetFor", () => {
  it("matches presets, ignoring extra whitespace", () => {
    expect(presetFor("0 3 * * *")?.id).toBe("nightly");
    expect(presetFor("  0  3 * * 0 ")?.id).toBe("weekly");
  });

  it("returns undefined for a custom expression", () => {
    expect(presetFor("30 2 * * 1-5")).toBeUndefined();
    expect(presetFor(undefined)).toBeUndefined();
  });
});

describe("describeSchedule", () => {
  it("describes no schedule, presets, custom and paused", () => {
    expect(describeSchedule(undefined)).toBe("Only when you start it");
    expect(describeSchedule({ cron: "0 3 * * *", enabled: true })).toBe("Every night at 3:00");
    expect(describeSchedule({ cron: "30 2 * * 1-5", enabled: true })).toBe("Custom (30 2 * * 1-5)");
    expect(describeSchedule({ cron: "0 3 * * *", enabled: false })).toBe("Every night at 3:00 (paused)");
  });
});
