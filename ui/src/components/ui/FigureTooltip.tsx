import type { TooltipState } from "@/components/ui/useFigureTooltip";

/** Widest the readout is allowed to get, and how far it sits off the pointer. */
const MAX_WIDTH = 240;
const OFFSET = 14;

export function FigureTooltip({ state }: { state: TooltipState | null }) {
  if (!state) return null;

  // Flipped to the other side of the pointer near the right edge: the figure
  // card clips its overflow, so a tooltip that runs past it is simply cut off.
  const flip = state.x + OFFSET + MAX_WIDTH > state.containerWidth;

  return (
    <div
      role="tooltip"
      className="pointer-events-none absolute z-10 rounded-md border border-border-2 bg-surface-3 px-3 py-2 shadow-lg"
      style={{
        left: state.x + (flip ? -OFFSET : OFFSET),
        top: state.y,
        maxWidth: MAX_WIDTH,
        transform: `translate(${flip ? "-100%" : "0"}, -50%)`,
      }}
    >
      <p className="mb-1 text-[12.5px] leading-snug font-medium text-text-2">{state.title}</p>

      <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
        {state.rows.map((row) => (
          <div key={row.label} className="contents">
            {/* Neither column may wrap: left to itself the value column
                collapses and breaks a date across two lines. */}
            <dt className="data whitespace-nowrap text-text-5">{row.label}</dt>
            <dd className="m-0 data whitespace-nowrap text-right text-text-3">{row.value}</dd>
          </div>
        ))}
      </dl>
    </div>
  );
}
