import type { ReactNode } from "react";

import { cn } from "@/lib/cn";

export interface KeyValueRow {
  label: string;
  value: ReactNode;
}

interface KeyValueProps {
  rows: KeyValueRow[];
  className?: string;
}

/**
 * The label/value grid used throughout the trace detail.
 *
 * Rows with an empty value are dropped rather than rendered blank: a detail
 * the server did not send is absent, not empty.
 */
export function KeyValue({ rows, className }: KeyValueProps) {
  const present = rows.filter((row) => row.value !== null && row.value !== undefined && row.value !== "");
  if (present.length === 0) return null;

  return (
    <dl className={cn("grid grid-cols-[118px_1fr] items-baseline gap-x-4 gap-y-1.5", className)}>
      {present.map((row) => (
        <div key={row.label} className="contents">
          <dt className="label text-text-5">{row.label}</dt>
          <dd className="m-0 data text-[12px] text-text-2">{row.value}</dd>
        </div>
      ))}
    </dl>
  );
}
