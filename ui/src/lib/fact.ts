import type { Fact } from "@/api/types";

/**
 * Renders a fact's value as a person would say it.
 *
 * Values are JSONB, and the extractor wraps scalars as `{"v": 8}`. Printing
 * that wrapper would put punctuation from our storage format in front of the
 * reader for no reason, so a single-key `v` is unwrapped and everything else
 * is shown as compact JSON.
 */
export function factValue(value: Fact["value"]): string {
  const keys = Object.keys(value);

  if (keys.length === 1 && keys[0] === "v") {
    const inner = value["v"];
    return typeof inner === "string" ? inner : JSON.stringify(inner);
  }

  return JSON.stringify(value);
}
