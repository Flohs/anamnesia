import { Chip } from "@/components/ui/Chip";
import { EmptyState } from "@/components/ui/EmptyState";
import { Panel, PanelHeader } from "@/components/ui/Panel";
import { TraceStrip } from "@/components/activity/TraceStrip";
import { cn } from "@/lib/cn";
import { formatRelative } from "@/lib/format";
import { TONE_TEXT, TRACE_KIND_TONE, traceStatusTone } from "@/lib/tone";
import type { TraceSummary } from "@/api/types";

interface TraceFeedProps {
  traces: readonly TraceSummary[];
  onOpen: (id: string) => void;
}

export function TraceFeed({ traces, onOpen }: TraceFeedProps) {
  return (
    <Panel>
      <PanelHeader title="trace" meta="bar segments are real step durations · select one to open it" />

      {traces.length === 0 ? (
        <EmptyState title="Nothing has happened yet">
          Traces appear as soon as a Claude Code session checkpoints, or when a worker loop finds
          something to do. They live in memory, so this list starts empty after every restart.
        </EmptyState>
      ) : (
        <ul className="m-0 list-none p-0">
          {traces.map((trace) => (
            <li key={trace.id}>
              <TraceRow trace={trace} onOpen={onOpen} />
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

/**
 * Flattens a summary onto one line.
 *
 * A retrieve summary quotes the prompt, and a prompt can be several paragraphs
 * that reach the wire with escaped newlines. Printed as-is the feed shows a
 * literal backslash-n mid-sentence.
 */
function singleLine(text: string): string {
  return text.replace(/\\[rn]/g, " ").replace(/\s+/g, " ").trim();
}

/** Status chips only appear when the status is worth calling out. */
const STATUS_LABEL: Partial<Record<TraceSummary["status"], string>> = {
  skipped: "skipped",
  failed: "failed",
  running: "running",
};

function TraceRow({ trace, onOpen }: { trace: TraceSummary; onOpen: (id: string) => void }) {
  const statusLabel = STATUS_LABEL[trace.status];

  return (
    <button
      type="button"
      onClick={() => onOpen(trace.id)}
      className="block w-full cursor-pointer border-0 border-b border-border bg-transparent px-4 py-3 text-left last:border-b-0 hover:bg-surface-2"
    >
      <div className="mb-2 flex items-center gap-2.5">
        <span
          className={cn("w-[74px] shrink-0 label", TONE_TEXT[TRACE_KIND_TONE[trace.kind]])}
        >
          {trace.kind}
        </span>

        <span className="flex-1 truncate text-[14px] text-text-2">
          {singleLine(trace.summary)}
        </span>

        {statusLabel ? <Chip tone={traceStatusTone(trace.status)}>{statusLabel}</Chip> : null}
        {/* A consolidate trace spans every scope, so it carries no project. */}
        {trace.project ? <Chip>{trace.project}</Chip> : null}

        <span className="data text-text-4">{formatRelative(trace.started_at)}</span>
      </div>

      <TraceStrip
        steps={trace.step_timings ?? []}
        status={trace.status}
        className="md:ml-[84px]"
      />
    </button>
  );
}
