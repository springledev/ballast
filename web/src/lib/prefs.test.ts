// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { nextTick } from "vue";

import { applyTheme, nextTheme, parseTheme, usePref } from "./prefs";

describe("parseTheme", () => {
  it.each([
    [null, "auto"],
    ["light", "light"],
    ["dark", "dark"],
    ["purple", "auto"],
  ])("%j -> %s", (v, want) => {
    expect(parseTheme(v)).toBe(want);
  });
});

describe("nextTheme", () => {
  it("cycles auto, light, dark and back", () => {
    expect(nextTheme("auto")).toBe("light");
    expect(nextTheme("light")).toBe("dark");
    expect(nextTheme("dark")).toBe("auto");
  });
});

describe("applyTheme", () => {
  it("sets and clears the override", () => {
    const el = document.createElement("html");
    applyTheme("dark", el);
    expect(el.getAttribute("data-theme")).toBe("dark");
    applyTheme("auto", el);
    expect(el.hasAttribute("data-theme")).toBe(false);
  });
});

describe("usePref", () => {
  it("ignores stored values that are not allowed and remembers changes", async () => {
    localStorage.setItem("t_pref", "bogus");
    const p = usePref("t_pref", "cards", ["cards", "compact"] as const);
    expect(p.value).toBe("cards");
    p.value = "compact";
    await nextTick();
    expect(localStorage.getItem("t_pref")).toBe("compact");
    expect(usePref("t_pref", "cards", ["cards", "compact"] as const).value).toBe("compact");
  });
});
