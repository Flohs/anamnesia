import { useEffect, useReducer } from "react";

import { useTransport } from "@/api/TransportProvider";
import { activityReducer, initialActivityState, type ActivityState } from "@/api/activityReducer";
import type { ActivitySnapshot } from "@/api/types";

/** How often to poll once the stream has given up. */
const POLL_INTERVAL_MS = 5000;

/**
 * Subscribes to the activity stream and keeps the Now screen's state.
 *
 * When the stream drops, this falls back to polling `/v1/activity` and says so
 * through `connection`, rather than leaving a "streaming" badge above data
 * that stopped moving ten minutes ago.
 */
export function useActivityStream(): ActivityState {
  const { transport } = useTransport();
  const [state, dispatch] = useReducer(activityReducer, initialActivityState);

  useEffect(() => {
    dispatch({ type: "connecting" });

    const unsubscribe = transport.subscribe({
      onOpen: () => dispatch({ type: "open" }),
      onEvent: (event) => dispatch({ type: "event", event }),
      onError: (error) => dispatch({ type: "stream-error", message: error.message }),
    });

    return unsubscribe;
  }, [transport]);

  const polling = state.connection === "polling" || state.connection === "offline";

  useEffect(() => {
    if (!polling) return;

    let cancelled = false;
    const poll = async () => {
      try {
        const snapshot = await transport.get<ActivitySnapshot>("/v1/activity");
        if (!cancelled) dispatch({ type: "polled", snapshot });
      } catch {
        // Already reflected in `connection`; a failed poll changes nothing.
      }
    };

    void poll();
    const timer = setInterval(() => void poll(), POLL_INTERVAL_MS);

    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [polling, transport]);

  return state;
}
