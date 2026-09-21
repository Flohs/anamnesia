import type { ReactNode } from "react";

import { ErrorBoundary } from "@/components/ui/ErrorBoundary";
import { cn } from "@/lib/cn";

interface FigureProps {
  /** "FIG. 01". Figures are numbered because they are referred to by number. */
  number: string;
  title: string;
  meta?: string;
  caption?: string;
  children: ReactNode;
  className?: string;
}

/** The framed diagram block, borrowed from anamnesia.dev's figures. */
export function Figure({ number, title, meta, caption, children, className }: FigureProps) {
  return (
    <figure
      className={cn(
        "m-0 flex flex-col overflow-hidden rounded-md border border-border bg-surface",
        className,
      )}
    >
      <figcaption className="flex items-center gap-4 border-b border-border px-4.5 py-3">
        <span className="shrink-0 label-lg text-coral">{number}</span>
        <span className="flex-1 text-center text-[14.5px] font-medium -tracking-[0.2px] text-text-2">
          {title}
        </span>
        {meta ? <span className="shrink-0 data text-text-5">{meta}</span> : null}
      </figcaption>

      {/* Bounded here rather than around the whole view: four figures share
          the Shape page, and one that cannot draw should cost its own card
          and keep its heading, not blank the other three. */}
      <div className="flex flex-1 flex-col justify-center px-4.5 py-5">
        <ErrorBoundary label={number}>
          {children}
          {caption ? <p className="mt-3 text-center data text-text-5">{caption}</p> : null}
        </ErrorBoundary>
      </div>
    </figure>
  );
}
