import { useMemo } from "react";

import { FigureTooltip } from "@/components/ui/FigureTooltip";
import { useFigureTooltip } from "@/components/ui/useFigureTooltip";
import type { EmbeddingMap as EmbeddingMapData } from "@/api/types";

const WIDTH = 520;
const HEIGHT = 232;
const PAD = 26;

/** One hue per cluster label, drawn from the palette's secondary set. */
const HUES = ["#ff7a45", "#7cc7f0", "#8fe4b8", "#c4a3f5", "#f5c969", "#f08b9c"] as const;

/**
 * The memory space, flattened to two dimensions.
 *
 * The explained variance is stated on the figure because a 2048-to-2
 * projection discards most of the structure, and a scatter that does not admit
 * that invites people to read clusters as fact.
 */
/**
 * Below this, a two-component projection says nothing: with a handful of
 * vectors the first component absorbs everything and the picture is an
 * artefact of the arithmetic rather than a fact about the memory.
 */
const MIN_MEANINGFUL_POINTS = 8;

export function EmbeddingMap({ data }: { data: EmbeddingMapData }) {
  const { points, groups } = useMemo(() => project(data), [data]);
  const tooltip = useFigureTooltip();

  if (points.length < MIN_MEANINGFUL_POINTS) {
    return (
      <p className="py-8 text-center text-[13.5px] text-text-4">
        {points.length === 0
          ? "Nothing is embedded yet, so there is no space to plot."
          : `Only ${points.length} ${points.length === 1 ? "memory is" : "memories are"} embedded. A two-component projection needs a few dozen before its clusters mean anything.`}
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
      aria-label={`${data.n} memories projected onto two principal components`}
    >
      <line x1={PAD} y1={HEIGHT - PAD} x2={WIDTH - 14} y2={HEIGHT - PAD} stroke="#1f1f27" />
      <line x1={PAD} y1={14} x2={PAD} y2={HEIGHT - PAD} stroke="#1f1f27" />

      <text x={WIDTH - 14} y={HEIGHT - 8} textAnchor="end" className="fill-text-4 font-mono text-[11px]">
        PC1 · {Math.round(data.explained_variance[0] * 100)}%
      </text>
      <text x={PAD + 6} y={12} className="fill-text-4 font-mono text-[11px]">
        PC2 · {Math.round(data.explained_variance[1] * 100)}%
      </text>

      {points.map((point) => {
        const hovered = tooltip.state?.id === point.id;

        return (
          <circle
            key={point.id}
            cx={point.cx}
            cy={point.cy}
            r={hovered ? 5 : 3}
            fill={point.colour}
            opacity={hovered ? 1 : 0.62}
            stroke={hovered ? "#d4d4dc" : "none"}
            {...tooltip.hoverProps({
              title: point.title,
              id: point.id,
              rows: [
                { label: "project", value: point.project },
                { label: "cluster", value: point.kind },
              ],
            })}
          />
        );
      })}

      {groups.map((group) => (
        <text
          key={group.label}
          x={group.cx}
          y={group.labelY}
          textAnchor="middle"
          fill={group.colour}
          opacity={0.9}
          className="font-mono text-[11px]"
        >
          {group.label}
        </text>
      ))}
    </svg>

      <FigureTooltip state={tooltip.state} />
    </div>
  );
}

/** Scales the server's coordinates into the viewbox and places one label per kind. */
function project(data: EmbeddingMapData) {
  const xs = data.points.map((point) => point.x);
  const ys = data.points.map((point) => point.y);
  const scaleX = makeScale(xs, PAD + 8, WIDTH - PAD);
  const scaleY = makeScale(ys, HEIGHT - PAD - 8, 22);

  const colourOf = new Map<string, string>();
  for (const point of data.points) {
    if (!colourOf.has(point.kind)) {
      colourOf.set(point.kind, HUES[colourOf.size % HUES.length] ?? HUES[0]);
    }
  }

  const points = data.points.map((point) => ({
    id: point.id,
    title: point.title,
    project: point.project,
    kind: point.kind,
    cx: scaleX(point.x),
    cy: scaleY(point.y),
    colour: colourOf.get(point.kind) ?? HUES[0],
  }));

  // A label goes above its cluster, or below when that would run off the top.
  const groups = [...colourOf.keys()].map((kind) => {
    const members = points.filter((_, index) => data.points[index]?.kind === kind);
    const cx = members.reduce((sum, member) => sum + member.cx, 0) / Math.max(members.length, 1);
    const top = Math.min(...members.map((member) => member.cy));
    const bottom = Math.max(...members.map((member) => member.cy));

    return {
      label: kind,
      colour: colourOf.get(kind) ?? HUES[0],
      cx,
      labelY: top - 12 < 20 ? bottom + 16 : top - 12,
    };
  });

  return { points, groups };
}

function makeScale(values: number[], from: number, to: number): (value: number) => number {
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;
  return (value) => from + ((value - min) / span) * (to - from);
}
