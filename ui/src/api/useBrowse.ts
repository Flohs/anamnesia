import { useInfiniteQuery, type UseInfiniteQueryResult, type InfiniteData } from "@tanstack/react-query";

import { useTransport } from "@/api/TransportProvider";
import type { Page, QueryParams } from "@/api/types";

/**
 * Cursor pagination over any browse endpoint.
 *
 * Every memory domain is served the same way: `{items, next_cursor}`, with an
 * opaque cursor. One hook covers all of them, so a new domain is a route
 * string rather than another near-identical hook.
 */

/** Rows per request. Enough to fill a screen, small enough to stay quick. */
export const PAGE_SIZE = 50;

export interface Browse<T> {
  rows: T[];
  /** True while the next page is in flight, for the button's own state. */
  loadingMore: boolean;
  hasMore: boolean;
  loadMore: () => void;
  query: UseInfiniteQueryResult<InfiniteData<Page<T>>>;
}

export function useBrowse<T>(
  domain: string,
  params: QueryParams = {},
  enabled = true,
): Browse<T> {
  const { transport } = useTransport();

  // Params land in the key so changing the scope starts a fresh list rather
  // than appending filtered rows onto unfiltered ones.
  const query = useInfiniteQuery({
    queryKey: ["browse", domain, params],
    enabled,
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) =>
      transport.get<Page<T>>(`/v1/${domain}`, {
        ...params,
        limit: PAGE_SIZE,
        ...(pageParam ? { cursor: pageParam } : {}),
      }),
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });

  return {
    rows: query.data?.pages.flatMap((page) => page.items) ?? [],
    loadingMore: query.isFetchingNextPage,
    hasMore: query.hasNextPage,
    loadMore: () => void query.fetchNextPage(),
    query,
  };
}
