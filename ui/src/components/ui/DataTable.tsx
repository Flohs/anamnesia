import type { ReactNode } from "react";

import { cn } from "@/lib/cn";

export interface Column<T> {
  /** Stable identity for the column, also used as the React key. */
  id: string;
  header: string;
  render: (row: T) => ReactNode;
  /** Monospace, tabular cell: ids, counts, durations. */
  numeric?: boolean;
  align?: "left" | "right";
  className?: string;
}

interface DataTableProps<T> {
  rows: readonly T[];
  columns: ReadonlyArray<Column<T>>;
  getRowKey: (row: T) => string;
  /** Shown in place of the table when there are no rows. */
  empty?: ReactNode;
  caption?: string;
  className?: string;
}

/**
 * One table for every list in the console.
 *
 * Columns are data rather than markup, so the experiences table, the projects
 * table and the hook-runs table share a single implementation and cannot
 * drift apart in padding, hover treatment or header casing.
 */
export function DataTable<T>({
  rows,
  columns,
  getRowKey,
  empty,
  caption,
  className,
}: DataTableProps<T>) {
  if (rows.length === 0 && empty) return <>{empty}</>;

  return (
    // Wide tables scroll inside their own container so the page body never
    // scrolls sideways.
    <div className={cn("overflow-x-auto", className)}>
      <table className="w-full border-collapse text-[13.5px]">
        {caption ? <caption className="sr-only">{caption}</caption> : null}
        <thead>
          <tr>
            {columns.map((column) => (
              <th
                key={column.id}
                scope="col"
                className={cn(
                  "whitespace-nowrap border-b border-border px-4 py-2.5 label font-normal text-text-5",
                  column.align === "right" ? "text-right" : "text-left",
                )}
              >
                {column.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={getRowKey(row)} className="group">
              {columns.map((column) => (
                <td
                  key={column.id}
                  className={cn(
                    "border-b border-border px-4 py-3 align-middle text-text-2",
                    "group-last:border-b-0 group-hover:bg-surface-2",
                    column.numeric && "data text-text-3",
                    column.align === "right" && "text-right",
                    column.className,
                  )}
                >
                  {column.render(row)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
