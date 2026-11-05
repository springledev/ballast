// Search, filter and grouping for long lists of backups. Someone with
// one server never sees these controls; someone with a hundred Proxmox
// guests gets them as soon as the list is long enough to need them.
// Everything here is pure so it can be tested without a browser.
import type { Light } from "./status";

/** Lists with more entries than this get search, filters and grouping. */
export const TOOLS_THRESHOLD = 6;

/** Whether a list is long enough to show the search and filter bar. */
export function needsTools(count: number): boolean {
  return count > TOOLS_THRESHOLD;
}

/** Status filter: everything, one light, or "attention" (red, amber or never backed up). */
export type LightFilter = "all" | "attention" | Light;

/** How a list is grouped. */
export type GroupBy = "none" | "type" | "destination" | "status";

/** The fields of a list entry the tools look at. */
export interface ListEntry {
  name: string;
  type: string;
  destination: string;
  light?: Light;
}

/** The current state of the search and filter bar. */
export interface ListQuery {
  text: string;
  light: LightFilter;
  group: GroupBy;
}

/** A fresh query that shows everything, ungrouped. */
export function emptyQuery(): ListQuery {
  return { text: "", light: "all", group: "none" };
}

const attention: Light[] = ["red", "amber", "none"];

/** Whether a light counts as "needs attention". */
export function needsAttention(light: Light | undefined): boolean {
  return light !== undefined && attention.includes(light);
}

function matchesLight(light: Light | undefined, filter: LightFilter): boolean {
  if (filter === "all") return true;
  if (filter === "attention") return needsAttention(light);
  return light === filter;
}

/**
 * Whether an entry matches the search text. Every word must appear in
 * the name, type or destination, so "db nas" finds databases stored on
 * the NAS.
 */
export function matchesText(e: ListEntry, text: string): boolean {
  const words = text.toLowerCase().split(/\s+/).filter(Boolean);
  if (!words.length) return true;
  const hay = `${e.name} ${e.type} ${e.destination}`.toLowerCase();
  return words.every((w) => hay.includes(w));
}

/** The entries that match the query, in their original order. */
export function filterEntries<T>(items: T[], entry: (t: T) => ListEntry, q: ListQuery): T[] {
  return items.filter((t) => {
    const e = entry(t);
    return matchesLight(e.light, q.light) && matchesText(e, q.text);
  });
}

/** How many entries have each light, plus how many need attention. */
export function countLights<T>(items: T[], entry: (t: T) => ListEntry): Record<LightFilter, number> {
  const c: Record<LightFilter, number> = { all: items.length, attention: 0, green: 0, amber: 0, red: 0, running: 0, none: 0 };
  for (const t of items) {
    const l = entry(t).light;
    if (!l) continue;
    c[l]++;
    if (needsAttention(l)) c.attention++;
  }
  return c;
}

/** A labelled group of entries. */
export interface Group<T> {
  key: string;
  label: string;
  items: T[];
}

const statusOrder: Light[] = ["red", "amber", "none", "running", "green"];
const statusGroupLabel: Record<Light, string> = {
  red: "Failed",
  amber: "Not checked",
  none: "No backup yet",
  running: "Backing up now",
  green: "Safe",
};

/**
 * Splits entries into groups. Groups by status put problems first;
 * other groups are alphabetical. With "none" there is one unlabelled
 * group holding everything.
 */
export function groupEntries<T>(
  items: T[],
  entry: (t: T) => ListEntry,
  by: GroupBy,
  typeLabel: (type: string) => string = (t) => t,
): Group<T>[] {
  if (by === "none") return [{ key: "", label: "", items }];
  const map = new Map<string, T[]>();
  for (const t of items) {
    const e = entry(t);
    const key = by === "type" ? e.type : by === "destination" ? e.destination : (e.light ?? "none");
    if (!map.has(key)) map.set(key, []);
    map.get(key)!.push(t);
  }
  const groups = [...map.entries()].map(([key, list]) => ({
    key,
    label: by === "status" ? statusGroupLabel[key as Light] : by === "type" ? typeLabel(key) : key,
    items: list,
  }));
  if (by === "status") {
    groups.sort((a, b) => statusOrder.indexOf(a.key as Light) - statusOrder.indexOf(b.key as Light));
  } else {
    groups.sort((a, b) => a.label.localeCompare(b.label));
  }
  return groups;
}

/** A "Safe" group longer than this starts collapsed so problems stay in view. */
export const COLLAPSE_SAFE_FROM = 12;

/**
 * Whether a group's rows are hidden behind a "Show" button: only the
 * long, healthy group of a status-grouped list, and only while nothing is
 * being searched or filtered (someone who asks for Safe backups gets them).
 */
export function collapsesGroup(group: { key: string; items: unknown[] }, q: ListQuery, open: boolean): boolean {
  return (
    !open &&
    q.group === "status" &&
    group.key === "green" &&
    group.items.length > COLLAPSE_SAFE_FROM &&
    q.light === "all" &&
    q.text.trim() === ""
  );
}

/**
 * The share of each light in a list, for the overview's status strip.
 * Problems come first so they are never squeezed out of sight; empty
 * lights are left out.
 */
export function lightShares<T>(items: T[], entry: (t: T) => ListEntry): { light: Light; count: number; percent: number }[] {
  const c = countLights(items, entry);
  const total = items.length || 1;
  return statusOrder
    .filter((l) => c[l] > 0)
    .map((l) => ({ light: l, count: c[l], percent: (c[l] / total) * 100 }));
}

/**
 * How a long page opens: grouped by status once the list is long enough
 * to fold (so problems lead and the healthy group folds), as long as the
 * lights are known. Short lists stay ungrouped.
 */
export function initialGroup(count: number, lightsKnown: boolean): GroupBy {
  return lightsKnown && count > COLLAPSE_SAFE_FROM ? "status" : "none";
}

/** The entries in groups that are not folded away: what a person can actually see. */
export function unfoldedItems<T>(groups: Group<T>[], folded: (g: Group<T>) => boolean): T[] {
  return groups.filter((g) => !folded(g)).flatMap((g) => g.items);
}
