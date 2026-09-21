import { createContext, use, useMemo, type ReactNode } from "react";

import { createTransport } from "@/api/client";
import type { Transport } from "@/api/transport";

interface TransportValue {
  transport: Transport;
}

const TransportContext = createContext<TransportValue | null>(null);

interface TransportProviderProps {
  children: ReactNode;
  /** Tests and stories inject their own transport rather than hitting fetch. */
  transport?: Transport;
}

export function TransportProvider({ children, transport }: TransportProviderProps) {
  const value = useMemo<TransportValue>(
    () => ({ transport: transport ?? createTransport() }),
    [transport],
  );

  return <TransportContext value={value}>{children}</TransportContext>;
}

// The provider and its hook belong together: the context object itself must
// stay private, and splitting them would export it just to satisfy a lint rule
// about fast refresh, which only costs a full reload when this file is edited.
// eslint-disable-next-line react-refresh/only-export-components
export function useTransport(): TransportValue {
  const value = use(TransportContext);
  if (!value) {
    throw new Error("useTransport must be used inside a TransportProvider");
  }
  return value;
}
