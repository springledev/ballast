// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createApp, nextTick } from "vue";
import { createMemoryHistory, createRouter } from "vue-router";

import type { ServiceStatus } from "@/api/types";
import { answerConfirm, pending } from "@/lib/confirm";
import DashboardView from "./DashboardView.vue";

function status(n: number): ServiceStatus[] {
  return Array.from({ length: n }, (_, i) => ({
    service: `svc-${i}`,
    catalog_entry: i % 2 ? "proxmox-vm" : "folder",
    destination: "nas",
    tiers: [{ tier: "restic", light: i === 0 ? "red" : "green" }],
  }));
}

function routes(list: ServiceStatus[]) {
  return (url: string, _init?: RequestInit) => {
    const body = url.includes("/status")
      ? list
      : url.includes("/ballast-health")
        ? { items: [] }
        : url.includes("/destinations") || url.includes("/notification-channels")
          ? [{ name: "x" }]
          : [];
    return Promise.resolve(new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }));
  };
}

async function flush() {
  for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0));
  await nextTick();
}

async function mount() {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: "/:p(.*)*", component: { template: "<div/>" } }] });
  const el = document.createElement("div");
  document.body.appendChild(el);
  const app = createApp(DashboardView).use(router);
  app.mount(el);
  await flush();
  return { el, unmount: () => (app.unmount(), el.remove()) };
}

describe("overview at different sizes", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it("keeps a small install simple: cards, no search bar", async () => {
    vi.stubGlobal("fetch", vi.fn(routes(status(3))));
    const { el, unmount } = await mount();
    expect(el.querySelector(".list-tools")).toBeNull();
    expect(el.querySelectorAll(".cards > li")).toHaveLength(3);
    expect(el.querySelector(".hero-text")?.textContent).toContain("1 of 3 needs attention");
    unmount();
  });

  it("gives a big install search, compact rows and problems first", async () => {
    vi.stubGlobal("fetch", vi.fn(routes(status(40))));
    const { el, unmount } = await mount();
    expect(el.querySelector(".list-tools")).not.toBeNull();
    // The one failing backup is on show; the 39 safe ones start folded.
    expect(el.querySelectorAll(".rows > li")).toHaveLength(1);
    expect(el.querySelector(".group-head")?.textContent).toContain("Failed");
    const showSafe = [...el.querySelectorAll<HTMLButtonElement>(".group-folded button")].find((b) => b.textContent?.includes("Show all 39 safe backups"))!;
    expect(showSafe).toBeDefined();
    showSafe.click();
    await nextTick();
    expect(el.querySelectorAll(".rows > li")).toHaveLength(40);
    expect(el.querySelector(".group-fold")?.textContent).toContain("Hide safe backups");

    const search = el.querySelector<HTMLInputElement>('input[type="search"]')!;
    search.value = "svc-3";
    search.dispatchEvent(new Event("input"));
    await nextTick();
    // svc-3 and svc-30..39
    expect(el.querySelectorAll(".rows > li")).toHaveLength(11);

    const attention = [...el.querySelectorAll<HTMLButtonElement>(".chip")].find((b) => b.textContent?.includes("Needs attention"))!;
    search.value = "";
    search.dispatchEvent(new Event("input"));
    await nextTick();
    attention.click();
    await nextTick();
    expect([...el.querySelectorAll(".rows .item-title")].map((n) => n.textContent)).toEqual(["svc-0"]);
    unmount();
  });

  it("starts backups for every selected row", async () => {
    const fetchMock = vi.fn(routes(status(30)));
    vi.stubGlobal("fetch", fetchMock);
    const { el, unmount } = await mount();
    el.querySelector<HTMLButtonElement>(".group-folded button")?.click();
    await nextTick();
    const boxes = el.querySelectorAll<HTMLInputElement>(".row-check input");
    for (const b of [boxes[0], boxes[1]]) {
      b.checked = true;
      b.dispatchEvent(new Event("change"));
    }
    await nextTick();
    const go = [...el.querySelectorAll<HTMLButtonElement>(".bulk-bar button")].find((b) => b.textContent?.includes("Back up 2 now"))!;
    go.click();
    await flush();
    const posts = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === "POST").map(([u]) => String(u));
    expect(posts).toHaveLength(2);
    expect(posts.every((u) => u.endsWith("/backup"))).toBe(true);
    unmount();
  });
});

describe("cancelling a running backup", () => {
  beforeEach(() => {
    localStorage.clear();
    pending.value = null;
  });
  afterEach(() => vi.unstubAllGlobals());

  const running = (cancelling = false): ServiceStatus[] => [
    { service: "photos", catalog_entry: "folder", destination: "nas", tiers: [{ tier: "restic", light: "running", cancelling }] },
  ];

  function cancelPosts(fetchMock: ReturnType<typeof vi.fn>) {
    return fetchMock.mock.calls
      .filter(([u, init]) => (init as RequestInit | undefined)?.method === "POST" && String(u).endsWith("/cancel"))
      .map(([u]) => String(u));
  }

  it("asks first, and stops nothing when the person keeps it running", async () => {
    const fetchMock = vi.fn(routes(running()));
    vi.stubGlobal("fetch", fetchMock);
    const { el, unmount } = await mount();
    const btn = el.querySelector<HTMLButtonElement>(".cancel-backup")!;
    expect(btn.textContent?.trim()).toBe("Cancel backup");
    btn.click();
    await flush();
    expect(pending.value?.message).toBe("Stop this backup? The last good backup is kept.");
    answerConfirm(false);
    await flush();
    expect(cancelPosts(fetchMock)).toHaveLength(0);
    unmount();
  });

  it("sends the stop once confirmed", async () => {
    const fetchMock = vi.fn(routes(running()));
    vi.stubGlobal("fetch", fetchMock);
    const { el, unmount } = await mount();
    el.querySelector<HTMLButtonElement>(".cancel-backup")!.click();
    await flush();
    answerConfirm(true);
    await flush();
    expect(cancelPosts(fetchMock)).toEqual(["/api/v1/services/photos/cancel"]);
    unmount();
  });

  it("shows Cancelling… while the backup tidies up", async () => {
    vi.stubGlobal("fetch", vi.fn(routes(running(true))));
    const { el, unmount } = await mount();
    const btn = el.querySelector<HTMLButtonElement>(".cancel-backup")!;
    expect(btn.textContent?.trim()).toBe("Cancelling…");
    expect(btn.disabled).toBe(true);
    expect(el.querySelector(".status-line")?.textContent).toContain("Stopping the backup");
    unmount();
  });
});
