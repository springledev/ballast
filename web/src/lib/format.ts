// Small formatting helpers shared by views. Kept dependency-free
// (spec §10: "No node requirement for users" — the built bundle
// ships alone, so every extra library is bundle size and audit
// surface for no beginner-facing benefit here).

export function formatRelative(iso?: string): string {
  if (!iso) return "never";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return iso;
  return formatAgeSeconds((Date.now() - then) / 1000);
}

export function formatAgeSeconds(seconds?: number): string {
  if (seconds === undefined || seconds === null || Number.isNaN(seconds)) return "never";
  if (seconds < 0) return "just now";
  const units: [string, number][] = [
    ["day", 86400],
    ["hour", 3600],
    ["minute", 60],
  ];
  for (const [name, size] of units) {
    if (seconds >= size) {
      const n = Math.floor(seconds / size);
      return `${n} ${name}${n === 1 ? "" : "s"} ago`;
    }
  }
  return "just now";
}

export function formatDateTime(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

export function formatDurationMs(ms: number): string {
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)} s`;
  return `${Math.floor(s / 60)}m ${Math.round(s % 60)}s`;
}
