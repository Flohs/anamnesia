import type { ActivityEvent, ActivitySnapshot, ConnectionState, TraceSummary } from "@/api/types";

/**
 * Folds activity events into the state the Now screen renders.
 *
 * Kept pure and separate from the subscription so the interesting behaviour
 * (a running trace being replaced when it finishes, the ring staying bounded,
 * a dropped connection downgrading to polling) is testable without a server.
 */

export interface ActivityState {
  snapshot: ActivitySnapshot | null;
  connection: ConnectionState;
  /** Set when the stream failed, so the UI can say what happened. */
  error: string | null;
}

export type ActivityAction =
  | { type: "connecting" }
  | { type: "open" }
  | { type: "stream-error"; message: string }
  | { type: "polled"; snapshot: ActivitySnapshot }
  | { type: "event"; event: ActivityEvent };

export const initialActivityState: ActivityState = {
  snapshot: null,
  connection: "connecting",
  error: null,
};

export function activityReducer(state: ActivityState, action: ActivityAction): ActivityState {
  switch (action.type) {
    case "connecting":
      return { ...state, connection: "connecting" };

    case "open":
      return { ...state, connection: "streaming", error: null };

    case "stream-error":
      // Data already on screen stays: a dropped stream makes it stale, not
      // wrong, and blanking the view loses the last thing the user saw.
      return {
        ...state,
        connection: state.snapshot ? "polling" : "offline",
        error: action.message,
      };

    case "polled":
      return { ...state, snapshot: action.snapshot };

    case "event":
      return { ...state, snapshot: applyEvent(state.snapshot, action.event) };
  }
}

function applyEvent(
  snapshot: ActivitySnapshot | null,
  event: ActivityEvent,
): ActivitySnapshot | null {
  if (event.type === "snapshot") return event.data;

  // Every other event patches an existing snapshot. Without one there is
  // nothing to patch, and inventing a partial would show half a screen.
  if (!snapshot) return null;

  switch (event.type) {
    case "trace":
      return { ...snapshot, traces: mergeTrace(snapshot.traces, event.data, snapshot.recorder.capacity) };

    case "step":
      return { ...snapshot, traces: bumpStepCount(snapshot.traces, event.data.trace_id) };

    case "loops":
      return { ...snapshot, loops: event.data };

    case "queues":
      return { ...snapshot, queues: event.data };
  }
}

/**
 * Prepends a trace, or replaces the one already there.
 *
 * A trace is announced when it begins and again when it ends, so the second
 * announcement must update the first rather than appearing as a duplicate.
 */
export function mergeTrace(
  traces: readonly TraceSummary[],
  incoming: TraceSummary,
  capacity: number,
): TraceSummary[] {
  const existing = traces.findIndex((trace) => trace.id === incoming.id);

  if (existing !== -1) {
    const next = [...traces];
    next[existing] = incoming;
    return next;
  }

  // The server's ring is the source of truth for how much history exists, so
  // the client holds the same amount and no more.
  return [incoming, ...traces].slice(0, Math.max(capacity, 1));
}

/**
 * A step arriving for a running trace means its detail grew. The feed only
 * shows the count, so that is all that needs updating here; the trace view
 * refetches the full trace when it is opened.
 */
function bumpStepCount(traces: readonly TraceSummary[], traceId: string): TraceSummary[] {
  return traces.map((trace) =>
    trace.id === traceId ? { ...trace, step_count: trace.step_count + 1 } : trace,
  );
}
