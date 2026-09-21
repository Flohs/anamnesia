import { useCallback, useRef, useState } from "react";
import type { PointerEvent as ReactPointerEvent, RefObject } from "react";

/** One labelled number in the readout. Labels align, values sit to the right. */
export interface TooltipRow {
  label: string;
  value: string;
}

export interface TooltipContent {
  title: string;
  rows: TooltipRow[];
  /** Optional handle so a figure can tell which of its shapes is hovered. */
  id?: string;
}

export interface TooltipState extends TooltipContent {
  /** Pointer position within the figure's wrapper, not the viewport. */
  x: number;
  y: number;
  containerWidth: number;
}

interface HoverProps {
  onPointerEnter: (event: ReactPointerEvent<SVGElement>) => void;
  onPointerMove: (event: ReactPointerEvent<SVGElement>) => void;
  onPointerLeave: () => void;
}

export interface FigureTooltipController {
  containerRef: RefObject<HTMLDivElement | null>;
  state: TooltipState | null;
  /** Spread onto any shape that should read out on hover. */
  hoverProps: (content: TooltipContent) => HoverProps;
}

/**
 * Hover readouts for the figures.
 *
 * The native `<title>` a shape can carry waits about a second and renders as
 * operating-system chrome, which is unreadable next to the rest of the console
 * and far too slow to sweep a row of cells with. This tracks the pointer
 * instead and hands the position to a tooltip drawn in the console's own type.
 */
export function useFigureTooltip(): FigureTooltipController {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const [state, setState] = useState<TooltipState | null>(null);

  const show = useCallback((content: TooltipContent, event: ReactPointerEvent<SVGElement>) => {
    const container = containerRef.current;
    if (!container) return;

    // Measured per move rather than cached: the figures sit in a fluid grid,
    // and a stale rect puts the tooltip somewhere the pointer is not.
    const bounds = container.getBoundingClientRect();
    setState({
      ...content,
      x: event.clientX - bounds.left,
      y: event.clientY - bounds.top,
      containerWidth: bounds.width,
    });
  }, []);

  const hoverProps = useCallback(
    (content: TooltipContent): HoverProps => ({
      onPointerEnter: (event) => show(content, event),
      onPointerMove: (event) => show(content, event),
      onPointerLeave: () => setState(null),
    }),
    [show],
  );

  return { containerRef, state, hoverProps };
}
