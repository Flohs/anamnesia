import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, type RenderOptions, type RenderResult } from "@testing-library/react";
import type { ReactElement, ReactNode } from "react";

import { TransportProvider } from "@/api/TransportProvider";
import type { StreamHandlers, Transport } from "@/api/transport";

/**
 * Renders a component inside the providers the app supplies, with a transport
 * that answers from a supplied table instead of the network.
 *
 * Nothing in a test should reach fetch: a component that quietly requests
 * something the test did not anticipate should fail loudly here rather than
 * hang.
 */
export interface StubOptions {
  /** Path to response body. A missing path rejects, which is the point. */
  responses?: Record<string, unknown>;
  onSubscribe?: (handlers: StreamHandlers) => void;
}

export function createStubTransport({ responses = {}, onSubscribe }: StubOptions = {}): Transport {
  return {
    name: "http",
    get: <T,>(path: string): Promise<T> =>
      path in responses
        ? Promise.resolve(responses[path] as T)
        : Promise.reject(new Error(`stub transport has no response for ${path}`)),
    subscribe: (handlers) => {
      onSubscribe?.(handlers);
      return () => undefined;
    },
  };
}

export function renderWithProviders(
  ui: ReactElement,
  { transport, ...options }: { transport?: Transport } & Omit<RenderOptions, "wrapper"> = {},
): RenderResult {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });

  function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        <TransportProvider transport={transport ?? createStubTransport()}>
          {children}
        </TransportProvider>
      </QueryClientProvider>
    );
  }

  return render(ui, { wrapper: Wrapper, ...options });
}
