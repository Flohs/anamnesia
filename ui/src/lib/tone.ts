import type { LoopState, SourceState, TraceStatus } from "@/api/types";

/**
 * The semantic vocabulary every component shares.
 *
 * anamnesia.dev's secondary hues carry the meaning: mint is healthy, amber is
 * waiting, rose is broken, coral is happening now, and neutral is deliberately
 * quiet. Domain states map to a tone here and nowhere else, so a skipped
 * checkpoint reads the same in the feed, in a table and in a chart.
 */
export type Tone = "neutral" | "ok" | "warn" | "bad" | "run" | "info" | "special";

/**
 * A skipped extraction is normal operation, not a fault: the surprise gate
 * rejecting unremarkable chat is the system working. It gets `neutral` so it
 * recedes, and only genuine failures get `bad`.
 */
export function traceStatusTone(status: TraceStatus): Tone {
  switch (status) {
    case "running":
      return "run";
    case "ok":
      return "ok";
    case "skipped":
      return "neutral";
    case "failed":
      return "bad";
  }
}

/** A loop is running, broken, working, or idle with nothing to do. */
export function loopTone(loop: Pick<LoopState, "running" | "last_error" | "last_result">): Tone {
  if (loop.last_error) return "bad";
  if (loop.running) return "run";
  if (isIdleResult(loop.last_result)) return "neutral";
  return "ok";
}

/**
 * "nothing to do" is the honest answer most of the time. Treating it as a
 * quiet state rather than a success keeps the worker lane from looking busy
 * when the machine is asleep.
 */
export function isIdleResult(result: string): boolean {
  return result.trim().toLowerCase() === "nothing to do";
}

export function sourceStateTone(state: SourceState): Tone {
  switch (state) {
    case "done":
      return "ok";
    case "pending":
      return "warn";
    case "failed":
      return "bad";
    case "skipped":
      return "neutral";
  }
}

/** Each trace kind owns a hue so the feed is scannable without reading. */
export const TRACE_KIND_TONE = {
  ingest: "run",
  retrieve: "info",
  consolidate: "special",
  "session-start": "neutral",
} as const satisfies Record<string, Tone>;

/**
 * Text colour per tone. Kept as whole class strings rather than interpolated
 * fragments, because Tailwind only emits classes it can see in the source.
 */
export const TONE_TEXT: Record<Tone, string> = {
  neutral: "text-text-4",
  ok: "text-mint",
  warn: "text-amber",
  bad: "text-rose",
  run: "text-coral",
  info: "text-sky",
  special: "text-lilac",
};

export const TONE_BG: Record<Tone, string> = {
  neutral: "bg-surface-2",
  ok: "bg-mint/8",
  warn: "bg-amber/10",
  bad: "bg-rose/10",
  run: "bg-coral/12",
  info: "bg-sky/10",
  special: "bg-lilac/10",
};

export const TONE_BORDER: Record<Tone, string> = {
  neutral: "border-border-2",
  ok: "border-mint/28",
  warn: "border-amber/28",
  bad: "border-rose/28",
  run: "border-coral/30",
  info: "border-sky/28",
  special: "border-lilac/28",
};

export const TONE_DOT: Record<Tone, string> = {
  neutral: "bg-text-5",
  ok: "bg-mint",
  warn: "bg-amber",
  bad: "bg-rose",
  run: "bg-coral",
  info: "bg-sky",
  special: "bg-lilac",
};

/** Raw hex per tone, for SVG fills where a class cannot reach. */
export const TONE_HEX: Record<Tone, string> = {
  neutral: "#65656f",
  ok: "#8fe4b8",
  warn: "#f5c969",
  bad: "#f08b9c",
  run: "#ff7a45",
  info: "#7cc7f0",
  special: "#c4a3f5",
};
