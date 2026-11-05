import { describe, expect, it } from "vitest";

import { describeCron, describeSchedule, enabledToSend, formatNextRun, nextRunSentence, presetFor, savedMessage } from "./schedule";

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

describe("formatNextRun", () => {
  // Monday 2 Nov 2026, 12:00 local time.
  const now = new Date(2026, 10, 2, 12, 0);
  it.each([
    [new Date(2026, 10, 2, 18, 30), "today 18:30"],
    [new Date(2026, 10, 3, 3, 0), "tomorrow 03:00"],
    [new Date(2026, 10, 4, 3, 0), "Wed 03:00"],
    [new Date(2026, 10, 8, 3, 0), "Sun 03:00"],
    [new Date(2026, 10, 16, 3, 0), "Mon 16 Nov 03:00"],
  ])("formats %s", (when, want) => {
    expect(formatNextRun(when.toISOString(), now)).toBe(want);
  });
  it("returns empty text when there is no next run", () => {
    expect(formatNextRun(undefined, now)).toBe("");
  });
});

describe("savedMessage", () => {
  const now = new Date(2026, 10, 2, 12, 0);
  const next = new Date(2026, 10, 3, 3, 0).toISOString();
  it.each([
    [{ enabled: true, next_run: next }, "Saved. Next run: tomorrow 03:00."],
    [{ enabled: true, next_run: next, due_now: true }, "Saved. It will run at the next check, then tomorrow 03:00."],
    [{ enabled: false }, "Saved. Paused, so it will not run."],
  ])("%j", (res, want) => {
    expect(savedMessage(res, now)).toBe(want);
  });
});

describe("enabledToSend", () => {
  it.each([
    [true, false, true],
    [true, true, undefined],
    [false, false, undefined],
    [false, true, undefined],
  ])("paused=%s bulk=%s", (paused, bulk, want) => {
    expect(enabledToSend(paused, bulk)).toBe(want);
  });
});

describe("nextRunSentence", () => {
  const now = new Date(2026, 10, 2, 12, 0);
  const next = new Date(2026, 10, 3, 3, 0).toISOString();
  it.each([
    [{ enabled: true, next_run: next }, "Next run: tomorrow 03:00"],
    [{ enabled: true, next_run: next, due_now: true }, "It will run at the next check, then tomorrow 03:00"],
    [{ enabled: false, next_run: next }, ""],
    [{ enabled: true }, ""],
  ])("%j", (res, want) => {
    expect(nextRunSentence(res, now)).toBe(want);
  });
});
