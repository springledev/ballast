import { describe, expect, it } from "vitest";

import { removedText, removeQuestion, removeWaitText } from "./removal";

describe("removal wording", () => {
  it("tells the person Ballast is waiting for the running backup", () => {
    expect(removeWaitText("notes")).toContain("Waiting for the running backup to finish…");
    expect(removeWaitText("notes")).toContain("notes");
  });

  it.each([
    ["question", removeQuestion("notes")],
    ["result", removedText("notes")],
  ])("%s names the backup and says existing backups are kept", (_n, text) => {
    expect(text).toContain("notes");
    expect(text).toMatch(/already stored|still in your destinations/);
    expect(text).not.toMatch(/Stop backing up/);
  });
});
