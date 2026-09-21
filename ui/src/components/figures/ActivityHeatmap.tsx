import { useMemo } from "react";

import { truncate } from "@/lib/format";
import { FigureTooltip } from "@/components/ui/FigureTooltip";
import { useFigureTooltip } from "@/components/ui/useFigureTooltip";
import type { ActivityBucket } from "@/api/types";

interface ActivityHeatmapProps {
  buckets: readonly ActivityBucket[];
  /** Which count drives the colour. Sources is the honest measure of input. */
  metric?: "sources" | "experiences" | "facts";
  /**
   * Days to draw, whether or not each one has data. Sized so the chart fits a
   * card without being scaled: scaling shrank every label with it.
   */
  days?: number;
}

/**
 * Sized against the card rather than shrunk to fit it.
 *
 * A two-column card gives roughly 610px of content width. 28 columns at 15px
 * plus a 168px label gutter comes to 588px, which leaves the drawing at
 * natural size and lets the type be read at a normal size instead of the 9.5px
 * that only looked adequate before it was scaled down.
 */
const CELL = 12;
const GAP = 3;
const LABEL_WIDTH = 168;
const TOP = 22;

const LABEL_SIZE = 11.5;
const AXIS_SIZE = 11;
/** Characters that fit the gutter at LABEL_SIZE in the monospace face. */
const LABEL_CHARS = 22;

/** Row label for buckets the server returned with no project. */
const UNSCOPED = "(no project)";

/**
 * Where memory accumulated, one cell per project per day.
 *
 * Four steps rather than a continuous ramp: the question this answers is
 * "busy, quiet or nothing", and a smooth gradient makes that harder to read at
 * cell size, not easier.
 */
const STEPS = ["#16161d", "rgba(255,122,69,.22)", "rgba(255,122,69,.48)", "#ff7a45"] as const;

export function ActivityHeatmap({ buckets, metric = "sources", days = 28 }: ActivityHeatmapProps) {
  const { rows, dates, valueAt, bucketAt, max } = useMemo(
    () => groupBuckets(buckets, metric, days),
    [buckets, metric, days],
  );
  const tooltip = useFigureTooltip();

  if (rows.length === 0) {
    return (
      <p className="py-8 text-center text-[13.5px] text-text-4">
        Nothing has been ingested yet. A cell appears for each day a project produced a
        checkpoint.
      </p>
    );
  }

  const width = LABEL_WIDTH + dates.length * (CELL + GAP);
  const height = TOP + rows.length * (CELL + GAP) + 8;

  return (
    // The tooltip is a sibling of the scrolling box rather than a child of it,
    // so a readout near the right edge cannot add a scrollbar to the figure.
    <div ref={tooltip.containerRef} className="relative">
      <div className="overflow-x-auto">
      {/* Natural size, never scaled to the container: scaling to fit shrank
          every label along with the grid until the text was unreadable. The
          range is chosen to fit instead, and the wrapper scrolls if it does
          not. */}
      <svg
        viewBox={`0 0 ${width} ${height}`}
        width={width}
        height={height}
        style={{ minWidth: width }}
        role="img"
        aria-label={`Daily ${metric} per project over the last ${dates.length} days`}
      >
        {/* With a short window the two ends collide, so only "today" is kept. */}
        {dates.length > 10 ? (
          <text
            x={LABEL_WIDTH}
            y={12}
            className="fill-text-4 font-mono"
            style={{ fontSize: AXIS_SIZE }}
          >
            {dates.length} days ago
          </text>
        ) : null}
        <text
          x={width}
          y={12}
          textAnchor="end"
          className="fill-text-4 font-mono"
          style={{ fontSize: AXIS_SIZE }}
        >
          today
        </text>

        {rows.map((project, rowIndex) => (
          <g key={project}>
            <text
              x={0}
              y={TOP + rowIndex * (CELL + GAP) + CELL - 2}
              className="fill-text-3 font-mono"
              style={{ fontSize: LABEL_SIZE }}
            >
              {truncate(project, LABEL_CHARS)}
            </text>

            {dates.map((date, columnIndex) => {
              const value = valueAt(project, date);
              const level = bucketLevel(value, max);
              const cell = bucketAt(project, date);
              const hovered = tooltip.state?.title === project && tooltip.state.id === date;

              return (
                <rect
                  key={date}
                  x={LABEL_WIDTH + columnIndex * (CELL + GAP)}
                  y={TOP + rowIndex * (CELL + GAP)}
                  width={CELL}
                  height={CELL}
                  rx={2}
                  fill={STEPS[level]}
                  // A 12px cell gives no feedback about which one is being
                  // read, so the hovered one is outlined while it is.
                  stroke={hovered ? "#d4d4dc" : level === 0 ? "#1f1f27" : "none"}
                  {...tooltip.hoverProps({
                    title: project,
                    id: date,
                    rows: [
                      { label: "date", value: date },
                      { label: "sources", value: String(cell?.sources ?? 0) },
                      { label: "facts", value: String(cell?.facts ?? 0) },
                      { label: "experiences", value: String(cell?.experiences ?? 0) },
                    ],
                  })}
                />
              );
            })}
          </g>
        ))}
        </svg>
      </div>

      <FigureTooltip state={tooltip.state} />
    </div>
  );
}

function bucketLevel(value: number, max: number): 0 | 1 | 2 | 3 {
  if (value === 0 || max === 0) return 0;
  const share = value / max;
  if (share < 0.25) return 1;
  if (share < 0.6) return 2;
  return 3;
}

function groupBuckets(
  buckets: readonly ActivityBucket[],
  metric: "sources" | "experiences" | "facts",
  days: number,
) {
  // The whole bucket is kept, not just the metric driving the colour: the
  // readout answers "quiet in what sense", which needs all three counts.
  const byKey = new Map<string, ActivityBucket>();
  const projects = new Set<string>();
  let max = 0;

  for (const bucket of buckets) {
    const project = bucket.project ?? UNSCOPED;
    const value = bucket[metric];
    byKey.set(`${project}|${bucket.date}`, bucket);
    projects.add(project);
    if (value > max) max = value;
  }

  // The server only returns days that had activity. A calendar has to show the
  // quiet days too, or a week of silence looks the same as a week of work.
  const dates: string[] = [];
  const today = new Date();
  for (let offset = days - 1; offset >= 0; offset -= 1) {
    const day = new Date(today);
    day.setDate(day.getDate() - offset);
    dates.push(day.toISOString().slice(0, 10));
  }

  return {
    rows: [...projects],
    dates,
    max,
    valueAt: (project: string, date: string) => byKey.get(`${project}|${date}`)?.[metric] ?? 0,
    bucketAt: (project: string, date: string) => byKey.get(`${project}|${date}`) ?? null,
  };
}
