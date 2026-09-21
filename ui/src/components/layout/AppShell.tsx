import type { ReactNode } from "react";

import { TopBar } from "@/components/layout/TopBar";
import type { ViewId } from "@/views/registry";
import type { ConnectionState } from "@/api/types";

interface AppShellProps {
  view: ViewId;
  onNavigate: (view: ViewId) => void;
  connection: ConnectionState;
  children: ReactNode;
}

export function AppShell({ view, onNavigate, connection, children }: AppShellProps) {
  return (
    <>
      <TopBar view={view} onNavigate={onNavigate} connection={connection} />
      <main className="mx-auto max-w-[1360px] px-6 pb-24 pt-6">{children}</main>
    </>
  );
}

/** Vertical rhythm between the bands of a view. */
export function Stack({ children }: { children: ReactNode }) {
  return <div className="flex flex-col gap-5">{children}</div>;
}
