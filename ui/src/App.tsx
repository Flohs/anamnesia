import { useCallback, useState } from "react";

import { AppShell } from "@/components/layout/AppShell";
import { ConstellationView } from "@/views/ConstellationView";
import { ErrorBoundary } from "@/components/ui/ErrorBoundary";
import { MemoryView } from "@/views/MemoryView";
import { NowView } from "@/views/NowView";
import { ShapeView } from "@/views/ShapeView";
import { SignalsView } from "@/views/SignalsView";
import { TraceView } from "@/views/TraceView";
import { useActivityStream } from "@/api/useActivityStream";
import { VIEWS, type ViewId } from "@/views/registry";

/** The reader knows these by their nav label, so a failure names that. */
const VIEW_LABEL: Record<ViewId, string> = Object.fromEntries(
  VIEWS.map((entry) => [entry.id, entry.label]),
) as Record<ViewId, string>;

export function App() {
  const [view, setView] = useState<ViewId>("now");
  const [traceId, setTraceId] = useState<string | null>(null);
  const { snapshot, connection } = useActivityStream();

  const openTrace = useCallback((id: string) => {
    setTraceId(id);
    setView("trace");
  }, []);

  return (
    <AppShell view={view} onNavigate={setView} connection={connection}>
      {/* The net under the whole view, for the panels that are not figures.
          Keyed on the view so navigating away from a broken one recovers,
          which a boundary does not do by itself. The nav lives outside it and
          stays usable whatever the view does. */}
      <ErrorBoundary label={VIEW_LABEL[view]} resetKey={view}>
        {view === "now" ? <NowView snapshot={snapshot} onOpenTrace={openTrace} /> : null}
        {view === "trace" ? <TraceView traceId={traceId} onBack={() => setView("now")} /> : null}
        {view === "memory" ? <MemoryView /> : null}
        {view === "shape" ? <ShapeView /> : null}
        {view === "constellation" ? <ConstellationView /> : null}
        {view === "signals" ? <SignalsView snapshot={snapshot} connection={connection} /> : null}
      </ErrorBoundary>
    </AppShell>
  );
}
