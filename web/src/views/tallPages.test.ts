// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createApp, nextTick } from "vue";
import { createMemoryHistory, createRouter } from "vue-router";

import ScheduleView from "./ScheduleView.vue";
import SettingsView from "./SettingsView.vue";

const N = 40;
const names = Array.from({ length: N }, (_, i) => `svc-${i}`);
const status = names.map((n, i) => ({
  service: n,
  catalog_entry: "folder",
  destination: "nas",
  tiers: [{ tier: "restic", light: i === 0 ? "red" : "green" }],
  schedule: n === "svc-5" ? { cron: "0 3 * * *", enabled: false } : { cron: "0 3 * * *", enabled: true },
}));
const services = names.map((n) => ({ name: n, catalog_entry: "folder", destination: "nas" }));

const calls: { url: string; method: string; body?: string }[] = [];
function fetchStub(url: string, init?: RequestInit) {
  const method = init?.method ?? "GET";
  calls.push({ url, method, body: init?.body as string | undefined });
  let body: unknown = [];
  if (url.endsWith("/status")) body = status;
  else if (url.endsWith("/services")) body = services;
  else if (url.includes("/notification-settings") || url.includes("/notifications")) body = { enabled: true, heartbeat_enabled: false };
  else if (url.includes("/schedule")) body = { cron: "0 3 * * *", enabled: true, next_runs: [] };
  return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
}

async function flush() {
  for (let i = 0; i < 8; i++) await new Promise((r) => setTimeout(r, 0));
  await nextTick();
}

async function mount(component: object) {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: "/:p(.*)*", component: { template: "<div/>" } }] });
  const el = document.createElement("div");
  document.body.appendChild(el);
  const app = createApp(component).use(router);
  app.mount(el);
  await flush();
  return { el, unmount: () => (app.unmount(), el.remove()) };
}

function type(el: HTMLElement, text: string) {
  const search = el.querySelector<HTMLInputElement>('input[type="search"]')!;
  search.value = text;
  search.dispatchEvent(new Event("input"));
}

beforeEach(() => {
  calls.length = 0;
  vi.stubGlobal("fetch", vi.fn(fetchStub));
});
afterEach(() => vi.unstubAllGlobals());

describe.each([
  ["Schedules", ScheduleView, "li form"],
  ["Notifications", SettingsView, "li form"],
])("%s page with many backups", (_n, view, row) => {
  it("shows a search box, puts the failing backup first and folds the safe group", async () => {
    const { el, unmount } = await mount(view);
    const rows = [...el.querySelectorAll(".group .list " + row)].filter((f) => f.textContent?.includes("svc-"));
    expect(el.querySelector('[role="search"]')).not.toBeNull();
    expect(el.querySelector(".group-head")?.textContent).toContain("Failed");
    expect(rows).toHaveLength(1);
    const show = [...el.querySelectorAll<HTMLButtonElement>(".group-folded button")].find((b) => b.textContent?.includes("Show all 39 safe backups"))!;
    expect(show).toBeDefined();
    show.click();
    await nextTick();
    expect(el.querySelectorAll(".group .list " + row).length).toBe(N);
    expect(el.querySelector(".group-fold")?.textContent).toContain("Hide safe backups");
    unmount();
  });

  it("searching shows matches even inside the safe group, with a count", async () => {
    const { el, unmount } = await mount(view);
    type(el, "svc-3");
    await flush();
    const rows = el.querySelectorAll(".group .list " + row);
    expect(rows.length).toBe(11); // svc-3 and svc-30..39
    expect(el.querySelector(".tools-count")?.textContent).toContain("11 of 40 shown");
    expect(el.querySelector(".group-folded")).toBeNull();
    unmount();
  });
});

describe("Schedules bulk change", () => {
  it("applies only to selected rows and leaves a paused schedule paused", async () => {
    const { el, unmount } = await mount(ScheduleView);
    type(el, "svc-5");
    await flush();
    const boxes = [...el.querySelectorAll<HTMLInputElement>('.row-check input[type="checkbox"]')];
    expect(boxes.length).toBeGreaterThan(0);
    const only = el.querySelector<HTMLInputElement>('input[aria-label="Select svc-5"]')!;
    only.checked = true;
    only.dispatchEvent(new Event("change"));
    await nextTick();
    const apply = [...el.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent?.includes("Apply to 1"))!;
    calls.length = 0;
    apply.click();
    await flush();
    const puts = calls.filter((c) => c.method !== "GET");
    expect(puts).toHaveLength(1);
    expect(puts[0].url).toContain("svc-5");
    expect(puts[0].body ?? "").not.toContain('"enabled":true');
    expect(el.textContent).toContain("1 stayed paused");
    unmount();
  });

  it("'Select all shown' skips rows hidden in the folded group", async () => {
    const { el, unmount } = await mount(ScheduleView);
    [...el.querySelectorAll<HTMLButtonElement>("button")].find((b) => b.textContent?.includes("Select all shown"))!.click();
    await nextTick();
    expect(el.querySelector(".bulk-bar")?.textContent).toContain("1 selected");
    unmount();
  });
});
