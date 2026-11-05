// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { createApp, nextTick } from "vue";
import { createMemoryHistory, createRouter } from "vue-router";

import { markSignedIn } from "@/api/auth";
import App from "./App.vue";

afterEach(() => vi.unstubAllGlobals());

describe("skip to content link", () => {
  it("is the first focusable element and moves focus to the main area", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(() => Promise.resolve(new Response(JSON.stringify({ ok: true, insecure_exposure: false, version: "t" }), { status: 200, headers: { "Content-Type": "application/json" } }))),
    );
    markSignedIn();
    const router = createRouter({ history: createMemoryHistory(), routes: [{ path: "/:p(.*)*", component: { template: "<div/>" } }] });
    const el = document.createElement("div");
    document.body.appendChild(el);
    const app = createApp(App).use(router);
    app.mount(el);
    for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0));
    await nextTick();

    const focusable = el.querySelector<HTMLElement>("a[href], button, input, select, [tabindex]:not([tabindex='-1'])")!;
    expect(focusable.classList.contains("skip-link")).toBe(true);
    expect(focusable.textContent).toContain("Skip to content");
    const main = el.querySelector<HTMLElement>("main")!;
    expect(main.id).toBe("main");
    expect(main.getAttribute("tabindex")).toBe("-1");

    focusable.click();
    expect(document.activeElement).toBe(main);
    app.unmount();
    el.remove();
  });
});
