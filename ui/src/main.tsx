import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { App } from "@/App";
import { TransportProvider } from "@/api/TransportProvider";
import "@/styles/theme.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // The console is a viewer, not an editor: a stale read costs nothing and
      // a retry storm against a struggling server costs plenty.
      retry: 1,
      staleTime: 5_000,
      refetchOnWindowFocus: false,
    },
  },
});

const container = document.getElementById("root");
if (!container) throw new Error("index.html is missing #root");

createRoot(container).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <TransportProvider>
        <App />
      </TransportProvider>
    </QueryClientProvider>
  </StrictMode>,
);
