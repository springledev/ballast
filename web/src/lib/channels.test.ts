import { describe, it, expect } from "vitest";
import { buildChannelInput, defaultNtfyUrl } from "./channels";

describe("channels", () => {
  it("buildChannelInput builds minimal input without username or token", () => {
    const input = buildChannelInput("my-channel", "https://ntfy.sh/my-topic", undefined, undefined, false, true, []);
    expect(input).toEqual({
      name: "my-channel",
      type: "ntfy",
      url: "https://ntfy.sh/my-topic",
      apply_to_all: true,
      services: [],
    });
  });

  it("buildChannelInput includes username when provided", () => {
    const input = buildChannelInput("my-channel", "https://ntfy.sh/my-topic", "user123", undefined, false, true, []);
    expect(input.username).toBe("user123");
  });

  it("buildChannelInput includes token when provided", () => {
    const input = buildChannelInput("my-channel", "https://ntfy.sh/my-topic", undefined, "secret-token", false, true, []);
    expect(input.token).toBe("secret-token");
  });

  it("buildChannelInput sets clear_token when true", () => {
    const input = buildChannelInput("my-channel", "https://ntfy.sh/my-topic", undefined, undefined, true, false, ["svc1"]);
    expect(input.clear_token).toBe(true);
  });

  it("buildChannelInput includes services list when not apply_to_all", () => {
    const input = buildChannelInput("my-channel", "https://ntfy.sh/my-topic", undefined, undefined, false, false, ["photos", "vault"]);
    expect(input.apply_to_all).toBe(false);
    expect(input.services).toEqual(["photos", "vault"]);
  });

  it("defaultNtfyUrl is set", () => {
    expect(defaultNtfyUrl).toBe("https://ntfy.sh/");
  });
});
