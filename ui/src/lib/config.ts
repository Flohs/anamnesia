import type { Config } from "@/api/types";

/**
 * Reads a decay half-life out of the server's configuration.
 *
 * The decay chart overlays these curves, and overlaying a hard-coded guess
 * next to real rows would be a chart that lies whenever the config changes.
 */
export function halfLifeDays(config: Config | undefined, kind: string, fallbackDays: number): number {
  // The setting is `decay.half_life_case`, with an underscore before the kind
  // and dots only in the namespace. Looking for a dot here silently fell back
  // to a hard-coded default, which drew a curve that disagreed with the
  // running configuration.
  const entry = config?.items.find((item) => item.key === `decay.half_life_${kind}`);
  const hours = entry ? parseDurationHours(entry.value) : undefined;
  return hours === undefined ? fallbackDays : hours / 24;
}

/** Go duration strings, as the config file writes them: "504h", "30m", "1h30m". */
export function parseDurationHours(value: string): number | undefined {
  const matches = value.matchAll(/(\d+(?:\.\d+)?)(h|m|s)/g);
  let hours = 0;
  let matched = false;

  for (const match of matches) {
    const amount = Number(match[1]);
    if (!Number.isFinite(amount)) continue;
    matched = true;
    if (match[2] === "h") hours += amount;
    if (match[2] === "m") hours += amount / 60;
    if (match[2] === "s") hours += amount / 3600;
  }

  return matched ? hours : undefined;
}
