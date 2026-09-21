/**
 * Colour for the constellation.
 *
 * Both maps draw on the console's existing semantic palette rather than
 * inventing a set: a fact is the same blue here that it is in the Memory
 * tables, so moving between views does not mean relearning the colours.
 */

/** The semantic hues from theme.css, in the order the palette declares them. */
const WHEEL = [
  "#ff7a45", // coral
  "#7cc7f0", // sky
  "#8fe4b8", // mint
  "#c4a3f5", // lilac
  "#f5c969", // amber
  "#f08b9c", // rose
] as const;

const DOMAIN: Record<string, string> = {
  facts: "#7cc7f0",
  experiences: "#ff7a45",
  entities: "#c4a3f5",
  edges: "#f5c969",
  sources: "#8fe4b8",
  commitments: "#f08b9c",
  artifacts: "#ff7a45",
  skills: "#f5c969",
};

/** Anything the palette has no opinion on reads as inert rather than wrong. */
const UNCOLOURED = "#65656f";

export function domainHue(domain: string): string {
  return DOMAIN[domain] ?? UNCOLOURED;
}

/**
 * Assigns each project a hue from the projects actually present.
 *
 * Hashing the slug seemed tidier until the four projects on the live install
 * produced three colours: a hash can only promise the same answer twice, never
 * a different one from a different slug. Drawing from the set present promises
 * distinctness, which is the property the ring needs, and sorting first makes
 * it independent of the order the server happened to list them in.
 */
export function projectHues(
  projects: Iterable<string | null | undefined>,
): (project: string | null | undefined) => string {
  const slugs = [...new Set([...projects].filter((p): p is string => Boolean(p)))].sort();
  const assigned = new Map(slugs.map((slug, index) => [slug, WHEEL[index % WHEEL.length]!]));

  return (project) => (project && assigned.get(project)) || UNCOLOURED;
}
