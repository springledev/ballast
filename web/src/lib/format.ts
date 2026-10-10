// Small formatting helpers shared by views. Kept dependency-free
// (spec §10: "No node requirement for users" — the built bundle
// ships alone, so every extra library is bundle size and audit
// surface for no beginner-facing benefit here).

/** A byte count as a short size such as "1.5 GB". */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

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

/** A byte count in binary units, such as "36.9 GiB". */
function formatGiB(n: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return i === 0 ? `${n} B` : `${v.toFixed(1)} ${units[i]}`;
}

/**
 * Describes an upload: percentage and bytes first, time left second
 * (the estimate swings when other jobs share the connection).
 */
export function describeUpload(u: { percent: number; bytes_done: number; total_bytes: number; seconds_remaining?: number }): string {
  const text = `${Math.round(u.percent)}% — ${formatGiB(u.bytes_done)} of ${formatGiB(u.total_bytes)}`;
  if (!u.seconds_remaining) return text;
  const mins = Math.max(1, Math.round(u.seconds_remaining / 60));
  const left = mins >= 90 ? `${Math.round(mins / 60)} hours` : `${mins} minute${mins === 1 ? "" : "s"}`;
  return `${text} (about ${left} left)`;
}
