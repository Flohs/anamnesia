import { DataTable, type Column } from "@/components/ui/DataTable";
import { EmptyState } from "@/components/ui/EmptyState";
import { ScoreBar } from "@/components/ui/ScoreBar";
import { useScopeNames, type ScopeNames } from "@/api/useScopeNames";
import { experienceTitle } from "@/lib/experience";
import { formatRelative } from "@/lib/format";
import type { Experience } from "@/api/types";

const columns = (names: ScopeNames): ReadonlyArray<Column<Experience>> => [
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
  {
    id: "project",
    header: "project",
    numeric: true,
    render: (row) => names.project(row.scope),
  },
  {
    id: "occurred",
    header: "occurred",
    numeric: true,
    render: (row) => formatRelative(row.occurred_at),
  },
];

export function ExperienceTable({ experiences }: { experiences: readonly Experience[] }) {
  const names = useScopeNames();

  return (
    <DataTable
      rows={experiences}
      columns={columns(names)}
      getRowKey={(row) => row.id}
      caption="Stored experiences"
      empty={
        <EmptyState title="No experiences yet">
          An experience is written when a session produces something worth remembering. Until a
          Claude Code session has run and been checkpointed, there is nothing here.
        </EmptyState>
      }
    />
  );
}

