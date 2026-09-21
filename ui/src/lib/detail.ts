/**
 * Safe readers for step detail.
 *
 * A step's detail is `Record<string, unknown>` by contract: the binary may add
 * keys at any time and the console must not crash on a shape it has not seen.
 * These readers return undefined rather than throwing, so a renderer degrades
 * to showing less instead of showing an error boundary.
 */

export type Detail = Record<string, unknown>;

export function readString(detail: Detail, key: string): string | undefined {
  const value = detail[key];
  return typeof value === "string" && value !== "" ? value : undefined;
}

export function readNumber(detail: Detail, key: string): number | undefined {
  const value = detail[key];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

export function readBoolean(detail: Detail, key: string): boolean | undefined {
  const value = detail[key];
  return typeof value === "boolean" ? value : undefined;
}

export function readArray(detail: Detail, key: string): unknown[] {
  const value = detail[key];
  return Array.isArray(value) ? value : [];
}

export function readStringArray(detail: Detail, key: string): string[] {
  return readArray(detail, key).filter((item): item is string => typeof item === "string");
}

/** A scored candidate, as every retrieval-ish step reports them. */
export interface DetailHit {
  id: string;
  title: string;
  kind?: string;
  score?: number;
}

export function readHits(detail: Detail, key = "hits"): DetailHit[] {
  return readArray(detail, key).flatMap((item) => {
    if (typeof item !== "object" || item === null) return [];
    const row = item as Detail;
    const title = readString(row, "title");
    if (!title) return [];

    return [
      {
        id: readString(row, "id") ?? title,
        title,
        ...(readString(row, "kind") !== undefined ? { kind: readString(row, "kind") as string } : {}),
        ...(readNumber(row, "score") !== undefined ? { score: readNumber(row, "score") as number } : {}),
      },
    ];
  });
}

/**
 * Keys a bespoke renderer has already shown, so the generic fallback can list
 * what is left without repeating anything.
 */
export function remainingEntries(detail: Detail, shown: readonly string[]): Array<[string, string]> {
  return Object.entries(detail)
    .filter(([key, value]) => !shown.includes(key) && isPrintable(value))
    .map(([key, value]) => [key.replace(/_/g, " "), String(value)] as [string, string]);
}

function isPrintable(value: unknown): boolean {
  return typeof value === "string" || typeof value === "number" || typeof value === "boolean";
}
