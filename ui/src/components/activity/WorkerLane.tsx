import { EmptyState } from "@/components/ui/EmptyState";
import { Panel } from "@/components/ui/Panel";
import { StatusDot } from "@/components/ui/StatusDot";
import { cn } from "@/lib/cn";
import { formatDuration, formatInterval, formatRelative } from "@/lib/format";
import { isIdleResult, loopTone } from "@/lib/tone";
import type { LoopState } from "@/api/types";

/**
 * The six worker loops, one row each.
 *
 * A loop that found nothing to do is dimmed rather than marked green: most of
 * the time the honest report is that there was nothing to do, and dressing
 * that as success makes an idle machine look busy.
 */
export function WorkerLane({ loops }: { loops: readonly LoopState[] }) {
  // A server started with --no-worker serves the API and nothing else. Left
  // blank, that reads as a rendering fault; said plainly, it explains why
  // memory is not growing.
  if (loops.length === 0) {
    return (
      <Panel>
        <EmptyState title="No worker loops are running">
          This server answers the API but does not extract, embed, decay or consolidate. It was
          started with <code className="font-mono text-text-3">--no-worker</code>, or the worker
          failed to start. Memory will not grow until one is running.
        </EmptyState>
      </Panel>
    );
  }

  return (
    <Panel>
      <ul className="m-0 list-none p-0">
        {loops.map((loop) => (
          <WorkerRow key={loop.name} loop={loop} />
        ))}
      </ul>
    </Panel>
  );
}

function WorkerRow({ loop }: { loop: LoopState }) {
  const tone = loopTone(loop);
  const idle = isIdleResult(loop.last_result);

  return (
    <li className="grid grid-cols-[15px_1fr_auto] items-center gap-3.5 border-b border-border px-4 py-2.5 last:border-b-0 hover:bg-surface-2 md:grid-cols-[15px_128px_92px_1fr_104px]">
      <StatusDot tone={tone} pulse={loop.running} label={loop.running ? "running" : undefined} />

      <span className="data text-[12px] text-text">{loop.name}</span>

      <span className="hidden data text-text-5 md:inline">{formatInterval(loop.interval_ms)}</span>

      <span
        className={cn(
          "hidden truncate text-[13.5px] md:block",
          loop.last_error ? "text-rose" : idle ? "text-text-4" : "text-text-3",
        )}
      >
        {loop.last_error || loop.last_result}
      </span>

      <span className="whitespace-nowrap text-right data text-text-4">
        {loop.last_start ? formatRelative(loop.last_start) : "never"} ·{" "}
        {formatDuration(loop.last_duration_ms)}
      </span>
    </li>
  );
}
