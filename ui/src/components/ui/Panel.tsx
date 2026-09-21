import type { ReactNode } from "react";

import { cn } from "@/lib/cn";

interface PanelProps {
  children: ReactNode;
  className?: string;
}

/** The bordered surface every block of content sits on. */
export function Panel({ children, className }: PanelProps) {
  return (
    <section className={cn("rounded-md border border-border bg-surface", className)}>
      {children}
    </section>
  );
}

interface PanelHeaderProps {
  /** Left-hand label, uppercase monospace. */
  title: ReactNode;
  /** Right-hand context: what the panel is showing, or how to read it. */
  meta?: ReactNode;
  children?: ReactNode;
  className?: string;
}

export function PanelHeader({ title, meta, children, className }: PanelHeaderProps) {
  return (
    <header
      className={cn(
        "flex items-center gap-3 border-b border-border px-4 py-3 label-lg text-text-3",
        className,
      )}
    >
      {title}
      {children}
      {meta ? <span className="ml-auto data normal-case text-text-5">{meta}</span> : null}
    </header>
  );
}
