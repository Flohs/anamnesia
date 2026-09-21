import type { Tone } from "@/lib/tone";
import type { StepTiming, TraceSummary } from "@/api/types";

/**
 * The trace strip.
 *
 * anamnesia.dev draws its retrieval pipeline as a segmented latency bar. The
 * console reuses that language for live traces: one segment per step, sized by
 * how long the step actually took, so a glance answers "where did the time
 * go" before any text is read.
 */

/**
 * Tone per step name, covering both pipelines:
 * neutral inputs, amber decisions, sky/blue candidate fetching, coral model
 * calls, lilac model output, mint writes and returns.
 */
const STEP_TONE: Record<string, Tone> = {
  source: "neutral",
  query: "neutral",
  scopes: "neutral",
  gate: "warn",
  fuse: "warn",
  similar: "info",
  vector: "info",
  cluster: "info",
  lexical: "ok",
  llm: "run",
  distil: "run",
  rerank: "run",
  ops: "special",
  apply: "ok",
  write: "ok",
  return: "ok",
};

/** Unknown step names still render, in the quiet tone. */
export function stepTone(name: string): Tone {
  return STEP_TONE[name] ?? "neutral";
}

/**
 * Below this share of the total, a segment is too narrow to hold its label
 * without clipping mid-word, so it renders as a bare block with the label in
 * its tooltip instead.
 */
const LABEL_MIN_SHARE = 0.055;

/** A segment narrower than this would vanish, so it is floored to stay visible. */
export const SEGMENT_MIN_PX = 4;
export const SEGMENT_LABELLED_MIN_PX = 46;

export interface StripSegment {
  key: string;
  name: string;
  label: string;
  tone: Tone;
  durationMs: number;
  /** Fraction of the trace's total step time, 0..1. */
  share: number;
  showLabel: boolean;
}

export interface Strip {
  segments: StripSegment[];
  totalMs: number;
  /** True when the trace stopped early, so the bar gets a terminal marker. */
  truncated: boolean;
}

/**
 * Builds the strip for one trace.
 *
 * Steps of zero duration still get a segment: a step that ran is worth seeing
 * even when it was too fast to measure, and dropping it would silently rewrite
 * the pipeline.
 */
export function buildStrip(steps: readonly StepTiming[], status: TraceSummary["status"]): Strip {
  const totalMs = steps.reduce((sum, step) => sum + Math.max(step.duration_ms, 0), 0);

  const segments = steps.map((step, index): StripSegment => {
    const durationMs = Math.max(step.duration_ms, 0);
    // With no measurable time anywhere, share the width evenly rather than
    // dividing by zero and collapsing every segment.
    const share = totalMs > 0 ? durationMs / totalMs : 1 / Math.max(steps.length, 1);

    return {
      key: `${index}-${step.name}`,
      name: step.name,
      label: stepLabel(step),
      tone: stepTone(step.name),
      durationMs,
      share,
      showLabel: share >= LABEL_MIN_SHARE,
    };
  });

  return { segments, totalMs, truncated: status === "failed" };
}

/**
 * The label inside a segment. A step may name the thing it used (a model, an
 * index) in its detail; that is more informative than the step name alone.
 */
function stepLabel(step: StepTiming): string {
  const detail = (step.detail ?? {}) as { model?: unknown; verdict?: unknown };

  if (typeof detail.model === "string" && detail.model !== "") {
    return `${step.name} · ${shortModel(detail.model)}`;
  }
  if (typeof detail.verdict === "string" && detail.verdict !== "") {
    return `${step.name} · ${detail.verdict}`;
  }
  return step.name;
}

/** "openai/gpt-4o-mini" -> "gpt-4o-mini". The vendor prefix is noise here. */
export function shortModel(model: string): string {
  const slash = model.lastIndexOf("/");
  return slash === -1 ? model : model.slice(slash + 1);
}

/**
 * The flex-basis for a segment. Flex grow is the duration itself so the
 * browser does the proportional maths, and min-width keeps a sub-millisecond
 * step from disappearing entirely.
 */
export function segmentStyle(segment: StripSegment): {
  flexGrow: number;
  flexBasis: number;
  minWidth: number;
} {
  return {
    flexGrow: Math.max(segment.durationMs, 0.001),
    flexBasis: 0,
    minWidth: segment.showLabel ? SEGMENT_LABELLED_MIN_PX : SEGMENT_MIN_PX,
  };
}
