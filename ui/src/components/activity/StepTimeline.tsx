import { StepDetail } from "@/components/activity/stepDetails";
import { cn } from "@/lib/cn";
import { formatDuration } from "@/lib/format";
import { stepTone } from "@/lib/trace";
import { TONE_BG, TONE_BORDER, TONE_TEXT } from "@/lib/tone";
import type { TraceStep } from "@/api/types";

/**
 * One trace, step by step.
 *
 * Steps are numbered because a pipeline is genuinely a sequence: the order is
 * information the reader needs, unlike the section numbering on the marketing
 * site, which the console deliberately drops.
 */
export function StepTimeline({ steps }: { steps: readonly TraceStep[] }) {
  return (
    <ol className="m-0 list-none py-1.5 pl-0">
      {steps.map((step, index) => (
        <StepRow key={`${index}-${step.name}`} step={step} index={index} last={index === steps.length - 1} />
      ))}
    </ol>
  );
}

function StepRow({ step, index, last }: { step: TraceStep; index: number; last: boolean }) {
  const tone = step.err ? "bad" : stepTone(step.name);

  return (
    <li className="grid grid-cols-[34px_1fr] gap-4 px-4">
      <div className="flex flex-col items-center">
        <span
          className={cn(
            "mt-3.5 grid size-6 shrink-0 place-items-center rounded-full border data text-[10px]",
            TONE_BG[tone],
            TONE_BORDER[tone],
            TONE_TEXT[tone],
          )}
        >
          {index + 1}
        </span>
        {!last ? <span className="my-1.5 w-px flex-1 bg-border-2" aria-hidden /> : null}
      </div>

      <div className="min-w-0 pb-5 pt-3">
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <span className="label-lg text-text-4">{step.name}</span>
          <span className="text-[15px] font-medium -tracking-[0.15px] text-text">{step.summary}</span>
          <span className="ml-auto data text-text-5">{formatDuration(step.duration_ms)}</span>
        </div>

        {step.err ? (
          <p className="mt-2 mb-0 whitespace-pre-wrap font-mono text-[12px] text-rose">{step.err}</p>
        ) : null}

        <div className="mt-3">
          <StepDetail name={step.name} detail={step.detail} />
        </div>
      </div>
    </li>
  );
}
