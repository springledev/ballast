import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setApiToken, apiToken } from "./auth";
import { api, ApiError } from "./client";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("api client", () => {
  beforeEach(() => {
    setApiToken("test-token");
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    setApiToken("");
  });

  it("sends the bearer token on every request", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, []));
    vi.stubGlobal("fetch", fetchMock);

    await api.listServices();

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, init] = fetchMock.mock.calls[0];
    expect((init.headers as Record<string, string>).Authorization).toBe("Bearer test-token");
  });

  it("throws ApiError with the server's error message on failure", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(404, { error: "service x: not found" }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(api.getService("x")).rejects.toMatchObject(
      new ApiError(404, "service x: not found"),
    );
  });

  it("clears the stored token on a 401 response", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(401, { error: "missing or invalid API token" }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(api.listServices()).rejects.toThrow();
    expect(apiToken.value).toBe("");
  });

  it("returns undefined for a 204 No Content response", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(api.deleteService("x")).resolves.toBeUndefined();
  });

  it("sends the typed destination request body as-is", async () => {
    const body = {
      name: "b2-bucket",
      type: "b2" as const,
      settings: { endpoint: "s3.us-west-004.backblazeb2.com", bucket: "my-bucket" },
      keep_daily: 7,
      keep_weekly: 4,
      keep_monthly: 6,
      init: true,
      credentials: { access_key_id: "id", secret_access_key: "secret" },
    };
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(201, {
        name: "b2-bucket",
        type: "b2",
        settings: body.settings,
        location: "s3:https://s3.us-west-004.backblazeb2.com/my-bucket",
        password_credential: "ballast-restic-password.b2-bucket",
        backend_credential: "ballast-dest-keys.b2-bucket",
        keep_daily: 7,
        keep_weekly: 4,
        keep_monthly: 6,
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const saved = await api.createDestination(body);

    expect(saved.location).toBe("s3:https://s3.us-west-004.backblazeb2.com/my-bucket");
    expect(saved).not.toHaveProperty("credentials");
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/destinations");
    expect(JSON.parse(init.body as string)).toEqual(body);
  });

  it("tests a destination by name", async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { ok: true }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(api.testDestination("nas")).resolves.toEqual({ ok: true });
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/v1/destinations/nas/test");
    expect(init.method).toBe("POST");
  });
});
