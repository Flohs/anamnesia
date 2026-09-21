import { Chip } from "@/components/ui/Chip";
import type { ConnectionState } from "@/api/types";
import type { Tone } from "@/lib/tone";

/**
 * What the console is doing to stay current.
 *
 * "streaming" must never be shown over data that stopped moving, so a dropped
 * connection says "polling" and a dead one says "offline".
 */
const STATES: Record<ConnectionState, { label: string; tone: Tone; pulse: boolean }> = {
  connecting: { label: "connecting", tone: "warn", pulse: true },
  streaming: { label: "streaming", tone: "ok", pulse: true },
  polling: { label: "polling", tone: "warn", pulse: false },
  offline: { label: "offline", tone: "bad", pulse: false },
};

export function ConnectionBadge({ state }: { state: ConnectionState }) {
  const { label, tone } = STATES[state];
  return (
    <Chip tone={tone} withDot className="rounded-full px-2.5 py-1">
      {label}
    </Chip>
  );
}
