import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import type { ReactNode } from "react";

import { TransportProvider } from "@/api/TransportProvider";
import { useArtifacts } from "@/api/queries";
import { createStubTransport } from "@/test/render";
import type { Transport } from "@/api/transport";

function wrap(transport: Transport) {
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

describe("useArtifacts", () => {
  /**
   * Artifacts answer with `{scope, artifacts}` rather than the `{items,
   * next_cursor}` every browse endpoint uses, so the hook cannot reuse the
   * paging helper and has to unwrap the envelope itself.
   */
  it("unwraps the artifacts envelope into a plain list", async () => {
    const transport = createStubTransport({
      responses: {
        "/v1/artifacts": {
          scope: { user: "default", project: null },
          artifacts: [
            { id: "a", title: "Memory Constellation", created_at: "2026-08-24T00:00:00Z" },
            { id: "b", description: "A zone walkthrough.", created_at: "2026-08-23T00:00:00Z" },
          ],
        },
      },
    });

    const { result } = renderHook(() => useArtifacts(), { wrapper: wrap(transport) });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.map((a) => a.id)).toEqual(["a", "b"]);
  });

  it("yields an empty list rather than undefined when nothing is stored", async () => {
    const transport = createStubTransport({
      responses: { "/v1/artifacts": { scope: { user: "default", project: null } } },
    });

    const { result } = renderHook(() => useArtifacts(), { wrapper: wrap(transport) });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toEqual([]);
  });
});
