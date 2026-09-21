import type { ReactNode } from "react";
import type { UseQueryResult } from "@tanstack/react-query";

import { ApiError } from "@/api/transport";
import { Alert } from "@/components/ui/Alert";

interface QueryBoundaryProps<T> {
  query: UseQueryResult<T>;
  children: (data: T) => ReactNode;
  /** Height reserved while loading, so the layout does not jump. */
  skeletonHeight?: number;
}

/**
 * Renders a query's three states without every caller repeating the ternary.
 *
 * A failure shows the message the transport composed, which already names the
 * likely cause and the fix, rather than a status code.
 */
export function QueryBoundary<T>({ query, children, skeletonHeight = 120 }: QueryBoundaryProps<T>) {
  if (query.isPending) {
    return (
      <div
        className="animate-pulse rounded-md bg-surface-2"
        style={{ height: skeletonHeight }}
        aria-busy="true"
        aria-label="Loading"
      />
    );
  }

  if (query.isError) {
    const error = query.error;
    const status = error instanceof ApiError ? error.status : 0;

    return (
      <Alert code={status === 401 ? "AUTH" : "API"} tone="bad" title="Could not load this">
        {error instanceof Error ? error.message : "The request failed for an unknown reason."}
      </Alert>
    );
  }

  return <>{children(query.data)}</>;
}
