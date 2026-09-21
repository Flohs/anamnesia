import type { ReactNode } from "react";

import { cn } from "@/lib/cn";

interface EyebrowProps {
  children: ReactNode;
  /** Secondary text after the label: a count, a scope, a caveat. */
  count?: ReactNode;
  className?: string;
}

/**
 * Section label with a rule trailing off to the right, as on anamnesia.dev.
 *
 * The site numbers its sections; the console does not, because in a dashboard
 * a number implies an order that the sections do not have.
 */
export function Eyebrow({ children, count, className }: EyebrowProps) {
  return (
    <h2 className={cn("mb-3.5 flex items-center gap-3.5 label-lg text-coral", className)}>
      {children}
      {count ? <span className="data normal-case text-text-4">{count}</span> : null}
      <span
        aria-hidden
        className="h-px flex-1 bg-gradient-to-r from-border-2 to-transparent"
      />
    </h2>
  );
}
