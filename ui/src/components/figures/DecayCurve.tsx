import { formatScore } from "@/lib/format";
import { experienceTitle } from "@/lib/experience";
import { FigureTooltip } from "@/components/ui/FigureTooltip";
import { useFigureTooltip } from "@/components/ui/useFigureTooltip";
import type { Experience } from "@/api/types";

interface DecayCurveProps {
  experiences: readonly Experience[];
  /** Half-lives in days, read from the running config rather than guessed. */
  caseHalfLifeDays?: number;
  strategyHalfLifeDays?: number;
  windowDays?: number;
}

const WIDTH = 520;
const HEIGHT = 232;
const PAD = { left: 38, right: 14, top: 14, bottom: 28 };

/**
 * What the decay worker is doing to relevance, and how fast.
 *
 * The curves come from the configured half-lives and the dots are real rows,
 * so a row sitting well above its curve is visible as exactly what it is:
 * something recently used, holding its place against the decay.
 */
export function DecayCurve({
  experiences,
  caseHalfLifeDays = 21,
  strategyHalfLifeDays = 365,
  windowDays = 90,
}: DecayCurveProps) {
  const tooltip = useFigureTooltip();
  const x = (days: number) => PAD.left + (days / windowDays) * (WIDTH - PAD.left - PAD.right);
  const y = (value: number) => PAD.top + (1 - value) * (HEIGHT - PAD.top - PAD.bottom);

  const now = Date.now();
  const points = experiences.map((experience) => {
    const ageDays = (now - new Date(experience.occurred_at).getTime()) / 86_400_000;
    return {
      id: experience.id,
      title: experienceTitle(experience),
      ageDays: Math.min(Math.max(ageDays, 0), windowDays),
      relevance: experience.relevance,
      strategy: experience.kind === "strategy",
      kind: experience.kind,
      // The plotted age is clamped to the window; the readout states the real
      // one, so a dot parked on the right edge does not misreport itself.
      trueAgeDays: Math.max(Math.round(ageDays), 0),
    };
  });

  return (
    <div ref={tooltip.containerRef} className="relative">
    <svg
      viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
      width="100%"
      height={HEIGHT}
      role="img"
      aria-label="Relevance against age, with the configured half-life curves"
    >
      {[0, 0.25, 0.5, 0.75, 1].map((value) => (
        <g key={value}>
          <line x1={PAD.left} y1={y(value)} x2={WIDTH - PAD.right} y2={y(value)} stroke="#1f1f27" />
          <text
            x={PAD.left - 8}
            y={y(value) + 3.5}
            textAnchor="end"
            className="fill-text-4 font-mono text-[11px]"
          >
            {formatScore(value)}
          </text>
        </g>
      ))}

      {[0, 30, 60, 90]
        .filter((day) => day <= windowDays)
        .map((day) => (
          <text
            key={day}
            x={x(day)}
            y={HEIGHT - 9}
            textAnchor="middle"
            className="fill-text-4 font-mono text-[11px]"
          >
            {day}d
          </text>
        ))}

      <path d={halfLifePath(caseHalfLifeDays, windowDays, x, y)} fill="none" stroke="#ff7a45" strokeWidth={1.6} opacity={0.85} />
      <path
        d={halfLifePath(strategyHalfLifeDays, windowDays, x, y)}
        fill="none"
        stroke="#8fe4b8"
        strokeWidth={1.6}
        strokeDasharray="4 3"
        opacity={0.85}
      />

      {points.map((point) => {
        const hovered = tooltip.state?.id === point.id;

        return (
          <circle
            key={point.id}
            cx={x(point.ageDays)}
            cy={y(point.relevance)}
            r={hovered ? 5 : 3.2}
            fill={point.strategy ? "#8fe4b8" : "#ff7a45"}
            opacity={hovered ? 1 : 0.9}
            stroke={hovered ? "#d4d4dc" : "none"}
            {...tooltip.hoverProps({
              title: point.title,
              id: point.id,
              rows: [
                { label: "kind", value: point.kind },
                { label: "age", value: `${point.trueAgeDays}d` },
                { label: "relevance", value: formatScore(point.relevance) },
              ],
            })}
          />
        );
      })}

      {/* Placed in the dead zone the two curves leave: past the case curve's
          collapse and well below the strategy line. Sitting it at the top put
          it on the dashed line and hid a section of it. */}
      <g transform={`translate(${x(windowDays * 0.5)}, ${y(0.55)})`}>
        <rect x={-11} y={-14} width={190} height={40} rx={6} fill="rgba(19,19,25,.94)" stroke="#26262f" />
        <line x1={0} y1={-3} x2={18} y2={-3} stroke="#ff7a45" strokeWidth={1.6} />
        <text x={26} y={0} className="fill-text-3 font-mono text-[11px]">
          case · {caseHalfLifeDays}d half-life
        </text>
        <line x1={0} y1={14} x2={18} y2={14} stroke="#8fe4b8" strokeWidth={1.6} strokeDasharray="4 3" />
        <text x={26} y={17} className="fill-text-3 font-mono text-[11px]">
          strategy · holds
        </text>
      </g>
    </svg>

      <FigureTooltip state={tooltip.state} />
    </div>
  );
}

function halfLifePath(
  halfLifeDays: number,
  windowDays: number,
  x: (days: number) => number,
  y: (value: number) => number,
): string {
  const steps = [];
  for (let day = 0; day <= windowDays; day += 2) {
    steps.push(`${day === 0 ? "M" : "L"}${x(day)} ${y(Math.pow(0.5, day / halfLifeDays))}`);
  }
  return steps.join(" ");
}
