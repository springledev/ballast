// Checks a service's parameters before anything is saved, so a typo is
// caught on the step where it was made, not after the whole wizard.
import type { CatalogParameter } from "@/api/types";

/**
 * Returns what is wrong with one parameter's value in plain English, or ""
 * when it is fine. A parameter whose prompt asks for an "absolute path"
 * must start with "/".
 */
export function checkParam(p: CatalogParameter, value: string | undefined): string {
  const v = value ?? "";
  const label = p.prompt || p.name;
  if (p.required && !v.trim()) return `Fill in "${label}".`;
  if (v && /absolute path/i.test(p.prompt ?? "") && !v.startsWith("/")) {
    return `Use a full path that starts with /, like /srv/data. "${v}" is relative.`;
  }
  return "";
}

/**
 * Turns the warnings the server returns after saving a service (for
 * example a folder that does not exist yet) into one sentence to show
 * after "Saved", or "" when there are none.
 */
export function warningsNote(warnings: string[] | undefined): string {
  const w = (warnings ?? []).filter((x) => x.trim());
  if (!w.length) return "";
  return `Saved, but note: ${w.join(" ")}`;
}
