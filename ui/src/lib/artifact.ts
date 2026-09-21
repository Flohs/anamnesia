import type { Artifact } from "@/api/types";
import { truncate } from "@/lib/format";

/** Longest label a ring tile or a list row can carry without wrapping. */
const LABEL_CHARS = 60;

/**
 * What to call an artifact.
 *
 * The capture hook records a description far more reliably than a title: on
 * the live install roughly half of them arrive titled, and every one of them
 * carries a description. Falling back to it turns a ring of "untitled" tiles
 * into a ring that can be read.
 */
export function artifactName(artifact: Pick<Partial<Artifact>, "title" | "description">): string {
  const title = artifact.title?.trim();
  if (title) return title;

  const description = artifact.description?.trim();
  if (!description) return "Untitled artifact";

  // A description is a sentence and a label is not, so the full stop goes.
  return truncate(description, LABEL_CHARS).replace(/\.$/, "");
}
