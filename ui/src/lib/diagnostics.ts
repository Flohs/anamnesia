import type { ActivitySnapshot, ConnectionState, Health, LoopName, LoopState } from "@/api/types";

/**
 * The states that carry this product.
 *
 * A memory service quietly doing nothing looks exactly like one with nothing
 * to do. These derivations tell the two apart, and each says what is true,
 * why, and the command that changes it. Pure so the rules can be tested
 * without rendering anything.
 *
 * Backticks in `body` mark inline code and are rendered as such.
 */

export type DiagnosticTone = "warn" | "bad" | "info";

export interface Diagnostic {
  id: string;
  /** Three or four letters naming the subsystem, shown as the badge. */
  code: string;
  tone: DiagnosticTone;
  title: string;
  body: string;
}

export interface DiagnosticInput {
  health: Health | undefined;
  snapshot: ActivitySnapshot | null;
  connection?: ConnectionState;
}

export function deriveDiagnostics({ health, snapshot, connection }: DiagnosticInput): Diagnostic[] {
  const diagnostics: Diagnostic[] = [];

  if (connection === "offline") {
    diagnostics.push({
      id: "offline",
      code: "NET",
      tone: "bad",
      title: "Cannot reach anamnesia",
      body: "Check that the server is up with `anamnesia status`, and that `ANAMNESIA_URL` points at it. On Linux the container reaches the host over the Docker bridge, which a loopback-bound server refuses: there, set `server.addr` to `0.0.0.0:8181` with a `server.token`. On Docker Desktop, `host.docker.internal` reaches a loopback-bound server already.",
    });
  } else if (connection === "polling") {
    diagnostics.push({
      id: "polling",
      code: "SSE",
      tone: "warn",
      title: "The activity stream dropped, so this is polling every five seconds",
      body: "Traces will appear late and short-lived ones may be missed entirely. Check that nothing between the console and the server buffers `text/event-stream`.",
    });
  }

  if (health) {
    // Nothing else matters if no model is configured: the pipeline runs and
    // produces nothing, which is the single most common cause of an install
    // that looks healthy and never remembers anything.
    if (!health.llm_provider || health.llm_provider === "stub") {
      diagnostics.push({
        id: "llm-stub",
        code: "LLM",
        tone: "bad",
        title: "No model is configured, so nothing will ever be extracted",
        body: "Checkpoints still arrive and still queue, but no operations are produced and memory cannot grow. Set a key with `anamnesia config set openrouter.api_key …`, then `anamnesia restart`.",
      });
    }

    if (!health.embed_provider || health.embed_provider === "stub") {
      diagnostics.push({
        id: "embed-stub",
        code: "VEC",
        tone: "bad",
        title: "Nothing is being embedded, so vector search returns nothing",
        body: "Set `embed.provider` and a key for it. Full-text search still works in the meantime, at lower recall.",
      });
    }

    const missing = health.missing_ann_indexes ?? [];
    if (missing.length > 0) {
      diagnostics.push({
        id: "ann",
        code: "ANN",
        tone: "warn",
        title: `Vector search is a sequential scan on ${listJoin(missing)}`,
        body: `Embeddings are ${health.schema_embed_dims}-wide and pgvector's HNSW index stops at 2000. Retrieval still works, it just reads every row. Lower \`embed.dims\` and run \`anamnesia migrate --dims 1536\` to index them.`,
      });
    }

    if (health.schema_embed_dims !== health.configured_embed_dims) {
      diagnostics.push({
        id: "dims-mismatch",
        code: "DIM",
        tone: "bad",
        title: "The schema and the configured embedding width disagree",
        body: `The tables store vector(${health.schema_embed_dims}) but \`embed.dims\` is ${health.configured_embed_dims}, so every embedding write fails. Run \`anamnesia migrate --dims ${health.configured_embed_dims}\`.`,
      });
    }

    for (const problem of health.problems ?? []) {
      diagnostics.push({ id: `problem-${hash(problem)}`, code: "SRV", tone: "bad", title: problem, body: "" });
    }
  }

  if (snapshot) {
    for (const loop of snapshot.loops) {
      const dormant = dormantLoop(loop);
      if (dormant) diagnostics.push(dormant);
    }

    if (snapshot.recorder.dropped_events > 0) {
      diagnostics.push({
        id: "dropped",
        code: "REC",
        tone: "warn",
        title: `${snapshot.recorder.dropped_events} events were dropped under load`,
        body: "The feed below is incomplete. This happens when traces arrive faster than the console consumes them.",
      });
    }

    // A short uptime with traces held means history was lost, which is worth
    // saying before someone concludes the machine was idle. Computed from
    // started_at, because uptime_ms is measured once at connect and would keep
    // this notice on screen hours after it stopped being true.
    const uptimeMs = Date.now() - new Date(snapshot.server.started_at).getTime();
    if (uptimeMs < 60 * 60 * 1000 && snapshot.traces.length > 0) {
      diagnostics.push({
        id: "restart",
        code: "MEM",
        tone: "info",
        title: "Traces from before the last restart are gone",
        body: "Traces live in memory by design and are never written to Postgres, so this feed begins when the server started. Stored memory is unaffected.",
      });
    }
  }

  return diagnostics;
}

/**
 * Intervals a loop is expected to run at, from the binary's own defaults.
 * A configured value far above its default is not a fault, but it silently
 * changes how memory behaves, and a console that prints "every 9999h" without
 * comment is not doing its job.
 */
const EXPECTED_INTERVAL_MS: Partial<Record<LoopName, number>> = {
  extract: 15_000,
  embed: 60_000,
  forget: 3_600_000,
  decay: 3_600_000,
  consolidate: 86_400_000,
  "purge-sources": 3_600_000,
};

/** What each loop stops doing when it is turned down to never. */
const LOOP_CONSEQUENCE: Partial<Record<LoopName, string>> = {
  extract: "checkpoints will queue without ever being turned into memory",
  embed: "new rows will never get vectors, so vector search will not find them",
  forget: "expired working memory will accumulate",
  decay: "relevance will never be recomputed, so nothing ages",
  consolidate:
    "similar experiences will never be folded into higher-level insights, so memory grows without ever getting shorter",
  "purge-sources": "raw conversation content will outlive its expiry",
};

/**
 * Ten times the default is the threshold: it clears ordinary tuning, and
 * catches the sentinel values people use to disable a loop.
 */
const DORMANT_FACTOR = 10;

function dormantLoop(loop: LoopState): Diagnostic | null {
  const expected = EXPECTED_INTERVAL_MS[loop.name];
  if (expected === undefined || loop.interval_ms < expected * DORMANT_FACTOR) return null;

  const days = loop.interval_ms / 86_400_000;
  const every = days >= 1 ? `${Math.round(days)} days` : `${Math.round(loop.interval_ms / 3_600_000)} hours`;

  return {
    id: `dormant-${loop.name}`,
    code: "CFG",
    tone: "warn",
    title: `The ${loop.name} worker runs once every ${every}, so in practice it does not run`,
    body: `\`worker.${settingFor(loop.name)}\` is set far above its default. Nothing is broken, but ${LOOP_CONSEQUENCE[loop.name] ?? "this loop does no work"}.`,
  };
}

function settingFor(name: LoopName): string {
  return name === "embed" ? "embed_backfill" : `${name.replace("-", "_")}_every`;
}

function listJoin(items: readonly string[]): string {
  if (items.length <= 1) return items[0] ?? "";
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

/** Stable key for a free-text problem, so React does not reorder alerts. */
function hash(value: string): string {
  let result = 0;
  for (let index = 0; index < value.length; index += 1) {
    result = (result * 31 + value.charCodeAt(index)) | 0;
  }
  return Math.abs(result).toString(36);
}
