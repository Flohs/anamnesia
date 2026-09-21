import type { Experience } from "@/api/types";
import { truncate } from "@/lib/format";

/**
 * What to call an experience.
 *
 * The extractor frequently writes a body without a title, so a table keyed on
 * `title` shows a column of blanks against a real install. The first sentence
 * of the body is what a person would call it anyway.
 */
export function experienceTitle(experience: Pick<Experience, "title" | "body">): string {
  const title = experience.title?.trim();
  if (title) return title;

  const body = experience.body.trim();
  // An empty body used to produce an empty cell, which reads as a rendering
  // fault rather than as a row the extractor left unnamed.
  if (body === "") return "Untitled";

  const firstSentence = /^(.{10,120}?[.!?])(\s|$)/.exec(body);
  if (firstSentence?.[1]) return firstSentence[1];

  return truncate(body.split("\n")[0] ?? body, 90);
}
