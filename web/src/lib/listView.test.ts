import { describe, expect, it } from "vitest";

import {
  collapsesGroup,
  countLights,
  emptyQuery,
  filterEntries,
  groupEntries,
  lightShares,
  matchesText,
  needsTools,
  type ListEntry,
  type ListQuery,
} from "./listView";

const items: ListEntry[] = [
  { name: "photos", type: "immich", destination: "nas", light: "green" },
  { name: "vaultwarden", type: "vaultwarden", destination: "nas", light: "amber" },
  { name: "nextcloud-db", type: "postgres", destination: "b2", light: "red" },
  { name: "web-101", type: "proxmox-vm", destination: "b2", light: "running" },
  { name: "new-one", type: "folder", destination: "nas", light: "none" },
];
const id = (e: ListEntry) => e;
const q = (over: Partial<ListQuery>): ListQuery => ({ ...emptyQuery(), ...over });

describe("needsTools", () => {
  it.each([
    [0, false],
    [1, false],
    [6, false],
    [7, true],
    [150, true],
  ])("%i entries -> %s", (n, want) => {
    expect(needsTools(n)).toBe(want);
  });
});

describe("matchesText", () => {
  it.each([
    ["", true],
    ["photo", true],
    ["PHOTOS", true],
    ["immich nas", true],
    ["immich b2", false],
    ["xyz", false],
  ])("%j", (text, want) => {
    expect(matchesText(items[0], text)).toBe(want);
  });
});

describe("filterEntries", () => {
  it.each<[string, Partial<ListQuery>, string[]]>([
    ["everything", {}, ["photos", "vaultwarden", "nextcloud-db", "web-101", "new-one"]],
    ["attention is red, amber and never backed up", { light: "attention" }, ["vaultwarden", "nextcloud-db", "new-one"]],
    ["one light", { light: "red" }, ["nextcloud-db"]],
    ["text and light together", { light: "attention", text: "nas" }, ["vaultwarden", "new-one"]],
    ["by destination", { text: "b2" }, ["nextcloud-db", "web-101"]],
  ])("%s", (_name, over, want) => {
    expect(filterEntries(items, id, q(over)).map((e) => e.name)).toEqual(want);
  });
});

describe("countLights", () => {
  it("counts each light and the ones needing attention", () => {
    const c = countLights(items, id);
    expect(c).toEqual({ all: 5, attention: 3, green: 1, amber: 1, red: 1, running: 1, none: 1 });
  });
});

describe("groupEntries", () => {
  it("keeps one unlabelled group when not grouping", () => {
    const g = groupEntries(items, id, "none");
    expect(g).toHaveLength(1);
    expect(g[0].items).toHaveLength(5);
  });

  it("puts problems first when grouping by status", () => {
    expect(groupEntries(items, id, "status").map((g) => g.label)).toEqual([
      "Failed",
      "Not checked",
      "No backup yet",
      "Backing up now",
      "Safe",
    ]);
  });

  it("sorts destinations alphabetically", () => {
    const g = groupEntries(items, id, "destination");
    expect(g.map((x) => [x.label, x.items.length])).toEqual([
      ["b2", 2],
      ["nas", 3],
    ]);
  });

  it("uses friendly type names", () => {
    const g = groupEntries(items.slice(0, 2), id, "type", (t) => t.toUpperCase());
    expect(g.map((x) => x.label)).toEqual(["IMMICH", "VAULTWARDEN"]);
  });
});

describe("lightShares", () => {
  it("lists problems first and leaves out empty lights", () => {
    const s = lightShares(items.slice(0, 2), id);
    expect(s.map((x) => x.light)).toEqual(["amber", "green"]);
    expect(s[0].percent).toBe(50);
  });

  it("is empty for an empty list", () => {
    expect(lightShares([], id)).toEqual([]);
  });
});

describe("collapsesGroup", () => {
  const safe = (n: number) => ({ key: "green", items: new Array(n).fill(0) });
  const byStatus = { ...emptyQuery(), group: "status" as const };
  const cases = [
    { name: "folds a long safe group", g: safe(20), q: byStatus, open: false, want: true },
    { name: "keeps a short safe group open", g: safe(12), q: byStatus, open: false, want: false },
    { name: "stays open once asked for", g: safe(20), q: byStatus, open: true, want: false },
    { name: "never folds a problem group", g: { key: "red", items: new Array(30).fill(0) }, q: byStatus, open: false, want: false },
    { name: "does not fold ungrouped lists", g: safe(20), q: emptyQuery(), open: false, want: false },
    { name: "shows safe backups when searching", g: safe(20), q: { ...byStatus, text: "app" }, open: false, want: false },
    { name: "shows safe backups when the Safe filter is on", g: safe(20), q: { ...byStatus, light: "green" as const }, open: false, want: false },
  ];
  for (const c of cases) {
    it(c.name, () => {
      expect(collapsesGroup(c.g, c.q, c.open)).toBe(c.want);
    });
  }
});
