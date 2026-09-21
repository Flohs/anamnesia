import { cn } from "@/lib/cn";
import { TONE_DOT, type Tone } from "@/lib/tone";
import { formatScore } from "@/lib/format";

interface ScoreBarProps {
  /** 0..1. Values outside the range are clamped rather than overflowing. */
  score: number;
  tone?: Tone;
  /** Prints the numeric score next to the bar. */
  showValue?: boolean;
  className?: string;
}

/** A similarity or relevance score, as a bar plus its number. */
export function ScoreBar({ score, tone = "info", showValue = true, className }: ScoreBarProps) {
  const clamped = Math.min(Math.max(score, 0), 1);

  return (
    <span className={cn("flex items-center gap-2.5", className)}>
      <span className="h-1 w-[74px] shrink-0 overflow-hidden rounded-full bg-surface-3">
        <span
          className={cn("block h-full rounded-full", TONE_DOT[tone])}
          style={{ width: `${clamped * 100}%` }}
        />
      </span>
      {showValue ? <span className="data text-text-3">{formatScore(clamped)}</span> : null}
    </span>
  );
}
