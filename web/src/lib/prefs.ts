// Small per-browser display preferences: colour theme and how long
// lists are shown. They live in this browser only and are never sent
// to the server; a private window just gets the defaults.
import { ref, watch, type Ref } from "vue";

/** "auto" follows the device's light or dark setting. */
export type Theme = "auto" | "light" | "dark";

const THEME_KEY = "ballast_theme";

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function write(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* not saved; still applies until the page is closed */
  }
}

/** Parses a stored theme, falling back to "auto" for anything unknown. */
export function parseTheme(v: string | null): Theme {
  return v === "light" || v === "dark" ? v : "auto";
}

/** The theme after this one when cycling with a single button. */
export function nextTheme(t: Theme): Theme {
  return t === "auto" ? "light" : t === "light" ? "dark" : "auto";
}

/** Plain-English name of a theme, for the toggle's label. */
export const themeLabel: Record<Theme, string> = {
  auto: "Match device",
  light: "Light",
  dark: "Dark",
};

/** Sets the theme on the page: "auto" removes the override. */
export function applyTheme(t: Theme, root: HTMLElement = document.documentElement): void {
  if (t === "auto") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", t);
}

/** The current theme, applied to the page and remembered whenever it changes. */
export const theme = ref<Theme>(parseTheme(read(THEME_KEY)));
applyTheme(theme.value);
watch(theme, (t) => {
  applyTheme(t);
  write(THEME_KEY, t);
});

/**
 * A preference remembered in this browser under key, limited to the
 * allowed values so an old or hand-edited value can't break a view.
 */
export function usePref<T extends string>(key: string, fallback: T, allowed: readonly T[]): Ref<T> {
  const stored = read(key) as T | null;
  const r = ref(stored && allowed.includes(stored) ? stored : fallback) as Ref<T>;
  watch(r, (v) => write(key, v));
  return r;
}
