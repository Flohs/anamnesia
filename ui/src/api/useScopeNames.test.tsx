import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import type { ReactNode } from "react";

import { TransportProvider } from "@/api/TransportProvider";
import { USER_LEVEL, useScopeNames } from "@/api/useScopeNames";
import { createStubTransport } from "@/test/render";

const PROJECT_ID = "5dff0b71-48f4-4d46-89ce-639ccdf9b513";

function wrapper({ children }: { children: ReactNode }) {
  const transport = createStubTransport({
    responses: {
      "/v1/projects": {
        items: [
          {
            id: PROJECT_ID,
            slug: "zeroploy",
            user: "default",
            created_at: "2026-08-01T00:00:00Z",
            last_activity: null,
            counts: { facts: 1, experiences: 1, skills: 0, entities: 0, sources: 1 },
          },
        ],
        next_cursor: null,
      },
      "/v1/users": {
        items: [{ id: "user-1", handle: "default", created_at: "2026-08-01T00:00:00Z" }],
        next_cursor: null,
      },
    },
  });

  return (
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}
    >
      <TransportProvider transport={transport}>{children}</TransportProvider>
    </QueryClientProvider>
  );
}

async function names() {
  const { result } = renderHook(() => useScopeNames(), { wrapper });
  await waitFor(() => expect(result.current.ready).toBe(true));
  return result;
}

describe("useScopeNames", () => {
  it("names a row that belongs to a known project", async () => {
    const result = await names();

    expect(result.current.project({ user_id: "user-1", project_id: PROJECT_ID })).toBe("zeroploy");
  });

  it("calls a row with an explicit null project user-level", async () => {
    const result = await names();

    expect(result.current.project({ user_id: "user-1", project_id: null })).toBe(USER_LEVEL);
  });

  /**
   * The server omits project_id entirely on a user-level row rather than
   * sending null, so an equality check against null lets undefined through to
   * the id shortener, which then reads .slice on nothing.
   */
  it("calls a row with no project_id at all user-level", async () => {
    const result = await names();

    expect(result.current.project({ user_id: "user-1" })).toBe(USER_LEVEL);
  });

  it("shortens an id that matches no known project rather than hiding the row", async () => {
    const result = await names();

    expect(result.current.project({ user_id: "user-1", project_id: "deadbeef-0000" })).toBe(
      "deadbeef",
    );
  });
});
