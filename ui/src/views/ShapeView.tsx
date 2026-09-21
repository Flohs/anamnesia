import { Figure } from "@/components/ui/Figure";
import { PageHeader } from "@/components/ui/PageHeader";
import { QueryBoundary } from "@/components/ui/QueryBoundary";
import { ActivityHeatmap } from "@/components/figures/ActivityHeatmap";
import { ConsolidationTree } from "@/components/figures/ConsolidationTree";
import { DecayCurve } from "@/components/figures/DecayCurve";
import { EmbeddingMap } from "@/components/figures/EmbeddingMap";
import { useActivityBuckets, useConfig, useEmbeddingMap, useExperiences } from "@/api/queries";
import { halfLifeDays } from "@/lib/config";

export function ShapeView() {
  const experiences = useExperiences();
  const buckets = useActivityBuckets(28);
  const embedding = useEmbeddingMap();
  const config = useConfig();

  const caseHalfLife = halfLifeDays(config.data, "case", 21);
  const strategyHalfLife = halfLifeDays(config.data, "strategy", 365);

  return (
    <>
      <PageHeader accent="memory" lede="Four views of the same store: how it folds, when it grew, what it is forgetting, and how it clusters.">
        The shape of the
      </PageHeader>

      <div className="grid gap-4 [grid-template-columns:repeat(auto-fit,minmax(430px,1fr))]">
        <Figure
          number="FIG. 01"
          title="Consolidation folds detail upward"
          meta="by abstraction"
          caption="raw cases feed insights, insights feed strategies"
        >
          <QueryBoundary query={experiences} skeletonHeight={250}>
            {(page) => <ConsolidationTree experiences={page.items} />}
          </QueryBoundary>
        </Figure>

        <Figure
          number="FIG. 02"
          title="Where memory accumulated"
          meta="4 weeks · per project"
          caption="one cell is one day · darker is quieter"
        >
          <QueryBoundary query={buckets} skeletonHeight={140}>
            {(data) => <ActivityHeatmap buckets={data.buckets} />}
          </QueryBoundary>
        </Figure>

        <Figure
          number="FIG. 03"
          title="What it is forgetting, and how fast"
          meta="relevance vs age"
          caption="curves come from the running configuration, dots are real rows"
        >
          <QueryBoundary query={experiences} skeletonHeight={232}>
            {(page) => (
              <DecayCurve
                experiences={page.items}
                caseHalfLifeDays={caseHalfLife}
                strategyHalfLifeDays={strategyHalfLife}
              />
            )}
          </QueryBoundary>
        </Figure>

        <Figure
          number="FIG. 04"
          title="Memory space, flattened"
          meta="PCA · 2048 to 2"
          caption="a two-component projection hides most of the structure, so read clusters as a hint"
        >
          <QueryBoundary query={embedding} skeletonHeight={232}>
            {(data) => <EmbeddingMap data={data} />}
          </QueryBoundary>
        </Figure>
      </div>
    </>
  );
}
