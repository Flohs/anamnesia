import { Fragment } from "react";

import { Alert, Code } from "@/components/ui/Alert";
import { deriveDiagnostics } from "@/lib/diagnostics";
import type { ActivitySnapshot, ConnectionState, Health } from "@/api/types";

interface HealthAlertsProps {
  health: Health | undefined;
  snapshot: ActivitySnapshot | null;
  connection?: ConnectionState;
}

/** Renders whatever the diagnostics rules found, in severity order. */
export function HealthAlerts({ health, snapshot, connection }: HealthAlertsProps) {
  const diagnostics = deriveDiagnostics({
    health,
    snapshot,
    ...(connection ? { connection } : {}),
  });

  if (diagnostics.length === 0) return null;

  return (
    <div className="flex flex-col gap-3">
      {diagnostics.map((diagnostic) => (
        <Alert
          key={diagnostic.id}
          code={diagnostic.code}
          tone={diagnostic.tone}
          title={diagnostic.title}
        >
          <InlineCode text={diagnostic.body} />
        </Alert>
      ))}
    </div>
  );
}

/**
 * Renders `backticked` spans as code.
 *
 * The diagnostics rules stay pure strings so they can be unit-tested, and the
 * markup happens here.
 */
export function InlineCode({ text }: { text: string }) {
  return (
    <>
      {text.split(/(`[^`]+`)/g).map((part, index) =>
        part.startsWith("`") && part.endsWith("`") && part.length > 2 ? (
          <Code key={index}>{part.slice(1, -1)}</Code>
        ) : (
          <Fragment key={index}>{part}</Fragment>
        ),
      )}
    </>
  );
}
