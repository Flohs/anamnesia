import { Eyebrow } from "@/components/ui/Eyebrow";
import { PageHeader } from "@/components/ui/PageHeader";
import { QueryBoundary } from "@/components/ui/QueryBoundary";
import { Stack } from "@/components/layout/AppShell";
import { StatusRibbon } from "@/components/activity/StatusRibbon";
import { WorkerLane } from "@/components/activity/WorkerLane";
import { QueueTiles, type QueueTile } from "@/components/activity/QueueTiles";
import { TraceFeed } from "@/components/activity/TraceFeed";
import { HealthAlerts } from "@/views/HealthAlerts";
import { useHealth, useQueueDepths, useStats } from "@/api/queries";
import type { ActivitySnapshot, QueueDepths } from "@/api/types";

interface NowViewProps {
  snapshot: ActivitySnapshot | null;
  onOpenTrace: (id: string) => void;
}

export function NowView({ snapshot, onOpenTrace }: NowViewProps) {
  const health = useHealth();
  const stats = useStats();
  const queues = useQueueDepths();

  return (
    <>
      <PageHeader accent="away" lede="Six loops, one extractor, one retriever. This is what they have been doing, in the order they did it.">
        Everything it did while you were
      </PageHeader>

      <Stack>
        <QueryBoundary query={health} skeletonHeight={84}>
          {(data) => (
            <StatusRibbon
              health={data}
              startedAt={snapshot?.server.started_at ?? null}
              projectCount={stats.data?.totals.projects ?? 0}
              experienceCount={stats.data?.totals.experiences ?? 0}
              factCount={stats.data?.totals.facts ?? 0}
              entityCount={stats.data?.totals.entities ?? 0}
              user={stats.data?.scope.user ?? "default"}
            />
          )}
        </QueryBoundary>

        <HealthAlerts health={health.data} snapshot={snapshot} />

        <section>
          <Eyebrow count={snapshot && snapshot.loops.length > 0 ? `${snapshot.loops.length} loops` : undefined}>
            Worker loops
          </Eyebrow>
          {snapshot ? <WorkerLane loops={snapshot.loops} /> : null}
        </section>

        <section>
          <Eyebrow>Queue</Eyebrow>
          <QueueTiles
            tiles={buildTiles({
              // Polled, because the stream never sends a queues event.
              queues: queues.data ?? snapshot?.queues ?? null,
              // The client's own list, not recorder.held: that arrives with
              // the first frame and is never updated as traces come in.
              tracesHeld: snapshot?.traces.length ?? 0,
              failed: stats.data?.sources_by_state.failed ?? 0,
              skipped: stats.data?.sources_by_state.skipped ?? 0,
            })}
          />
        </section>

        <section>
          <Eyebrow count={snapshot ? `${snapshot.traces.length} held · newest first` : undefined}>
            Activity
          </Eyebrow>
          <TraceFeed traces={snapshot?.traces ?? []} onOpen={onOpenTrace} />
        </section>
      </Stack>
    </>
  );
}

interface TileInput {
  queues: QueueDepths | null;
  tracesHeld: number;
  failed: number;
  skipped: number;
}

function buildTiles({ queues, tracesHeld, failed, skipped }: TileInput): QueueTile[] {
  return [
    { value: queues?.extract_pending ?? 0, label: "awaiting extraction", toneWhenNonZero: "warn" },
    { value: queues?.embed_pending ?? 0, label: "awaiting embedding", toneWhenNonZero: "warn" },
    { value: tracesHeld, label: "traces held" },
    { value: failed, label: "sources failed", toneWhenNonZero: "bad" },
    { value: skipped, label: "skipped by the gate" },
  ];
}
