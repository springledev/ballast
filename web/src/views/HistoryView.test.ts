// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { createApp, nextTick } from "vue";
import { createMemoryHistory, createRouter } from "vue-router";

import type { RunDetail, RunSummary } from "@/api/types";
import HistoryView from "./HistoryView.vue";

function run(id: number, over: Partial<RunSummary> = {}): RunSummary {
  return {
    id,
    service: "immich",
    state: "SUCCESS",
    light: "green",
    started_at: new Date(Date.now() - id * 3600_000).toISOString(),
    duration_seconds: 65,
    verification: { checks: 2, passed: 2, failed: 0, warnings: 0 },
    targets: [
      { destination: "nas", state: "SUCCESS", light: "green" },
      { destination: "b2", state: "FAILED", light: "red", detail: "no route to host", secondary: true },
    ],
    ...over,
  };
}

const failed: RunDetail = {
  ...run(7, { light: "red", state: "FAILED", error: "restic exited with code 1" }),
  verification_results: [
    { target: "nas", method: "restic-read-back", passed: true, duration_ms: 5, recorded_at: "" },
    { target: "b2", method: "restic-read-back", passed: false, detail: "could not reach b2", duration_ms: 5, recorded_at: "" },
  ],
  log_tail: [{ at: "2026-01-01T00:00:00Z", text: "UPLOADING -> FAILED: boom" }],
};

type Handler = (url: string) => Response | Promise<Response>;
const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

async function flush() {
  for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0));
  await nextTick();
}

async function mount(handler: Handler) {
  vi.stubGlobal("fetch", vi.fn((url: string) => Promise.resolve(handler(url))));
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: "/history/:service", component: HistoryView }, { path: "/:p(.*)*", component: { template: "<div/>" } }],
  });
  router.push("/history/immich");
  await router.isReady();
  const el = document.createElement("div");
  document.body.appendChild(el);
  const app = createApp({ template: "<router-view/>" }).use(router);
  app.mount(el);
  await flush();
  return { el, unmount: () => (app.unmount(), el.remove()) };
}

describe("HistoryView", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("lists runs with status, destinations and a check summary", async () => {
    const { el, unmount } = await mount(() => json({ runs: [run(2), run(1, { light: "red", state: "FAILED" })] }));
    const rows = el.querySelectorAll(".hist-row");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("Backed up and checked");
    expect(rows[0].textContent).toContain("All 2 checks passed");
    expect(rows[0].textContent).toContain("1 min 5 s");
    expect(rows[0].textContent).toContain("nas");
    expect(rows[0].textContent).toContain("b2");
    expect(rows[1].textContent).toContain("Failed");
    expect(el.textContent).not.toContain("Load more");
    unmount();
  });

  it("loads older runs with the before cursor", async () => {
    const urls: string[] = [];
    const { el, unmount } = await mount((url) => {
      urls.push(url);
      return url.includes("before=2") ? json({ runs: [run(2), run(1)] }) : json({ runs: [run(3), run(2)], next_before: 2 });
    });
    expect(el.querySelectorAll(".hist-row")).toHaveLength(2);
    const more = [...el.querySelectorAll("button")].find((b) => b.textContent?.includes("Load more"))!;
    more.click();
    await flush();
    expect(urls.some((u) => u.includes("/services/immich/runs?limit=20&before=2"))).toBe(true);
    expect(el.querySelectorAll(".hist-row")).toHaveLength(3);
    expect(el.textContent).not.toContain("Load more");
    unmount();
  });

  it("shows an empty state", async () => {
    const { el, unmount } = await mount(() => json({ runs: [] }));
    expect(el.textContent).toContain("hasn't been backed up yet");
    unmount();
  });

  it("shows an error notice when loading fails", async () => {
    const { el, unmount } = await mount(() => json({ error: "service not found" }, 404));
    const notice = el.querySelector('.notice--error[role="alert"]');
    expect(notice?.textContent).toContain("Couldn't load the history for immich");
    expect(notice?.textContent).toContain("service not found");
    unmount();
  });

  it("opens a run: reason first, per-destination results, log tail, then returns focus", async () => {
    const { el, unmount } = await mount((url) => (url.endsWith("/runs/7") ? json(failed) : json({ runs: [run(7, { light: "red", state: "FAILED" })] })));
    const row = el.querySelector<HTMLButtonElement>("#run-row-7")!;
    row.click();
    await flush();
    const detail = el.querySelector(".hist-detail")!;
    expect(detail.querySelector(".notice")?.textContent).toContain("This backup failed: restic exited with code 1");
    expect(detail.textContent).toContain("Failed");
    expect(detail.textContent).toContain("could not reach b2");
    expect(detail.textContent).toContain("no route to host");
    const groups = [...detail.querySelectorAll(".hist-group h5")].map((h) => h.textContent);
    expect(groups).toEqual(["nas", "b2"]);
    expect(detail.querySelector("pre.hist-log")?.textContent).toContain("UPLOADING -> FAILED: boom");
    expect(detail.querySelector("details.advanced")?.hasAttribute("open")).toBe(true);

    const back = [...detail.querySelectorAll("button")].find((b) => b.textContent?.includes("Back to the list"))!;
    back.click();
    await flush();
    expect(el.querySelector(".hist-detail")).toBeNull();
    expect(document.activeElement).toBe(row);
    unmount();
  });
});
