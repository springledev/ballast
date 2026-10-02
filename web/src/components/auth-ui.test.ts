// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createApp, nextTick } from "vue";

import { freshKey, markSignedIn, session } from "@/api/auth";
import SecurityView from "@/views/SecurityView.vue";
import ConnectGate from "./ConnectGate.vue";

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

async function flush() {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
  await nextTick();
}

function mount(component: object, props: Record<string, unknown> = {}) {
  const el = document.createElement("div");
  document.body.appendChild(el);
  const app = createApp(component, props);
  app.mount(el);
  return { el, unmount: () => { app.unmount(); el.remove(); } };
}

describe("sign-in screen", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    freshKey.value = "";
  });
  afterEach(() => vi.unstubAllGlobals());

  async function submit(el: HTMLElement, key: string) {
    const input = el.querySelector<HTMLInputElement>("#token")!;
    input.value = key;
    input.dispatchEvent(new Event("input"));
    el.querySelector("form")!.dispatchEvent(new Event("submit", { cancelable: true }));
    await flush();
  }

  it("posts the key to /auth/login, tells the app, and does not keep the key", async () => {
    fetchMock.mockResolvedValue(json(200, { ok: true }));
    const onSignedIn = vi.fn();
    const { el, unmount } = mount(ConnectGate, { onSignedIn });
    await submit(el, " abc123 ");

    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/auth/login");
    expect(JSON.parse(init.body)).toEqual({ token: "abc123" });
    expect((init.headers as Record<string, string>).Authorization).toBeUndefined();
    expect(onSignedIn).toHaveBeenCalledTimes(1);
    expect(window.localStorage.length).toBe(0);
    unmount();
  });

  it("shows a plain-English message for a wrong key and for too many tries", async () => {
    fetchMock.mockResolvedValueOnce(json(401, { error: "that access key didn't work" }));
    const onSignedIn = vi.fn();
    const { el, unmount } = mount(ConnectGate, { onSignedIn });
    await submit(el, "bad");
    expect(el.textContent).toContain("didn't work");
    expect(onSignedIn).not.toHaveBeenCalled();

    fetchMock.mockResolvedValueOnce(json(429, { error: "slow down" }));
    await submit(el, "bad");
    expect(el.textContent).toContain("Too many tries");
    unmount();
  });

  it("shows a freshly rotated key once, and clears it after sign-in", async () => {
    freshKey.value = "new-key-1234";
    fetchMock.mockResolvedValue(json(200, { ok: true }));
    const { el, unmount } = mount(ConnectGate, {});
    expect(el.textContent).toContain("new-key-1234");
    expect(el.querySelector<HTMLInputElement>("#token")!.value).toBe("new-key-1234");
    await submit(el, "new-key-1234");
    expect(freshKey.value).toBe("");
    unmount();
  });
});

describe("security page", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("rotating signs this browser out and hands the new key to the sign-in screen", async () => {
    const fetchMock = vi.fn().mockResolvedValue(json(200, { token: "rotated-key" }));
    vi.stubGlobal("fetch", fetchMock);
    markSignedIn();
    freshKey.value = "";
    const { el, unmount } = mount(SecurityView);
    const rotate = [...el.querySelectorAll("button")].find((b) => b.textContent?.includes("Make a new key"))!;
    rotate.click();
    await flush();

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/auth/rotate");
    expect(freshKey.value).toBe("rotated-key");
    expect(session.value).toBe("out");
    unmount();
  });

  it("sign out everywhere calls the endpoint and signs out", async () => {
    const fetchMock = vi.fn().mockResolvedValue(json(200, { ok: true }));
    vi.stubGlobal("fetch", fetchMock);
    markSignedIn();
    const { el, unmount } = mount(SecurityView);
    [...el.querySelectorAll("button")].find((b) => b.textContent?.includes("Sign out everywhere"))!.click();
    await flush();

    expect(fetchMock.mock.calls[0][0]).toBe("/api/v1/auth/sign-out-everywhere");
    expect(session.value).toBe("out");
    unmount();
  });
});
