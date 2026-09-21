import { useMemo } from "react";

import { experienceTitle } from "@/lib/experience";
import { FigureTooltip } from "@/components/ui/FigureTooltip";
import { useFigureTooltip } from "@/components/ui/useFigureTooltip";
import type { Experience } from "@/api/types";

interface ConsolidationTreeProps {
  experiences: readonly Experience[];
}

const WIDTH = 520;
const HEIGHT = 250;

/** Left gutter reserved for the level labels, so no node can sit on one. */
const GUTTER = 68;

/** Abstraction level to row, and to the colour that level is drawn in. */
const LEVEL_STYLE = [
  { y: 210, radius: 5, fill: "rgba(101,101,111,.3)", stroke: "#65656f", label: "raw" },
  { y: 128, radius: 7, fill: "rgba(124,199,240,.14)", stroke: "#7cc7f0", label: "fold 1" },
  { y: 46, radius: 9, fill: "rgba(255,122,69,.16)", stroke: "#ff7a45", label: "fold 2" },
] as const;

/**
 * How consolidation folds detail upward.
 *
 * Raw cases at the bottom, each fold above them, so the claim that "memory
 * gets shorter as it gets older" is visible as shape rather than asserted in
 * prose. Rows are placed by their abstraction level and linked by parent_id.
 */
export function ConsolidationTree({ experiences }: ConsolidationTreeProps) {
  const layout = useMemo(() => buildLayout(experiences), [experiences]);
  const tooltip = useFigureTooltip();

  if (layout.nodes.length === 0) {
    return (
      <p className="py-8 text-center text-[13.5px] text-text-4">
        No experiences yet, so there is nothing to fold.
      </p>
    );
  }

  // Rows exist but none links to another: nothing has been folded, or the rows
  // they were folded into are not on this page. Drawing a single row of dots
  // would imply a tree that does not exist. The reason is not knowable from
  // here, so this says what is missing rather than why.
  if (layout.edges.length === 0) {
    return (
      <p className="py-8 text-center text-[13.5px] text-text-4">
        Nothing to draw as a fold: every row here is raw. The worker clusters similar experiences
        and distils each cluster into one higher-level record, on the interval set by
        worker.consolidate_every.
      </p>
    );
  }

  return (
    <div ref={tooltip.containerRef} className="relative">
    <svg
      viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
      width="100%"
      height={HEIGHT}
      role="img"
      aria-label="Experiences arranged by abstraction level, linked to what they were folded into"
    >
      {LEVEL_STYLE.map((level, index) =>
        layout.levels.has(index) ? (
          <text
            key={level.label}
            x={12}
            y={level.y + 4}
            className="fill-text-4 font-mono text-[11px]"
          >
            {level.label}
          </text>
        ) : null,
      )}

      {layout.edges.map((edge) => (
        <path
          key={edge.key}
          d={`M${edge.x1} ${edge.y1} C ${edge.x1} ${edge.y1 - 38}, ${edge.x2} ${edge.y2 + 38}, ${edge.x2} ${edge.y2}`}
          fill="none"
          stroke="#2e2e39"
          strokeWidth={1}
        />
      ))}

      {layout.nodes.map((node) => {
        const style = LEVEL_STYLE[Math.min(node.level, LEVEL_STYLE.length - 1)] ?? LEVEL_STYLE[0];
        const hovered = tooltip.state?.id === node.id;

        return (
          <circle
            key={node.id}
            cx={node.x}
            cy={style.y}
            r={hovered ? style.radius + 2 : style.radius}
            fill={style.fill}
            stroke={hovered ? "#d4d4dc" : style.stroke}
            {...tooltip.hoverProps({
              title: node.title,
              id: node.id,
              rows: [
                { label: "kind", value: node.kind },
                { label: "level", value: style.label },
                ...(node.folded > 0
                  ? [{ label: "folded", value: `${node.folded} rows` }]
                  : []),
              ],
            })}
          />
        );
      })}
    </svg>

      <FigureTooltip state={tooltip.state} />
    </div>
  );
}

interface LayoutNode {
  id: string;
  title: string;
  level: number;
  x: number;
  kind: string;
  /** How many rows this one was distilled from; 0 for a raw row. */
  folded: number;
}

/**
 * The ids a summary was distilled from. Absent on rows written before the
 * server recorded the whole cluster, which is what parent_id then stands in
 * for.
 */
function consolidatedFrom(experience: Experience): string[] | null {
  const value = experience.meta?.consolidated_from;
  if (!Array.isArray(value)) return null;

  const ids = value.filter((id): id is string => typeof id === "string");
  return ids.length > 0 ? ids : null;
}

function buildLayout(experiences: readonly Experience[]) {
  const byLevel = new Map<number, Experience[]>();
  for (const experience of experiences) {
    const level = Math.min(experience.abstraction, LEVEL_STYLE.length - 1);
    byLevel.set(level, [...(byLevel.get(level) ?? []), experience]);
  }

  const nodes: LayoutNode[] = [];
  const positionOf = new Map<string, { x: number; level: number }>();

  for (const [level, rows] of byLevel) {
    rows.forEach((experience, index) => {
      // Evenly spaced across the width; a single node on a level sits centred.
      const span = WIDTH - GUTTER - 24;
      const x =
        rows.length === 1
          ? GUTTER + span / 2
          : GUTTER + index * (span / Math.max(rows.length - 1, 1));

      nodes.push({
        id: experience.id,
        title: experienceTitle(experience),
        level,
        x,
        kind: experience.kind,
        folded: consolidatedFrom(experience)?.length ?? 0,
      });
      positionOf.set(experience.id, { x, level });
    });
  }

  // Edges are read from the summary down, because that is the row the server
  // writes them on: the summary sits at the higher abstraction and lists every
  // row it folded in meta.consolidated_from. Its parent_id names only one of
  // those members, so it serves as the fallback for rows written before that
  // field existed, not as the link itself.
  const edges = experiences.flatMap((summary) => {
    const parent = positionOf.get(summary.id);
    if (!parent) return [];

    const sources = consolidatedFrom(summary) ?? (summary.parent_id ? [summary.parent_id] : []);

    return sources.flatMap((sourceId) => {
      const child = positionOf.get(sourceId);
      // A member outside the fetched page drops its own edge, not the summary.
      if (!child) return [];

      // A fold always moves up a level. A link to a row at the same or a
      // higher level is a supersede link, not a consolidation, and drawing it
      // here would render as a meaningless sideways sweep.
      if (parent.level <= child.level) return [];

      const childStyle =
        LEVEL_STYLE[Math.min(child.level, LEVEL_STYLE.length - 1)] ?? LEVEL_STYLE[0];
      const parentStyle =
        LEVEL_STYLE[Math.min(parent.level, LEVEL_STYLE.length - 1)] ?? LEVEL_STYLE[0];

      return [
        {
          key: `${summary.id}-${sourceId}`,
          x1: child.x,
          y1: childStyle.y - childStyle.radius - 2,
          x2: parent.x,
          y2: parentStyle.y + parentStyle.radius + 2,
        },
      ];
    });
  });

  return { nodes, edges, levels: byLevel };
}
