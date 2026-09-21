import type { ReactNode } from "react";

import { cn } from "@/lib/cn";

interface EmptyStateProps {
  title: string;
  children: ReactNode;
  className?: string;
}

/**
 * An empty list is an invitation, not a void.
 *
 * Every empty state says what would fill it and what has to happen first,
 * because this console is routinely pointed at an install with no facts and a
 * handful of experiences, and a blank table teaches nothing.
 */
export function EmptyState({ title, children, className }: EmptyStateProps) {
  return (
    <div className={cn("px-6 py-9 text-center", className)}>
      <h3 className="mb-1 text-[15px] font-semibold -tracking-[0.2px] text-text-2">{title}</h3>
      <p className="mx-auto max-w-[52ch] text-[13.5px] text-text-4">{children}</p>
    </div>
  );
}
