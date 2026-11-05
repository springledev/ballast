// @vitest-environment jsdom
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { createApp, h } from "vue";

import BrandMark from "./BrandMark.vue";

function shapes(root: Element): string[] {
  return [...root.querySelectorAll("rect, path")].map((el) =>
    [...el.attributes].map((a) => `${a.name}=${a.value}`).sort().join(" "),
  );
}

describe("BrandMark", () => {
  it("draws the same shapes as favicon.svg", () => {
    const host = document.createElement("div");
    createApp({ render: () => h(BrandMark) }).mount(host);
    const file = document.createElement("div");
    file.innerHTML = readFileSync(resolve(__dirname, "../../public/favicon.svg"), "utf8");
    expect(shapes(host)).toEqual(shapes(file));
    expect(shapes(host).length).toBeGreaterThan(0);
  });

  it("is hidden from screen readers, because the name sits next to it", () => {
    const host = document.createElement("div");
    createApp({ render: () => h(BrandMark, { size: 56 }) }).mount(host);
    const svg = host.querySelector("svg");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
    expect(svg?.getAttribute("width")).toBe("56");
  });
});
