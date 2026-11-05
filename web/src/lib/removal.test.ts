import { expect, it } from "vitest";

import { removeWaitText } from "./removal";

it("tells the person Ballast is waiting for the running backup", () => {
  expect(removeWaitText("notes")).toContain("Waiting for the running backup to finish…");
  expect(removeWaitText("notes")).toContain("notes");
});
