import { describe, expect, it } from "vitest";

import type { ServiceStatus } from "@/api/types";
import { anyServiceVerified, checklistComplete, firstRunChecklist } from "./checklist";

describe("firstRunChecklist", () => {
  const cases = [
    { name: "fresh install", in: { destinations: 0, services: 0, channels: 0, anyVerified: false }, done: [false, false, false, false] },
    { name: "destination only", in: { destinations: 1, services: 0, channels: 0, anyVerified: false }, done: [true, false, false, false] },
    { name: "no notifications yet", in: { destinations: 2, services: 3, channels: 0, anyVerified: true }, done: [true, true, false, true] },
    { name: "all done", in: { destinations: 1, services: 1, channels: 1, anyVerified: true }, done: [true, true, true, true] },
  ];
  for (const c of cases) {
    it(c.name, () => {
      const items = firstRunChecklist(c.in);
      expect(items.map((i) => i.done)).toEqual(c.done);
      expect(checklistComplete(items)).toBe(c.done.every(Boolean));
    });
  }

  it("keeps the order, labels and deep links", () => {
    const items = firstRunChecklist({ destinations: 0, services: 0, channels: 0, anyVerified: false });
    expect(items.map((i) => [i.label, i.to])).toEqual([
      ["Add a destination", "/destinations"],
      ["Add a service", "/wizard"],
      ["Set up notifications", "/settings"],
      ["Run your first verified backup", "/services"],
    ]);
  });
});

describe("anyServiceVerified", () => {
  const svc = (...verified: (string | undefined)[]): ServiceStatus => ({
    service: "s",
    catalog_entry: "folder",
    destination: "local",
    tiers: verified.map((v, i) => ({ tier: `t${i}`, light: "green", last_verified_at: v })),
  });
  const cases = [
    { name: "no services", in: [], want: false },
    { name: "never verified", in: [svc(undefined)], want: false },
    { name: "one verified tier", in: [svc(undefined), svc(undefined, "2026-01-01T00:00:00Z")], want: true },
  ];
  for (const c of cases) {
    it(c.name, () => expect(anyServiceVerified(c.in)).toBe(c.want));
  }
});
