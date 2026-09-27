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
});
