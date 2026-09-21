import { useMemo, useState } from "react";

import { ArtifactList } from "@/components/constellation/ArtifactList";
import { ArtifactRing } from "@/components/constellation/ArtifactRing";
import { PageHeader } from "@/components/ui/PageHeader";
import { Panel, PanelHeader } from "@/components/ui/Panel";
import { QueryBoundary } from "@/components/ui/QueryBoundary";
import { UnitOverview } from "@/components/constellation/UnitOverview";
import { projectHues } from "@/components/constellation/hues";
import { useArtifacts, useEmbeddingMap, useStats } from "@/api/queries";
import type { CloudPoint } from "@/components/constellation/MemoryDie";

/**
 * The whole memory in one view: what it has made, around what it is made of.
 *
 * The ring is circular, so it can only use as much width as it has height.
 * The rail beside it takes the width that would otherwise sit empty and
 * spends it on the two things a tile cannot carry: what each artifact is
 * called, and what the store is made of.
 */
export function ConstellationView() {
  const artifacts = useArtifacts();
  const stats = useStats();
  const experiences = useEmbeddingMap("experiences");
  const facts = useEmbeddingMap("facts");

  const [lit, setLit] = useState<number | null>(null);
  const [matches, setMatches] = useState<number[] | undefined>(undefined);
  const [isolate, setIsolate] = useState<string | null>(null);

  const cloud = useMemo<CloudPoint[]>(
    () => [
      ...(experiences.data?.points ?? []).map((p) => ({ x: p.x, y: p.y, domain: "experiences" })),
      ...(facts.data?.points ?? []).map((p) => ({ x: p.x, y: p.y, domain: "facts" })),
    ],
    [experiences.data, facts.data],
  );

  const items = useMemo(() => artifacts.data ?? [], [artifacts.data]);
  const hueOf = useMemo(() => projectHues(items.map((artifact) => artifact.project)), [items]);

  return (
    <>
      <PageHeader
        accent="in orbit"
        lede="Every artifact this memory has produced, ringed around the store it came from. Hover a name to find it in the ring, or a tile to find it in the list."
      >
        Everything it made,
      </PageHeader>

      {/* One height for both columns. The ring is bound by the shorter side,
          so this is what sets its diameter: below about 490px the 26 tiles
          start to touch. Fixing it here rather than on the ring alone is what
          keeps the list scrolling inside its own panel instead of growing the
          page until the ring has scrolled out of sight. */}
      <div
        className={
          "grid gap-4 lg:h-[clamp(490px,68vh,660px)] " +
          "lg:grid-cols-[minmax(0,1fr)_400px] xl:grid-cols-[minmax(0,1fr)_460px]"
        }
      >
        <Panel className="flex min-h-0 flex-col overflow-hidden">
          <PanelHeader title="ring" meta={`${items.length} artifacts`} />
          <div className="relative min-h-0 flex-1">
            <QueryBoundary query={artifacts} skeletonHeight={440}>
              {(loaded) => (
                <ArtifactRing
                  artifacts={loaded}
                  cloud={cloud}
                  lit={lit}
                  onHover={setLit}
                  hueOf={hueOf}
                  isolate={isolate}
                  onMatches={setMatches}
                />
              )}
            </QueryBoundary>
          </div>
        </Panel>

        <div className="flex min-h-0 flex-col gap-4">
          <Panel className="flex min-h-0 flex-1 flex-col overflow-hidden">
            <PanelHeader
              title="artifacts"
              meta={`${new Set(items.map((a) => a.project)).size} projects`}
            />
            <div className="min-h-0 flex-1 overflow-y-auto">
              <QueryBoundary query={artifacts} skeletonHeight={200}>
                {(loaded) => (
                  <ArtifactList
                    artifacts={loaded}
                    lit={lit}
                    onHover={setLit}
                    hueOf={hueOf}
                    {...(matches ? { matches } : {})}
                  />
                )}
              </QueryBoundary>
            </div>
          </Panel>

          <Panel className="overflow-hidden">
            <PanelHeader title="what it holds" meta="click to light the die" />
            <QueryBoundary query={stats} skeletonHeight={150}>
              {(data) => (
                <UnitOverview totals={data.totals} isolate={isolate} onIsolate={setIsolate} />
              )}
            </QueryBoundary>
          </Panel>
        </div>
      </div>
    </>
  );
}
