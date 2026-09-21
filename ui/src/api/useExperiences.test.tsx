import { waitFor } from "@testing-library/react";
import { renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import type { ReactNode } from "react";

import { TransportProvider } from "@/api/TransportProvider";
import { useExperiences } from "@/api/queries";
import type { QueryParams, Transport } from "@/api/transport";
import type { Experience, Page } from "@/api/types";

function row(id: string): Experience {
  return {
    id,
    scope: { user_id: "user-1", project_id: "project-1" },
    kind: "case",
    abstraction: 0,
    body: "body",
    trust: 0.7,
    importance: 0.5,
    relevance: 0.5,
    use_count: 0,
    occurred_at: "2026-08-21T20:00:00Z",
    ingested_at: "2026-08-21T20:00:00Z",
  };
}

/**
 * Two pages, the way the server serves them: the first carries a cursor, the
 * second closes the list. Records the params of every request so the test can
 * show the cursor was actually sent back.
 */
function pagingTransport(): { transport: Transport; requests: QueryParams[] } {
  const requests: QueryParams[] = [];

  const transport: Transport = {
    name: "http",
    get: <T,>(path: string, params?: QueryParams): Promise<T> => {
      if (path !== "/v1/experiences") return Promise.reject(new Error(`unexpected path ${path}`));
      requests.push(params ?? {});

      const page: Page<Experience> = params?.cursor
        ? { items: [row("c"), row("d")], next_cursor: null }
        : { items: [row("a"), row("b")], next_cursor: "cursor-page-2" };

      return Promise.resolve(page as T);
    },
    subscribe: () => () => undefined,
  };

  return { transport, requests };
}

function wrapper(transport: Transport) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });

  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        <TransportProvider transport={transport}>{children}</TransportProvider>
      </QueryClientProvider>
    );
  };
}

describe("useExperiences", () => {
  it("follows the cursor so the figures see the whole store", async () => {
    const { transport, requests } = pagingTransport();

    const { result } = renderHook(() => useExperiences(), { wrapper: wrapper(transport) });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(result.current.data?.items.map((item) => item.id)).toEqual(["a", "b", "c", "d"]);
    expect(requests).toHaveLength(2);
    expect(requests[1]?.cursor).toBe("cursor-page-2");
  });
});
