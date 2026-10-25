// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createApp, nextTick } from "vue";

import type { Service, Snapshot } from "@/api/types";
import RestoreView from "./RestoreView.vue";

const services: Service[] = [
  { name: "wiki", catalog_entry: "folder", destination: "nas", targets: [{ destination: "nas" }, { destination: "b2" }] },
  { name: "vault", catalog_entry: "vaultwarden", destination: "nas", targets: [{ destination: "nas" }, { destination: "b2" }] },
];

function snap(id: string): Snapshot {
  return { id, short_id: id.slice(0, 4), time: "2026-10-20T03:00:00Z", hostname: "host", paths: ["/data"] };
}

function json(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
}

interface PendingSnapshots {
  url: string;
  respond(body: Snapshot[]): void;
}

// Snapshot requests are held open until the test answers them, so the test
// decides which response arrives first. Everything else answers at once.
function installFetch() {
  const snapshotCalls: PendingSnapshots[] = [];
  const restoreBodies: unknown[] = [];
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    if (url.includes("/snapshots")) {
      return new Promise<Response>((resolve) => {
        snapshotCalls.push({ url, respond: (body) => resolve(json(body)) });
      });
    }
    if (url.endsWith("/restore")) {
      restoreBodies.push(JSON.parse(String(init?.body)));
      return Promise.resolve(json({ restored: true }));
    }
    if (url.endsWith("/services")) return Promise.resolve(json(services));
    if (url.endsWith("/destinations")) return Promise.resolve(json([{ name: "nas" }, { name: "b2" }]));
    return Promise.resolve(json([]));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { fetchMock, snapshotCalls, restoreBodies };
}

async function flush() {
  for (let i = 0; i < 8; i++) await new Promise((r) => setTimeout(r, 0));
  await nextTick();
}

function pick(el: HTMLElement, selector: string, value: string) {
  const select = el.querySelector<HTMLSelectElement>(selector)!;
  select.value = value;
  select.dispatchEvent(new Event("change"));
}

function snapshotOptions(el: HTMLElement): string[] {
  return [...el.querySelectorAll<HTMLOptionElement>("#restore-snapshot option")].map((o) => o.value);
}

async function mount() {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const app = createApp(RestoreView);
  app.mount(el);
  await flush();
  return { el, unmount: () => (app.unmount(), el.remove()) };
}

describe("restore picker", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  it("ignores a slow response for the off-site copy that arrives after the main copy's", async () => {
    const { snapshotCalls, restoreBodies } = installFetch();
    const { el, unmount } = await mount();

    pick(el, "#restore-service", "wiki");
    await flush();
    snapshotCalls[0].respond([snap("main-1")]);
    await flush();

    // Off-site copy: slow. Then switch back to the main copy, which answers at once.
    pick(el, "#restore-from", "b2");
    await flush();
    pick(el, "#restore-from", "");
    await flush();
    expect(snapshotCalls[2].url).not.toContain("from=");
    snapshotCalls[2].respond([snap("main-2")]);
    await flush();

    // The stale off-site answer lands last and must not win.
    snapshotCalls[1].respond([snap("b2-1")]);
    await flush();

    expect(snapshotOptions(el)).toEqual(["main-2"]);
    expect(el.querySelector<HTMLSelectElement>("#restore-snapshot")!.value).toBe("main-2");

    // The restore sends the snapshot from the copy that is shown.
    el.querySelector<HTMLFormElement>("form.panel")!.dispatchEvent(new Event("submit", { cancelable: true }));
    await flush();
    expect(restoreBodies).toEqual([expect.objectContaining({ snapshot: "main-2" })]);
    unmount();
  });

  it("ignores a stale list after switching to another service", async () => {
    const { snapshotCalls } = installFetch();
    const { el, unmount } = await mount();

    pick(el, "#restore-service", "wiki");
    await flush();
    snapshotCalls[0].respond([snap("main-1")]);
    await flush();
    pick(el, "#restore-from", "b2");
    await flush();

    // Switch service while wiki's off-site list is still out.
    pick(el, "#restore-service", "vault");
    await flush();
    const vaultCall = snapshotCalls.find((c) => c.url.includes("/services/vault/snapshots"))!;
    expect(vaultCall.url).not.toContain("from=");
    vaultCall.respond([snap("vault-1")]);
    await flush();

    snapshotCalls.find((c) => c.url.includes("/services/wiki/snapshots") && c.url.includes("from=b2"))!.respond([snap("wiki-b2")]);
    await flush();

    expect(snapshotOptions(el)).toEqual(["vault-1"]);
    expect(el.querySelector<HTMLSelectElement>("#restore-snapshot")!.value).toBe("vault-1");
    unmount();
  });

  it("asks for snapshots once when the service changes, even if an off-site copy was picked", async () => {
    const { snapshotCalls } = installFetch();
    const { el, unmount } = await mount();

    pick(el, "#restore-service", "wiki");
    await flush();
    snapshotCalls[0].respond([snap("main-1")]);
    await flush();
    pick(el, "#restore-from", "b2");
    await flush();
    snapshotCalls[1].respond([snap("wiki-b2")]);
    await flush();

    const before = snapshotCalls.length;
    pick(el, "#restore-service", "vault");
    await flush();
    const after = snapshotCalls.slice(before);
    expect(after).toHaveLength(1);
    expect(after[0].url).toContain("/services/vault/snapshots");
    expect(after[0].url).not.toContain("from=");
    unmount();
  });

  it("shows no snapshot choice while a new list is loading", async () => {
    const { snapshotCalls } = installFetch();
    const { el, unmount } = await mount();

    pick(el, "#restore-service", "wiki");
    await flush();
    snapshotCalls[0].respond([snap("main-1")]);
    await flush();
    expect(el.querySelector("#restore-snapshot")).not.toBeNull();

    pick(el, "#restore-from", "b2");
    await flush();
    expect(el.querySelector("#restore-snapshot")).toBeNull();
    expect(el.querySelector(".loading")?.textContent).toContain("Looking for backups");
    unmount();
  });
});

describe("restore picker with many backups", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => vi.unstubAllGlobals());

  const many: Service[] = Array.from({ length: 10 }, (_, i) => ({
    name: `app-${String(i).padStart(2, "0")}`,
    catalog_entry: "folder",
    destination: "nas",
  }));

  function stubMany() {
    vi.stubGlobal(
      "fetch",
      vi.fn((url: string) => Promise.resolve(json(url.endsWith("/services") ? many : url.endsWith("/destinations") ? [{ name: "nas" }] : []))),
    );
  }

  function optionNames(el: HTMLElement): string[] {
    return [...el.querySelectorAll<HTMLOptionElement>("#restore-service option")].map((o) => o.value).filter(Boolean);
  }

  it("has no filter box for a handful of backups", async () => {
    installFetch();
    const { el, unmount } = await mount();
    expect(el.querySelector(".restore-filter")).toBeNull();
    unmount();
  });

  it("narrows the list as you type, and keeps the chosen backup in it", async () => {
    stubMany();
    const { el, unmount } = await mount();
    const filter = el.querySelector<HTMLInputElement>(".restore-filter")!;
    expect(optionNames(el)).toHaveLength(10);

    filter.value = "app-03";
    filter.dispatchEvent(new Event("input"));
    await nextTick();
    expect(optionNames(el)).toEqual(["app-03"]);

    pick(el, "#restore-service", "app-03");
    await flush();
    filter.value = "app-07";
    filter.dispatchEvent(new Event("input"));
    await nextTick();
    expect(optionNames(el)).toEqual(["app-03", "app-07"]);
    unmount();
  });
});
