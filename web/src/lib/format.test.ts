import { describe, expect, it } from "vitest";

import { formatAgeSeconds, formatBytes, formatDurationMs, describeUpload } from "./format";

describe("formatAgeSeconds", () => {
  it("returns 'never' for undefined", () => {
    expect(formatAgeSeconds(undefined)).toBe("never");
  });

  it("formats minutes, hours, and days", () => {
    expect(formatAgeSeconds(30)).toBe("just now");
    expect(formatAgeSeconds(120)).toBe("2 minutes ago");
    expect(formatAgeSeconds(3600)).toBe("1 hour ago");
    expect(formatAgeSeconds(2 * 86400)).toBe("2 days ago");
  });
});

describe("formatDurationMs", () => {
  it("formats sub-second durations in ms", () => {
    expect(formatDurationMs(500)).toBe("500 ms");
  });

  it("formats seconds", () => {
    expect(formatDurationMs(1500)).toBe("1.5 s");
  });

  it("formats minutes and seconds", () => {
    expect(formatDurationMs(65000)).toBe("1m 5s");
  });
});

describe("formatBytes", () => {
  it("scales to a short size", () => {
    expect(formatBytes(12)).toBe("12 B");
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatBytes(5 * 1024 * 1024)).toBe("5.0 MB");
  });
});

describe("describeUpload", () => {
  const g = 1024 ** 3;
  it("shows percentage and bytes first", () => {
    expect(describeUpload({ percent: 43.9, bytes_done: 36.9 * g, total_bytes: 84.1 * g })).toBe("44% — 36.9 GiB of 84.1 GiB");
  });
  it("adds time left as secondary", () => {
    expect(describeUpload({ percent: 50, bytes_done: g, total_bytes: 2 * g, seconds_remaining: 600 })).toBe("50% — 1.0 GiB of 2.0 GiB (about 10 minutes left)");
  });
});
