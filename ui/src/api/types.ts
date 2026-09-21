/**
 * The read API exposed by `anamnesia serve`.
 *
 * These types are the contract from
 * docs/superpowers/specs/2026-08-18-binary-read-api-spec.md, section 6. They
 * are the single definition of a server shape in this project: nothing else
 * describes a response body. When the binary's contract changes, this file
 * changes first and TypeScript points at every consequence.
 */

// ─── shared ──────────────────────────────────────────────────────────

/** RFC3339 in UTC, as every timestamp in the contract is. */
export type Timestamp = string;

export type Uuid = string;

/**
 * How the server identifies a row's owner: UUIDs, not names. Turning these
 * into the slugs a person reads is the job of useScopeNames().
 */
export interface Scope {
  user_id: Uuid;
  /** Omitted, not nulled, on a row that belongs to the user rather than a project. */
  project_id?: Uuid | null;
}

/** Query-string values a browse endpoint accepts. */
export type QueryParams = Record<string, string | number | boolean | undefined>;

export interface Page<T> {
  items: T[];
  next_cursor: string | null;
}

// ─── health ──────────────────────────────────────────────────────────

export interface Health {
  ok: boolean;
  service: string;
  version?: string;
  database: string;
  migration_version: number;
  schema_embed_dims: number;
  configured_embed_dims: number;
  embed_provider?: string;
  embed_model?: string;
  llm_provider?: string;
  llm_model?: string;
  missing_ann_indexes?: string[];
  problems?: string[];
}

// ─── activity ────────────────────────────────────────────────────────

/** Worker loops are a closed set: the six the binary runs. */
export type LoopName =
  | "extract"
  | "embed"
  | "decay"
  | "consolidate"
  | "forget"
  | "purge-sources";

export interface LoopState {
  name: LoopName;
  interval_ms: number;
  running: boolean;
  last_start: Timestamp | null;
  last_duration_ms: number;
  last_result: string;
  last_error: string;
  runs: number;
  failures: number;
}

export type TraceKind = "ingest" | "retrieve" | "consolidate" | "session-start";

export type TraceStatus = "running" | "ok" | "skipped" | "failed";

/**
 * Step names are open rather than a union: the binary may add a step to a
 * pipeline without the console needing a release. Known names get a bespoke
 * detail renderer, unknown ones fall back to a generic key/value view.
 */
export type StepName = string;

/**
 * The minimum a step needs for the trace strip: what it was and how long it
 * took. Summaries carry these so the feed can draw a strip per trace without
 * fetching every trace in full.
 */
export interface StepTiming {
  name: StepName;
  duration_ms: number;
  /** Only on full traces. Lets a segment label the model or verdict it used. */
  detail?: Record<string, unknown>;
}

export interface TraceStep extends StepTiming {
  at: Timestamp;
  summary: string;
  /** Only present on a step that failed. */
  err?: string;
  detail: Record<string, unknown>;
}

export interface TraceSummary {
  id: Uuid;
  seq: number;
  kind: TraceKind;
  status: TraceStatus;
  user: string;
  project: string;
  started_at: Timestamp;
  ended_at: Timestamp | null;
  duration_ms: number;
  summary: string;
  step_count: number;
  /**
   * Names and durations per step, enough to draw the strip without fetching
   * the trace. Sent by rc5 and later. Absent when a trace has no steps yet,
   * since the server omits an empty array.
   */
  step_timings?: StepTiming[];
}

export interface Trace extends TraceSummary {
  steps: TraceStep[];
}

export interface ServerInfo {
  started_at: Timestamp;
  uptime_ms: number;
  version: string;
}

export interface RecorderInfo {
  capacity: number;
  held: number;
  dropped_events: number;
}

export interface QueueDepths {
  extract_pending: number;
  embed_pending: number;
}

export interface ActivitySnapshot {
  server: ServerInfo;
  recorder: RecorderInfo;
  loops: LoopState[];
  queues: QueueDepths;
  traces: TraceSummary[];
}

// ─── activity stream ─────────────────────────────────────────────────

/**
 * Server-sent events. The first frame is a full snapshot, so one connection
 * renders the whole screen with no companion request.
 */
export type ActivityEvent =
  | { type: "snapshot"; data: ActivitySnapshot }
  | { type: "trace"; data: TraceSummary }
  | { type: "step"; data: { trace_id: Uuid; step: TraceStep } }
  | { type: "loops"; data: LoopState[] }
  | { type: "queues"; data: QueueDepths };

export type ConnectionState = "connecting" | "streaming" | "polling" | "offline";

// ─── hooks ───────────────────────────────────────────────────────────

export interface HookRun {
  at: Timestamp;
  verb: string;
  ok: boolean;
  ms: number;
  note: string;
}

export interface HookRuns {
  path: string;
  items: HookRun[];
}

// ─── stats ───────────────────────────────────────────────────────────

export interface DomainTotals {
  users: number;
  projects: number;
  facts: number;
  experiences: number;
  skills: number;
  working_memory: number;
  entities: number;
  edges: number;
  sources: number;
  commitments: number;
  artifacts: number;
}

export type SourceState = "pending" | "done" | "failed" | "skipped";

export interface EmbeddingCoverage {
  total: number;
  embedded: number;
}

export interface Stats {
  scope: { user: string; project: string | null };
  totals: DomainTotals;
  sources_by_state: Record<SourceState, number>;
  queues: QueueDepths;
  experiences_by_abstraction: Record<string, number>;
  embedding_coverage: Record<string, EmbeddingCoverage>;
}

export interface ActivityBucket {
  date: string;
  /** Null for a source ingested outside any project. */
  project: string | null;
  sources: number;
  facts: number;
  experiences: number;
}

export interface ActivityBuckets {
  buckets: ActivityBucket[];
}

// ─── scope ───────────────────────────────────────────────────────────

export interface ProjectCounts {
  facts: number;
  experiences: number;
  skills: number;
  entities: number;
  sources: number;
}

export interface Project {
  id: Uuid;
  slug: string;
  user: string;
  created_at: Timestamp;
  last_activity: Timestamp | null;
  counts: ProjectCounts;
}

export interface User {
  id: Uuid;
  handle: string;
  created_at: Timestamp;
  last_activity: Timestamp | null;
  projects: number;
  counts: ProjectCounts;
}

// ─── memory domains ──────────────────────────────────────────────────

export type FactScope = "user" | "project" | "environment";

export interface Fact {
  id: Uuid;
  scope: Scope;
  key: string;
  value: Record<string, unknown>;
  fact_scope: FactScope;
  source?: string;
  source_id?: Uuid;
  trust: number;
  pii_tags?: string[];
  valid_from: Timestamp;
  valid_to?: Timestamp;
  ingested_at: Timestamp;
  superseded_by?: Uuid;
}

export type ExperienceKind = "case" | "strategy" | "hybrid";

export interface Experience {
  id: Uuid;
  scope: Scope;
  kind: ExperienceKind;
  abstraction: number;
  /** Often absent: the extractor writes a body without always naming it. */
  title?: string;
  body: string;
  last_used_at?: Timestamp;
  meta?: Record<string, unknown>;
  outcome?: string;
  trust: number;
  importance: number;
  relevance: number;
  use_count: number;
  occurred_at: Timestamp;
  topic?: string;
  participants?: string[];
  parent_id?: Uuid;
  source_id?: Uuid;
  ingested_at: Timestamp;
}

export type SkillKind = "function" | "script" | "api" | "mcp";

export interface Skill {
  id: Uuid;
  scope: Scope;
  name: string;
  kind: SkillKind;
  description?: string;
  use_count: number;
  last_used_at?: Timestamp;
  created_at: Timestamp;
}

// ─── artifacts ───────────────────────────────────────────────────────

export interface Artifact {
  id: Uuid;
  scope: Scope;
  /** Often absent: the capture hook records a description far more reliably. */
  title?: string;
  description?: string;
  url?: string;
  /** The slug, already resolved by the server rather than a project_id. */
  project?: string;
  created_at: Timestamp;
}

/** Artifacts answer with their own envelope rather than the browse `Page`. */
export interface Artifacts {
  scope: { user: string; project: string | null };
  artifacts: Artifact[];
}

export interface Source {
  id: Uuid;
  scope: Scope;
  kind: string;
  title?: string;
  occurred_at: Timestamp;
  ingested_at: Timestamp;
  extraction_state: SourceState;
  extracted_at?: Timestamp;
  extraction_error?: string;
  ops_produced: number;
  expires_at: Timestamp;
  preserve_raw?: boolean;
  metadata?: Record<string, unknown>;
  /**
   * The whole conversation, tens of kilobytes per row, nulled out once
   * `expires_at` passes. Never render it wholesale.
   */
  raw_content?: string;
}

export interface Entity {
  id: Uuid;
  scope: Scope;
  kind: string;
  name: string;
  props?: Record<string, unknown>;
  created_at: Timestamp;
}

export interface Edge {
  id: Uuid;
  from_id: Uuid;
  to_id: Uuid;
  kind: string;
  props?: Record<string, unknown>;
  trust: number;
  source?: string;
  valid_from: Timestamp;
  valid_to?: Timestamp;
  invalidated_at?: Timestamp;
}

export type WorkingRole = "observation" | "plan" | "state" | "tool_output";

export interface WorkingEntry {
  id: Uuid;
  scope: Scope;
  session_id: Uuid;
  position: number;
  role: WorkingRole;
  body: string;
  folded_into?: Uuid;
  expires_at: Timestamp;
  created_at: Timestamp;
}

// ─── embedding map ───────────────────────────────────────────────────

export interface EmbeddingPoint {
  id: Uuid;
  title: string;
  project: string;
  kind: string;
  x: number;
  y: number;
}

export interface EmbeddingMap {
  domain: "experiences" | "facts";
  n: number;
  dims: number;
  explained_variance: [number, number];
  points: EmbeddingPoint[];
}

// ─── config ──────────────────────────────────────────────────────────

export interface ConfigEntry {
  key: string;
  value: string;
  source: string;
  secret: boolean;
}

export interface Config {
  items: ConfigEntry[];
}
