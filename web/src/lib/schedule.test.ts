import { describe, expect, it } from "vitest";

import { describeCron, describeSchedule, presetFor } from "./schedule";

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
    expect(describeSchedule({ cron: "30 2 * * 1-5", enabled: true })).toBe("Weekdays at 2:30");
    expect(describeSchedule({ cron: "0 */6 * * 0", enabled: true })).toBe("Custom (0 */6 * * 0)");
    expect(describeSchedule({ cron: "0 3 * * *", enabled: false })).toBe("Every night at 3:00 (paused)");
  });
});

describe("describeCron", () => {
  const cases: [string, string | undefined][] = [
    ["15 4 * * *", "Every day at 4:15"],
    ["15 4 * * 1-5", "Weekdays at 4:15"],
    ["0 2 * * 0", "Every Sunday at 2:00"],
    ["0 2 * * 7", "Every Sunday at 2:00"],
    ["0 2 * * 3", "Every Wednesday at 2:00"],
    ["0 */4 * * *", "Every 4 hours"],
    ["0 * * * *", "Every hour"],
    ["0 */1 * * *", undefined],
    ["0 */6 * * 0", undefined],
    ["30 3 1 * *", undefined],
    ["61 3 * * *", undefined],
    ["0 25 * * *", undefined],
    ["not a cron", undefined],
    ["", undefined],
  ];
  it.each(cases)("%s", (cron, want) => {
    expect(describeCron(cron)).toBe(want);
  });
});
