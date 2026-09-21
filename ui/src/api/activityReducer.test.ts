import { describe, expect, it } from "vitest";

import { activityReducer, initialActivityState, mergeTrace } from "@/api/activityReducer";
import type { ActivitySnapshot, TraceSummary } from "@/api/types";

function trace(id: string, overrides: Partial<TraceSummary> = {}): TraceSummary {
  return {
    id,
    seq: 1,
    kind: "ingest",
    status: "running",
    user: "default",
    project: "anamnesia-ui",
    started_at: "2026-08-18T12:00:00Z",
    ended_at: null,
    duration_ms: 0,
    summary: "running",
    step_count: 1,
    step_timings: [],
    ...overrides,
  };
}

const snapshot: ActivitySnapshot = {
  server: { started_at: "2026-08-18T11:30:00Z", uptime_ms: 1_800_000, version: "v0.1.0-rc3" },
  recorder: { capacity: 3, held: 0, dropped_events: 0 },
  loops: [],
  queues: { extract_pending: 0, embed_pending: 0 },
  traces: [],
};

describe("mergeTrace", () => {
  it("replaces a trace when it is announced again on completion", () => {
    const running = trace("a");
    const finished = trace("a", { status: "ok", summary: "wrote 1 fact", duration_ms: 1840 });

    const merged = mergeTrace([running], finished, 10);

    expect(merged).toHaveLength(1);
    expect(merged[0]?.status).toBe("ok");
  });

  it("prepends a new trace so the newest is first", () => {
    const merged = mergeTrace([trace("a")], trace("b"), 10);
    expect(merged.map((row) => row.id)).toEqual(["b", "a"]);
  });

  it("holds no more than the server's ring capacity", () => {
    const merged = mergeTrace([trace("c"), trace("b"), trace("a")], trace("d"), 3);
    expect(merged.map((row) => row.id)).toEqual(["d", "c", "b"]);
  });

  it("keeps at least one trace even if capacity is reported as zero", () => {
    expect(mergeTrace([], trace("a"), 0)).toHaveLength(1);
  });
});

describe("activityReducer", () => {
  it("starts with nothing and reports itself as connecting", () => {
    expect(initialActivityState).toEqual({ snapshot: null, connection: "connecting", error: null });
  });

  it("adopts the snapshot the stream opens with", () => {
    const state = activityReducer(initialActivityState, {
      type: "event",
      event: { type: "snapshot", data: snapshot },
    });

    expect(state.snapshot).toEqual(snapshot);
  });

  it("downgrades to polling on a stream error, keeping what is on screen", () => {
    const withData = activityReducer(initialActivityState, {
      type: "event",
      event: { type: "snapshot", data: snapshot },
    });

    const dropped = activityReducer(withData, { type: "stream-error", message: "disconnected" });

    expect(dropped.connection).toBe("polling");
    expect(dropped.snapshot).toEqual(snapshot);
    expect(dropped.error).toBe("disconnected");
  });

  it("reports offline rather than polling when it never had data", () => {
    const state = activityReducer(initialActivityState, {
      type: "stream-error",
      message: "refused",
    });

    expect(state.connection).toBe("offline");
  });

  it("clears the error once the stream reconnects", () => {
    const broken = activityReducer(initialActivityState, { type: "stream-error", message: "x" });
    expect(activityReducer(broken, { type: "open" })).toMatchObject({
      connection: "streaming",
      error: null,
    });
  });

  it("ignores patch events that arrive before any snapshot", () => {
    const state = activityReducer(initialActivityState, {
      type: "event",
      event: { type: "queues", data: { extract_pending: 4, embed_pending: 0 } },
    });

    expect(state.snapshot).toBeNull();
  });

  it("counts a step arriving for a running trace", () => {
    const withTrace = activityReducer(initialActivityState, {
      type: "event",
      event: { type: "snapshot", data: { ...snapshot, traces: [trace("a", { step_count: 2 })] } },
    });

    const stepped = activityReducer(withTrace, {
      type: "event",
      event: {
        type: "step",
        data: {
          trace_id: "a",
          step: { name: "gate", at: "2026-08-18T12:00:01Z", duration_ms: 310, summary: "", err: "", detail: {} },
        },
      },
    });

    expect(stepped.snapshot?.traces[0]?.step_count).toBe(3);
  });
});
