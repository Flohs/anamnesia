/* eslint-disable react-refresh/only-export-components --
   This file exports domain definitions, not components. They contain JSX
   because a column renderer and an empty state are part of what defines a
   domain, and splitting them out would scatter one definition across two
   files to satisfy a fast-refresh heuristic. Editing this file costs a full
   reload, which is the correct trade for keeping the definitions together. */
import { Chip } from "@/components/ui/Chip";
import { type Column } from "@/components/ui/DataTable";
import { EmptyState } from "@/components/ui/EmptyState";
import { ScoreBar } from "@/components/ui/ScoreBar";
import { type ScopeNames } from "@/api/useScopeNames";
import { experienceTitle } from "@/lib/experience";
import { factValue } from "@/lib/fact";
import { formatBytes, formatRelative } from "@/lib/format";
import { sourceStateTone } from "@/lib/tone";
import type {
  Edge,
  Entity,
  Experience,
  Fact,
  Scope,
  Skill,
  Source,
  WorkingEntry,
} from "@/api/types";

/**
 * One definition per memory domain: what to call it, where it comes from, what
 * its columns are, and what to say when it is empty.
 *
 * Kept together because the Memory screen's whole job is to present these
 * side by side, and a reader comparing two domains should be able to compare
 * their definitions without opening two files.
 */
export interface DomainDefinition<T> {
  id: string;
  label: string;
  /** The browse route, minus the /v1/ prefix. */
  route: string;
  columns: (names: ScopeNames) => ReadonlyArray<Column<T>>;
  rowKey: (row: T) => string;
  empty: React.ReactNode;
}

const project = <T extends { scope: Scope }>(names: ScopeNames): Column<T> => ({
  id: "project",
  header: "project",
  numeric: true,
  render: (row) => names.project(row.scope),
});

// ─── facts ───────────────────────────────────────────────────────────

export const FACTS: DomainDefinition<Fact> = {
  id: "facts",
  label: "Facts",
  route: "facts",
  rowKey: (row) => row.id,
  columns: (names) => [
    {
      id: "key",
      header: "key",
      render: (row) => <span className="font-mono text-[12.5px] text-text">{row.key}</span>,
    },
    { id: "value", header: "value", render: (row) => factValue(row.value) },
    { id: "scope", header: "scope", numeric: true, render: (row) => row.fact_scope },
    { id: "trust", header: "trust", render: (row) => <ScoreBar score={row.trust} tone="ok" /> },
    project(names),
    {
      id: "learned",
      header: "learned",
      numeric: true,
      render: (row) => formatRelative(row.ingested_at),
    },
  ],
  empty: (
    <EmptyState title="No facts yet">
      A fact is a claim worth restating: a preference you stated, a decision you settled, a piece
      of project configuration. Most checkpoints are skipped as unsurprising, which is the gate
      working rather than a fault.
    </EmptyState>
  ),
};

// ─── experiences ─────────────────────────────────────────────────────

export const EXPERIENCES: DomainDefinition<Experience> = {
  id: "experiences",
  label: "Experiences",
  route: "experiences",
  rowKey: (row) => row.id,
  columns: (names) => [
    {
      id: "title",
      header: "title",
      render: (row) => <span className="font-medium text-text">{experienceTitle(row)}</span>,
    },
    { id: "kind", header: "kind", numeric: true, render: (row) => row.kind },
    {
      id: "abstraction",
      header: "abstraction",
      numeric: true,
      render: (row) => (row.abstraction === 0 ? "raw" : `level ${row.abstraction}`),
    },
    {
      id: "relevance",
      header: "relevance",
      render: (row) => <ScoreBar score={row.relevance} tone="run" />,
    },
    project(names),
    {
      id: "occurred",
      header: "occurred",
      numeric: true,
      render: (row) => formatRelative(row.occurred_at),
    },
  ],
  empty: (
    <EmptyState title="No experiences yet">
      An experience is something that happened, kept as a short narrative. Until a Claude Code
      session has run and been checkpointed, there is nothing here.
    </EmptyState>
  ),
};

// ─── skills ──────────────────────────────────────────────────────────

export const SKILLS: DomainDefinition<Skill> = {
  id: "skills",
  label: "Skills",
  route: "skills",
  rowKey: (row) => row.id,
  columns: (names) => [
    {
      id: "name",
      header: "name",
      render: (row) => <span className="font-medium text-text">{row.name}</span>,
    },
    { id: "kind", header: "kind", numeric: true, render: (row) => row.kind },
    { id: "description", header: "what it does", render: (row) => row.description ?? "" },
    { id: "uses", header: "used", numeric: true, align: "right", render: (row) => row.use_count },
    project(names),
    {
      id: "last",
      header: "last used",
      numeric: true,
      render: (row) => (row.last_used_at ? formatRelative(row.last_used_at) : "never"),
    },
  ],
  empty: (
    <EmptyState title="No skills registered">
      A skill is something callable that Claude can look up later: a function, a script, an API,
      an MCP tool. They are registered deliberately through the MCP surface rather than extracted
      from conversation, so this stays empty until something registers one.
    </EmptyState>
  ),
};

// ─── entities and edges ──────────────────────────────────────────────

export const ENTITIES: DomainDefinition<Entity> = {
  id: "entities",
  label: "Entities",
  route: "entities",
  rowKey: (row) => row.id,
  columns: (names) => [
    {
      id: "name",
      header: "name",
      render: (row) => <span className="font-medium text-text">{row.name}</span>,
    },
    { id: "kind", header: "kind", render: (row) => <Chip>{row.kind}</Chip> },
    project(names),
    {
      id: "created",
      header: "first seen",
      numeric: true,
      render: (row) => formatRelative(row.created_at),
    },
  ],
  empty: (
    <EmptyState title="No entities yet">
      Entities are the nodes of the memory graph: people, projects, tools, concepts. They support
      multi-hop questions, and nothing writes them yet on this install.
    </EmptyState>
  ),
};

export const EDGES: DomainDefinition<Edge> = {
  id: "edges",
  label: "Edges",
  route: "edges",
  rowKey: (row) => row.id,
  columns: () => [
    { id: "kind", header: "relation", render: (row) => <Chip>{row.kind}</Chip> },
    { id: "from", header: "from", numeric: true, render: (row) => row.from_id.slice(0, 8) },
    { id: "to", header: "to", numeric: true, render: (row) => row.to_id.slice(0, 8) },
    { id: "trust", header: "trust", render: (row) => <ScoreBar score={row.trust} tone="ok" /> },
    {
      id: "valid",
      header: "valid from",
      numeric: true,
      render: (row) => formatRelative(row.valid_from),
    },
  ],
  empty: (
    <EmptyState title="No edges yet">
      An edge is a typed, time-bounded relation between two entities. There are no entities to
      relate yet.
    </EmptyState>
  ),
};

// ─── sources ─────────────────────────────────────────────────────────

export const SOURCES: DomainDefinition<Source> = {
  id: "sources",
  label: "Sources",
  route: "sources",
  rowKey: (row) => row.id,
  columns: (names) => [
    { id: "kind", header: "kind", numeric: true, render: (row) => row.kind },
    {
      id: "state",
      header: "extraction",
      render: (row) => (
        <Chip tone={sourceStateTone(row.extraction_state)} withDot>
          {row.extraction_state}
        </Chip>
      ),
    },
    {
      id: "ops",
      header: "operations",
      numeric: true,
      align: "right",
      render: (row) => (
        <span className={row.ops_produced === 0 ? "text-text-5" : undefined}>
          {row.ops_produced}
        </span>
      ),
    },
    {
      id: "size",
      header: "size",
      numeric: true,
      align: "right",
      // Nulled out once expired, which is the mechanism working, not a gap.
      render: (row) =>
        row.raw_content ? formatBytes(row.raw_content.length) : <span className="text-text-5">expired</span>,
    },
    project(names),
    {
      id: "ingested",
      header: "arrived",
      numeric: true,
      render: (row) => formatRelative(row.ingested_at),
    },
  ],
  empty: (
    <EmptyState title="No sources yet">
      A source is raw material handed to the extractor, usually a session checkpoint. Its content
      is discarded after seven days whether or not anything was learned from it.
    </EmptyState>
  ),
};

// ─── working memory ──────────────────────────────────────────────────

export const WORKING: DomainDefinition<WorkingEntry> = {
  id: "working",
  label: "Working",
  route: "working",
  rowKey: (row) => row.id,
  columns: (names) => [
    { id: "role", header: "role", numeric: true, render: (row) => row.role },
    { id: "body", header: "entry", render: (row) => row.body },
    {
      id: "session",
      header: "session",
      numeric: true,
      render: (row) => row.session_id.slice(0, 8),
    },
    project(names),
    {
      id: "expires",
      header: "expires",
      numeric: true,
      render: (row) => formatRelative(row.expires_at),
    },
  ],
  empty: (
    <EmptyState title="Working memory is empty">
      These are in-session notes with a short time to live, folded into an experience at session
      end or discarded. Empty is the normal state between sessions.
    </EmptyState>
  ),
};

export const DOMAINS = [
  EXPERIENCES,
  FACTS,
  SOURCES,
  SKILLS,
  ENTITIES,
  EDGES,
  WORKING,
] as const;
