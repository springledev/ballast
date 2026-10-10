// Static checks on the view templates for the phone layout: every
// stacked table labels its cells, and no screen talks about internal
// planning documents.
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const dirs = [join(__dirname), join(__dirname, "..", "components")];
const files = dirs.flatMap((d) => readdirSync(d).filter((f) => f.endsWith(".vue")).map((f) => join(d, f)));
const template = (f: string) => {
  const s = readFileSync(f, "utf8");
  return s.slice(s.indexOf("<template>"));
};

describe("view templates", () => {
  it.each(files)("%s: tables stack on a phone with labelled cells", (f) => {
    const t = template(f);
    for (const table of t.match(/<table[\s\S]*?<\/table>/g) ?? []) {
      expect(table).toContain('<table class="stack">');
      const body = table.slice(table.indexOf("<tbody>"));
      for (const td of body.match(/<td[^>]*>/g) ?? []) {
        if (!td.includes("colspan")) expect(td).toContain("data-label=");
      }
    }
  });

  it.each(files)("%s: no planning references in user-facing text", (f) => {
    const t = template(f).replace(/<!--[\s\S]*?-->/g, "");
    expect(t).not.toMatch(/spec §|§\d|Phase 2|PLAN\.md|step \d+ (in|of) the plan/);
  });
});
