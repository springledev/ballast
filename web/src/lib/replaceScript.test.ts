import { describe, expect, it } from "vitest";

import { emptyProgress, parseProgress, readyToRetire, scriptSteps } from "./replaceScript";

describe("scriptSteps", () => {
  const all = { parallel: true, sevenDays: true, restored: true };
  const cases = [
    { name: "nothing done", p: emptyProgress(), light: "green" as const, ready: false },
    { name: "missing test restore", p: { ...all, restored: false }, light: "green" as const, ready: false },
    { name: "all done but amber", p: all, light: "amber" as const, ready: false },
    { name: "all done but red", p: all, light: "red" as const, ready: false },
    { name: "all done and green", p: all, light: "green" as const, ready: true },
  ];
  for (const c of cases) {
    it(c.name, () => expect(readyToRetire(c.p, c.light)).toBe(c.ready));
  }

  it("locks later steps until the parallel run is ticked", () => {
    const steps = scriptSteps(emptyProgress(), "green");
    expect(steps.map((s) => [s.key, s.enabled])).toEqual([
      ["parallel", true],
      ["sevenDays", false],
      ["restored", false],
      ["retire", false],
    ]);
  });
});

describe("parseProgress", () => {
  it("survives bad input", () => {
    expect(parseProgress(null)).toEqual({});
    expect(parseProgress("not json")).toEqual({});
    expect(parseProgress('{"a":{"parallel":true,"sevenDays":"yes"}}')).toEqual({
      a: { parallel: true, sevenDays: false, restored: false },
    });
  });
});
