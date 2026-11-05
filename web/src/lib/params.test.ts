import { describe, expect, it } from "vitest";

import { checkParam, warningsNote } from "./params";

const path = { name: "path", required: true, prompt: "Absolute path to the folder you want backed up" };

describe("checkParam", () => {
  const cases = [
    { name: "accepts a full path", p: path, v: "/srv/data", want: "" },
    { name: "rejects an empty required value", p: path, v: "", want: `Fill in "${path.prompt}".` },
    { name: "rejects spaces only", p: path, v: "   ", want: `Fill in "${path.prompt}".` },
    { name: "rejects a relative path", p: path, v: "relative/path", want: `Use a full path that starts with /, like /srv/data. "relative/path" is relative.` },
    { name: "rejects a path with a leading space", p: path, v: " /tmp", want: `Use a full path that starts with /, like /srv/data. " /tmp" is relative.` },
    { name: "allows unicode paths", p: path, v: "/srv/données/日本", want: "" },
    { name: "leaves an optional empty value alone", p: { name: "x", prompt: "Absolute path to skip" }, v: "", want: "" },
    { name: "does not police other prompts", p: { name: "db", required: true, prompt: "Database name" }, v: "mydb", want: "" },
    { name: "falls back to the name without a prompt", p: { name: "db", required: true }, v: "", want: `Fill in "db".` },
  ];

  for (const c of cases) {
    it(c.name, () => {
      expect(checkParam(c.p, c.v)).toBe(c.want);
    });
  }
});

describe("warningsNote", () => {
  const cases = [
    { name: "nothing to say", w: undefined, want: "" },
    { name: "empty list", w: [], want: "" },
    { name: "one warning", w: ["/mnt/x does not exist yet; it is saved, but backups fail until it does (is the drive mounted?)"], want: "Saved, but note: /mnt/x does not exist yet; it is saved, but backups fail until it does (is the drive mounted?)" },
    { name: "several", w: ["a.", "b."], want: "Saved, but note: a. b." },
  ];
  for (const c of cases) {
    it(c.name, () => {
      expect(warningsNote(c.w)).toBe(c.want);
    });
  }
});
