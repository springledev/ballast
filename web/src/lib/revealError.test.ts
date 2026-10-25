import { afterEach, describe, expect, it, vi } from "vitest";
import { ref } from "vue";

import { revealOnError } from "./revealError";

describe("revealOnError", () => {
  afterEach(() => {
    document.body.innerHTML = "";
  });

  function notice(text: string): HTMLElement {
    const el = document.createElement("p");
    el.className = "notice notice--error";
    el.setAttribute("role", "alert");
    el.textContent = text;
    el.scrollIntoView = vi.fn();
    document.body.append(el);
    return el;
  }

  const cases = [
    { name: "scrolls to the notice showing the message", message: "Fill in the path.", want: 1 },
    { name: "does nothing when the error is cleared", message: "", want: 0 },
  ];

  for (const c of cases) {
    it(c.name, async () => {
      const error = ref("");
      const el = notice("Fill in the path.");
      revealOnError(error);
      error.value = c.message;
      await Promise.resolve();
      await Promise.resolve();
      expect(vi.mocked(el.scrollIntoView)).toHaveBeenCalledTimes(c.want);
    });
  }

  it("scrolls again when the same message is set after being cleared", async () => {
    const error = ref("");
    const el = notice("Fill in the path.");
    revealOnError(error);
    for (let i = 0; i < 2; i++) {
      error.value = "";
      error.value = "Fill in the path.";
      await Promise.resolve();
      await Promise.resolve();
    }
    expect(vi.mocked(el.scrollIntoView)).toHaveBeenCalledTimes(2);
  });

  it("ignores other error notices on the page", async () => {
    const error = ref("");
    const other = notice("Something else broke.");
    const mine = notice("Fill in the path.");
    revealOnError(error);
    error.value = "Fill in the path.";
    await Promise.resolve();
    await Promise.resolve();
    expect(vi.mocked(other.scrollIntoView)).not.toHaveBeenCalled();
    expect(vi.mocked(mine.scrollIntoView)).toHaveBeenCalledTimes(1);
  });
});
