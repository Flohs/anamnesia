/**
 * Value formatting for the console.
 *
 * The rule everywhere: a human reads the magnitude first and the precision
 * second, so 6204 ms is "6.2 s" rather than "6204 ms", and a thousands
 * separator is a narrow space rather than a comma, which reads as a decimal
 * point in the locale this is built for.
 */

const THIN_SPACE = "\u2009";

/** 37 -> "37 ms", 1840 -> "1 840 ms", 6204 -> "6.2 s", 90_000 -> "1.5 min". */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "-";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 5000) return `${groupThousands(Math.round(ms))} ms`;
  if (ms < 90_000) return `${(ms / 1000).toFixed(1)} s`;
  if (ms < 3_600_000) return `${(ms / 60_000).toFixed(1)} min`;
  return `${(ms / 3_600_000).toFixed(1)} h`;
}

/** Compact duration for dense columns: "37ms", "1.8s", "6.2s", "1.5m". */
export function formatDurationShort(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "-";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  if (ms < 3_600_000) return `${(ms / 60_000).toFixed(1)}m`;
  return `${(ms / 3_600_000).toFixed(1)}h`;
}

/**
 * "12s ago", "22m ago", "6h ago", "3d ago".
 *
 * `now` is injectable so tests do not depend on the wall clock.
 */
export function formatRelative(when: string | Date, now: Date = new Date()): string {
  const then = typeof when === "string" ? new Date(when) : when;
  const seconds = Math.round((now.getTime() - then.getTime()) / 1000);

  if (!Number.isFinite(seconds)) return "-";
  if (seconds < 0) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86_400)}d ago`;
}

/** "every 1s", "every 1m", "every 24h" for a worker loop's interval. */
export function formatInterval(ms: number): string {
  if (ms < 1000) return `every ${ms}ms`;
  if (ms < 60_000) return `every ${Math.round(ms / 1000)}s`;
  if (ms < 3_600_000) return `every ${Math.round(ms / 60_000)}m`;
  return `every ${Math.round(ms / 3_600_000)}h`;
}

/**
 * Coarse duration for uptime and other spans nobody reads precisely: "34m",
 * "6h", "3d". A tenth of a minute is noise on a number like this.
 */
export function formatUptime(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "-";
  if (ms < 60_000) return `${Math.round(ms / 1000)}s`;
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)}m`;
  if (ms < 86_400_000) return `${Math.round(ms / 3_600_000)}h`;
  return `${Math.round(ms / 86_400_000)}d`;
}

/** 4271 -> "4.2 kB". Decimal units, because that is what the CLI reports. */
export function formatBytes(bytes: number): string {
  if (bytes < 1000) return `${bytes} B`;
  if (bytes < 1_000_000) return `${(bytes / 1000).toFixed(1)} kB`;
  return `${(bytes / 1_000_000).toFixed(1)} MB`;
}

/** 1840 -> "1 840". Grouped with a thin space, never a comma. */
export function groupThousands(value: number): string {
  return String(value).replace(/\B(?=(\d{3})+(?!\d))/g, THIN_SPACE);
}

/** 0.713 -> "0.71". Scores are always two decimals so columns align. */
export function formatScore(score: number): string {
  return score.toFixed(2);
}

/** Joins parts with the middot separator used across the design. */
export function joinMeta(...parts: Array<string | number | null | undefined>): string {
  return parts.filter((p) => p !== null && p !== undefined && p !== "").join(" · ");
}

/** Truncates to a whole word, appending an ellipsis only if it had to cut. */
export function truncate(text: string, max: number): string {
  if (text.length <= max) return text;
  const cut = text.slice(0, max - 1);
  const lastSpace = cut.lastIndexOf(" ");
  return `${(lastSpace > max * 0.6 ? cut.slice(0, lastSpace) : cut).trimEnd()}\u2026`;
}
