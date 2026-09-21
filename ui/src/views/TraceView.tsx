import { Chip } from "@/components/ui/Chip";
import { EmptyState } from "@/components/ui/EmptyState";
import { PageHeader } from "@/components/ui/PageHeader";
import { Panel, PanelHeader } from "@/components/ui/Panel";
import { QueryBoundary } from "@/components/ui/QueryBoundary";
import { StepTimeline } from "@/components/activity/StepTimeline";
import { useTrace } from "@/api/queries";
import { formatDuration, joinMeta } from "@/lib/format";
import { cn } from "@/lib/cn";
import { TONE_TEXT, TRACE_KIND_TONE, traceStatusTone } from "@/lib/tone";

interface TraceViewProps {
  traceId: string | null;
  onBack: () => void;
}

export function TraceView({ traceId, onBack }: TraceViewProps) {
  const trace = useTrace(traceId);

  return (
    <>
      <PageHeader accent="through" lede="Every step the machine took, with the material it actually worked on.">
        One checkpoint, thought
      </PageHeader>

      {traceId === null ? (
        <Panel>
          <EmptyState title="No trace selected">
            Choose a trace from the Now screen to see the gate verdict, the memories handed to the
            model, the operations it returned and the rows that were written.
          </EmptyState>
        </Panel>
      ) : (
        <QueryBoundary query={trace} skeletonHeight={420}>
          {(data) => (
            <Panel>
              <PanelHeader
                title={<span className={cn(TONE_TEXT[TRACE_KIND_TONE[data.kind]])}>{data.kind}</span>}
                meta={formatDuration(data.duration_ms)}
              >
                <span className="data normal-case text-text-5">
                  {joinMeta(data.id.slice(0, 8), data.project, data.user)}
                </span>
                <span className="ml-auto flex items-center gap-3">
                  <Chip tone={traceStatusTone(data.status)} withDot>
                    {data.status}
                  </Chip>
                </span>
              </PanelHeader>

              <StepTimeline steps={data.steps} />
            </Panel>
          )}
        </QueryBoundary>
      )}

      <button
        type="button"
        onClick={onBack}
        className="mt-5 cursor-pointer rounded-lg border border-border-2 bg-surface px-3.5 py-2 text-[13.5px] text-text-3 hover:bg-surface-2 hover:text-text-2"
      >
        Back to Now
      </button>
    </>
  );
}
