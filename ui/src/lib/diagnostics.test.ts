import { describe, expect, it } from "vitest";

import { deriveDiagnostics } from "@/lib/diagnostics";
import type { ActivitySnapshot, Health, LoopState } from "@/api/types";

const healthy: Health = {
  ok: true,
  service: "anamnesia",
  version: "v0.1.0-rc3",
  database: "ok",
  migration_version: 8,
  schema_embed_dims: 1536,
  configured_embed_dims: 1536,
  embed_provider: "openrouter",
  llm_provider: "openrouter",
  missing_ann_indexes: [],
  problems: [],
};

const ids = (health: Health, extra = {}) =>
  deriveDiagnostics({ health, snapshot: null, ...extra }).map((d) => d.id);

describe("deriveDiagnostics", () => {
  it("says nothing when the install is healthy and live", () => {
    expect(ids(healthy)).toEqual([]);
  });

  it("calls out a stub model, because nothing will ever be extracted", () => {
    expect(ids({ ...healthy, llm_provider: "stub" })).toContain("llm-stub");
  });

  it("calls out a stub embedder separately from a stub model", () => {
    expect(ids({ ...healthy, embed_provider: "stub" })).toContain("embed-stub");
  });

  it("explains a missing ANN index in terms of what it costs", () => {
    const [diagnostic] = deriveDiagnostics({
      health: { ...healthy, schema_embed_dims: 2048, configured_embed_dims: 2048, missing_ann_indexes: ["facts", "experiences", "entities"] },
      snapshot: null,
    });

    expect(diagnostic?.id).toBe("ann");
    expect(diagnostic?.title).toBe(
      "Vector search is a sequential scan on facts, experiences and entities",
    );
    expect(diagnostic?.body).toContain("anamnesia migrate --dims 1536");
  });

  it("treats a schema and config width mismatch as fatal", () => {
    const diagnostics = deriveDiagnostics({
      health: { ...healthy, schema_embed_dims: 1536, configured_embed_dims: 3072 },
      snapshot: null,
    });

    const mismatch = diagnostics.find((d) => d.id === "dims-mismatch");
    expect(mismatch?.tone).toBe("bad");
    expect(mismatch?.body).toContain("anamnesia migrate --dims 3072");
  });

  it("leads with the likely cause when the server cannot be reached", () => {
    const offline = deriveDiagnostics({ health: undefined, snapshot: null, connection: "offline" });

    // Docker Desktop reaches a loopback-bound server through
    // host.docker.internal, so "rebind the server" is wrong advice there and
    // must not come first. The server simply not running is the common case.
    expect(offline[0]?.body).toContain("anamnesia status");
    expect(offline[0]?.body.indexOf("anamnesia status")).toBeLessThan(
      offline[0]!.body.indexOf("0.0.0.0:8181"),
    );
  });
});

describe("dormant worker loops", () => {
  const loop = (over: Partial<LoopState> = {}): LoopState => ({
    name: "consolidate",
    interval_ms: 86_400_000,
    running: false,
    last_start: null,
    last_duration_ms: 0,
    last_result: "",
    last_error: "",
    runs: 0,
    failures: 0,
    ...over,
  });

  const snapshotWith = (loops: LoopState[]): ActivitySnapshot => ({
    server: { started_at: "2026-08-18T11:00:00Z", uptime_ms: 7_200_000, version: "v0.1.0-rc5" },
    recorder: { capacity: 200, held: 0, dropped_events: 0 },
    loops,
    queues: { extract_pending: 0, embed_pending: 0 },
    traces: [],
  });

  it("says nothing when a loop runs at its default cadence", () => {
    const found = deriveDiagnostics({ health: undefined, snapshot: snapshotWith([loop()]) });
    expect(found.map((d) => d.id)).not.toContain("dormant-consolidate");
  });

  it("tolerates ordinary tuning without complaining", () => {
    // Three times the default is someone making a choice, not disabling it.
    const found = deriveDiagnostics({
      health: undefined,
      snapshot: snapshotWith([loop({ interval_ms: 3 * 86_400_000 })]),
    });
    expect(found.map((d) => d.id)).not.toContain("dormant-consolidate");
  });

  it("flags a loop turned down to never, and says what stops happening", () => {
    // 9999h, the value found on a real install.
    const found = deriveDiagnostics({
      health: undefined,
      snapshot: snapshotWith([loop({ interval_ms: 35_996_400_000 })]),
    });

    const dormant = found.find((d) => d.id === "dormant-consolidate");
    expect(dormant?.title).toContain("once every 417 days");
    expect(dormant?.body).toContain("worker.consolidate_every");
    expect(dormant?.body).toContain("memory grows without ever getting shorter");
  });

  it("names the right setting for embed, which does not follow the pattern", () => {
    const found = deriveDiagnostics({
      health: undefined,
      snapshot: snapshotWith([loop({ name: "embed", interval_ms: 30 * 86_400_000 })]),
    });

    expect(found.find((d) => d.id === "dormant-embed")?.body).toContain("worker.embed_backfill");
  });
});

describe("the restart notice", () => {
  const snapshotStartedAt = (startedAt: string, traces: number): ActivitySnapshot => ({
    server: { started_at: startedAt, uptime_ms: 0, version: "v0.1.0-rc5" },
    recorder: { capacity: 200, held: 0, dropped_events: 0 },
    loops: [],
    queues: { extract_pending: 0, embed_pending: 0 },
    traces: Array.from({ length: traces }, (_, i) => ({
      id: `t${i}`,
      seq: i,
      kind: "ingest" as const,
      status: "ok" as const,
      user: "default",
      project: "p",
      started_at: startedAt,
      ended_at: startedAt,
      duration_ms: 1,
      summary: "",
      step_count: 0,
    })),
  });

  const minutesAgo = (n: number) => new Date(Date.now() - n * 60_000).toISOString();

  it("appears just after a restart, when history was genuinely lost", () => {
    const found = deriveDiagnostics({
      health: undefined,
      snapshot: snapshotStartedAt(minutesAgo(20), 3),
    });
    expect(found.map((d) => d.id)).toContain("restart");
  });

  it("goes away once the server has been up a while", () => {
    // uptime_ms stays 0 in this fixture: reading it instead of started_at kept
    // this notice on screen indefinitely, which is the bug it guards.
    const found = deriveDiagnostics({
      health: undefined,
      snapshot: snapshotStartedAt(minutesAgo(180), 3),
    });
    expect(found.map((d) => d.id)).not.toContain("restart");
  });

  it("says nothing when there are no traces to have lost", () => {
    const found = deriveDiagnostics({
      health: undefined,
      snapshot: snapshotStartedAt(minutesAgo(5), 0),
    });
    expect(found.map((d) => d.id)).not.toContain("restart");
  });
});
