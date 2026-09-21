import { cn } from "@/lib/cn";
import { formatDuration } from "@/lib/format";
import { buildStrip, segmentStyle, type StripSegment } from "@/lib/trace";
import { TONE_BG, TONE_BORDER, TONE_TEXT } from "@/lib/tone";
import type { StepTiming, TraceStatus } from "@/api/types";

interface TraceStripProps {
  steps: readonly StepTiming[];
  status: TraceStatus;
  className?: string;
}

/**
 * The signature element: one trace drawn as a segmented bar.
 *
 * anamnesia.dev renders its retrieval pipeline as a latency budget bar. Here
 * the same form carries live data, each segment sized by the step's real
 * duration, so the answer to "where did the time go" arrives before any
 * reading. A failed trace gets a terminal marker rather than simply stopping,
 * which would read as a rendering glitch.
 */
export function TraceStrip({ steps, status, className }: TraceStripProps) {
  const { segments, totalMs, truncated } = buildStrip(steps, status);
  if (segments.length === 0) return null;

  return (
    <div className={className}>
      <div className="flex h-[22px] gap-0.5">
        {segments.map((segment) => (
          <Segment key={segment.key} segment={segment} />
        ))}

        {truncated ? (
          <span
            className={cn(
              "flex shrink-0 items-center rounded-[3px] border px-1.5 label text-[9.5px]",
              TONE_BG.bad,
              TONE_BORDER.bad,
              TONE_TEXT.bad,
            )}
          >
            stopped here
          </span>
        ) : null}
      </div>

      <div className="mt-1.5 flex justify-between data text-[9.5px] text-text-5">
        <span>0</span>
        <span>{formatDuration(totalMs)}</span>
      </div>
    </div>
  );
}

function Segment({ segment }: { segment: StripSegment }) {
  const style = segmentStyle(segment);

  return (
    <span
      className={cn(
        "flex items-center overflow-hidden whitespace-nowrap rounded-[3px] border px-1.5",
        "label text-[9.5px]",
        TONE_BG[segment.tone],
        TONE_BORDER[segment.tone],
        TONE_TEXT[segment.tone],
      )}
      style={style}
      title={`${segment.label} · ${formatDuration(segment.durationMs)}`}
    >
      {segment.showLabel ? segment.label : null}
    </span>
  );
}
