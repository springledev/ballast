import { beforeEach, describe, expect, it } from "vitest";

import { answerConfirm, askConfirm, pending } from "./confirm";

describe("askConfirm", () => {
  beforeEach(() => {
    pending.value = null;
  });

  const cases = [
    { name: "resolves true when confirmed", answer: true },
    { name: "resolves false when cancelled", answer: false },
  ];

  for (const c of cases) {
    it(c.name, async () => {
      const result = askConfirm("Forget it?", "Forget");
      expect(pending.value?.message).toBe("Forget it?");
      expect(pending.value?.confirmLabel).toBe("Forget");
      answerConfirm(c.answer);
      await expect(result).resolves.toBe(c.answer);
      expect(pending.value).toBeNull();
    });
  }

  it("answers an earlier question no when a new one is asked", async () => {
    const first = askConfirm("First?");
    const second = askConfirm("Second?");
    await expect(first).resolves.toBe(false);
    expect(pending.value?.message).toBe("Second?");
    answerConfirm(true);
    await expect(second).resolves.toBe(true);
  });

  it("ignores an answer when nothing is asked", () => {
    expect(() => answerConfirm(true)).not.toThrow();
  });
});
